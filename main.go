package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/joho/godotenv"
)

// Config holds runtime settings loaded from the environment / .env file.
type Config struct {
	DiscordToken string
	ChannelRU    string
	ChannelINT   string
	GeminiKey    string
	GeminiModel  string
	DeepLKey     string
	Translators  []string // order of preference
	DataDir      string
}

func loadConfig() Config {
	_ = godotenv.Load()
	cfg := Config{
		DiscordToken: os.Getenv("DISCORD_TOKEN"),
		ChannelRU:    os.Getenv("CHANNEL_RU"),
		ChannelINT:   os.Getenv("CHANNEL_INT"),
		GeminiKey:    os.Getenv("GEMINI_API_KEY"),
		GeminiModel:  envOr("GEMINI_MODEL", "gemini-3.1-flash-lite"),
		DeepLKey:     os.Getenv("DEEPL_API_KEY"),
		DataDir:      envOr("DATA_DIR", "data"),
		Translators:  strings.Split(strings.ReplaceAll(envOr("TRANSLATORS", "deepl,gemini"), " ", ""), ","),
	}
	if cfg.DiscordToken == "" || cfg.ChannelRU == "" || cfg.ChannelINT == "" {
		log.Fatal("config: DISCORD_TOKEN, CHANNEL_RU and CHANNEL_INT are required")
	}
	if cfg.GeminiKey == "" && cfg.DeepLKey == "" {
		log.Fatal("config: set at least one of GEMINI_API_KEY or DEEPL_API_KEY")
	}
	return cfg
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	cfg := loadConfig()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Translators in order of preference (TRANSLATORS, default "deepl,gemini"):
	// DeepL is fast and reliable; Gemini is better with slang but slow on the free tier.
	var chain []Translator
	for _, name := range cfg.Translators {
		switch name {
		case "deepl":
			if cfg.DeepLKey != "" {
				chain = append(chain, NewDeepL(cfg.DeepLKey))
			}
		case "gemini":
			if cfg.GeminiKey != "" {
				g, err := NewGemini(ctx, cfg.GeminiKey, cfg.GeminiModel)
				if err != nil {
					log.Fatalf("gemini: %v", err)
				}
				chain = append(chain, g)
			}
		default:
			log.Fatalf("config: unknown translator %q in TRANSLATORS", name)
		}
	}
	if len(chain) == 0 {
		log.Fatal("config: no translator is configured (check TRANSLATORS and API keys)")
	}
	translator := NewFallback(chain...)

	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		log.Fatalf("data dir: %v", err)
	}
	store, err := OpenStore(filepath.Join(cfg.DataDir, "messages.json"))
	if err != nil {
		log.Fatalf("store: %v", err)
	}

	dg, err := discordgo.New("Bot " + cfg.DiscordToken)
	if err != nil {
		log.Fatalf("discord: %v", err)
	}
	// MessageContent is a privileged intent: it must also be enabled in the
	// Developer Portal (Bot -> Privileged Gateway Intents), otherwise message text arrives empty.
	dg.Identify.Intents = discordgo.IntentsGuildMessages | discordgo.IntentsGuildWebhooks | discordgo.IntentMessageContent

	bridge := NewBridge(dg, translator, store, map[string]Route{
		cfg.ChannelRU:  {Target: cfg.ChannelINT, Lang: LangEN},
		cfg.ChannelINT: {Target: cfg.ChannelRU, Lang: LangRU},
	})
	bridge.Register()

	if err := dg.Open(); err != nil {
		log.Fatalf("discord connect: %v", err)
	}
	log.Printf("bridge is live (translators: %s)", translator.Name())

	// Persist the message map periodically.
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := store.Flush(); err != nil {
					log.Printf("store flush: %v", err)
				}
			}
		}
	}()

	<-ctx.Done()
	log.Println("shutting down...")
	bridge.Close()
	_ = dg.Close()
	if err := store.Flush(); err != nil {
		log.Printf("store flush: %v", err)
	}
}
