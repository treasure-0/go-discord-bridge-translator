package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
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

	Translators  []string // order of preference
	GroqKey      string
	GroqModel    string
	AzureKey     string
	AzureRegion  string
	AzureLimit   int64 // free monthly characters
	DeepLKey     string
	GeminiKey    string
	GeminiModel  string
	QuotaWarnPct int64

	AlertUserID    string
	AlertChannelID string
	ReportTime     string // daily report, "09:00"
	ReportTZ       string
	DataDir        string
}

func loadConfig() Config {
	_ = godotenv.Load()
	cfg := Config{
		DiscordToken:   os.Getenv("DISCORD_TOKEN"),
		ChannelRU:      os.Getenv("CHANNEL_RU"),
		ChannelINT:     os.Getenv("CHANNEL_INT"),
		Translators:    strings.Split(strings.ReplaceAll(envOr("TRANSLATORS", "groq,azure,deepl,gemini"), " ", ""), ","),
		GroqKey:        os.Getenv("GROQ_API_KEY"),
		GroqModel:      envOr("GROQ_MODEL", "qwen/qwen3.8-27b"),
		AzureKey:       os.Getenv("AZURE_TRANSLATOR_KEY"),
		AzureRegion:    envOr("AZURE_TRANSLATOR_REGION", "global"),
		AzureLimit:     envInt("AZURE_MONTHLY_LIMIT", 2_000_000),
		DeepLKey:       os.Getenv("DEEPL_API_KEY"),
		GeminiKey:      os.Getenv("GEMINI_API_KEY"),
		GeminiModel:    envOr("GEMINI_MODEL", "gemini-3.1-flash-lite"),
		QuotaWarnPct:   envInt("QUOTA_ALERT_PERCENT", 15),
		AlertUserID:    os.Getenv("ALERT_USER_ID"),
		AlertChannelID: os.Getenv("ALERT_CHANNEL_ID"),
		ReportTime:     envOr("REPORT_TIME", "09:00"),
		ReportTZ:       envOr("REPORT_TZ", "Europe/Berlin"),
		DataDir:        envOr("DATA_DIR", "data"),
	}
	if cfg.DiscordToken == "" || cfg.ChannelRU == "" || cfg.ChannelINT == "" {
		log.Fatal("config: DISCORD_TOKEN, CHANNEL_RU and CHANNEL_INT are required")
	}
	return cfg
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int64) int64 {
	if v := os.Getenv(key); v != "" {
		n, err := strconv.ParseInt(strings.ReplaceAll(v, "_", ""), 10, 64)
		if err != nil {
			log.Fatalf("config: %s must be a number", key)
		}
		return n
	}
	return def
}

func main() {
	cfg := loadConfig()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		log.Fatalf("data dir: %v", err)
	}
	store, err := OpenStore(filepath.Join(cfg.DataDir, "messages.json"))
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	ops, err = OpenOps(filepath.Join(cfg.DataDir, "ops.json"))
	if err != nil {
		log.Fatalf("ops journal: %v", err)
	}
	crashed := ops.Start(time.Now(), serverBootTime())
	_ = ops.Flush()

	dg, err := discordgo.New("Bot " + cfg.DiscordToken)
	if err != nil {
		log.Fatalf("discord: %v", err)
	}
	// MessageContent is a privileged intent: it must also be enabled in the
	// Developer Portal (Bot -> Privileged Gateway Intents), otherwise message text arrives empty.
	dg.Identify.Intents = discordgo.IntentsGuildMessages | discordgo.IntentsGuildWebhooks | discordgo.IntentMessageContent
	notifier := NewNotifier(dg, cfg.AlertUserID, cfg.AlertChannelID)

	// Translators in order of preference (TRANSLATORS, default "groq,azure,deepl,gemini"):
	// an LLM first for slang and tone, machine translation as fast fallbacks.
	// Providers without an API key are skipped.
	var chain []Translator
	var quotas []QuotaReporter
	var names []string
	for _, name := range cfg.Translators {
		switch name {
		case "groq":
			if cfg.GroqKey != "" {
				chain = append(chain, NewGroq(cfg.GroqKey, cfg.GroqModel))
			}
		case "azure":
			if cfg.AzureKey != "" {
				a := NewAzure(cfg.AzureKey, cfg.AzureRegion, cfg.AzureLimit)
				chain = append(chain, a)
				quotas = append(quotas, a)
			}
		case "deepl":
			if cfg.DeepLKey != "" {
				d := NewDeepL(cfg.DeepLKey)
				chain = append(chain, d)
				quotas = append(quotas, d)
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
	for _, t := range chain {
		names = append(names, t.Name())
	}
	if len(chain) == 0 {
		log.Fatal("config: no translator is configured (set an API key for at least one of TRANSLATORS)")
	}
	translator := NewFallback(chain...)
	translator.OnLimit = func(provider string, cd time.Duration, err error) {
		notifier.Alert("limit-"+provider, time.Hour, fmt.Sprintf(
			"⚠️ Переводчик **%s** временно недоступен (%v). Бот на %s переключился на запасной.",
			provider, err, cd.Round(time.Second)))
	}

	bridge := NewBridge(dg, translator, store, map[string]Route{
		cfg.ChannelRU:  {Target: cfg.ChannelINT, Lang: LangEN, Local: LangRU},
		cfg.ChannelINT: {Target: cfg.ChannelRU, Lang: LangRU, Local: LangEN},
	})
	bridge.notifier = notifier
	bridge.Register()
	// Routine reconnects are only counted; outages over a minute are journaled and alerted.
	NewConnMonitor(notifier, time.Minute).Register(dg)
	// Library warnings (gateway close codes, failed heartbeats) go to the journal for diagnosis.
	dg.LogLevel = discordgo.LogWarning

	if err := dg.Open(); err != nil {
		log.Fatalf("discord connect: %v", err)
	}
	log.Printf("bridge is live (translators: %s)", translator.Name())
	if crashed {
		notifier.Alert("startup", 10*time.Minute, "⚠️ Бот восстановился после аварийной остановки. Переводчики: "+translator.Name())
	} else {
		notifier.Alert("startup", 10*time.Minute, "✅ Бот-переводчик запущен. Переводчики по порядку: "+translator.Name())
	}

	go notifier.WatchQuotas(ctx, quotas, cfg.QuotaWarnPct, 30*time.Minute)
	go WatchServer(ctx)
	if notifier.Enabled() {
		rep, err := NewReporter(notifier, ops, quotas, names, cfg.ReportTZ, cfg.ReportTime)
		if err != nil {
			log.Fatalf("config: %v", err)
		}
		go rep.Run(ctx)
	}

	// Persist state periodically.
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
				ops.Heartbeat(time.Now())
				if err := ops.Flush(); err != nil {
					log.Printf("ops flush: %v", err)
				}
			}
		}
	}()

	<-ctx.Done()
	log.Println("shutting down...")
	bridge.Close()
	_ = dg.Close()
	_ = store.Flush()
	ops.Stop()
	_ = ops.Flush()
}
