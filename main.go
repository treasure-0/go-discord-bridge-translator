package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/bwmarrin/discordgo"
	"github.com/joho/godotenv"
	"google.golang.org/genai"
)

// Bot represents the core structure for the AI-powered Discord translator.
type Bot struct {
	client     *genai.Client
	modelName  string
	safety     []*genai.SafetySetting
	channelRU  string
	channelINT string
}

func main() {
	// Load environment variables from .env file.
	_ = godotenv.Load()

	discordToken := os.Getenv("DISCORD_TOKEN")
	geminiAPIKey := os.Getenv("GEMINI_API_KEY")
	channelRU := os.Getenv("CHANNEL_RU")
	channelINT := os.Getenv("CHANNEL_INT")

	// Verify that all required environment variables are present.
	if discordToken == "" || geminiAPIKey == "" || channelRU == "" || channelINT == "" {
		log.Fatal("Critical Error: Missing required tokens or channel IDs in .env file")
	}

	// Set up signal handling for graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Initialize the Google Gemini AI client.
	genClient, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  geminiAPIKey,
		Backend: genai.BackendGeminiAPI,
		HTTPOptions: genai.HTTPOptions{
			APIVersion: "v1beta",
		},
	})
	if err != nil {
		log.Fatalf("Gemini client initialization failed: %v", err)
	}

	// Configure safety settings to allow a broad range of translations (no strict filtering).
	safety := []*genai.SafetySetting{
		{Category: genai.HarmCategoryDangerousContent, Threshold: genai.HarmBlockThresholdBlockNone},
		{Category: genai.HarmCategoryHarassment, Threshold: genai.HarmBlockThresholdBlockNone},
		{Category: genai.HarmCategoryHateSpeech, Threshold: genai.HarmBlockThresholdBlockNone},
		{Category: genai.HarmCategorySexuallyExplicit, Threshold: genai.HarmBlockThresholdBlockNone},
	}

	bot := &Bot{
		client:     genClient,
		modelName:  "gemini-3.1-flash-lite-preview",
		safety:     safety,
		channelRU:  channelRU,
		channelINT: channelINT,
	}

	// Create a new Discord session.
	dg, err := discordgo.New("Bot " + discordToken)
	if err != nil {
		log.Fatalf("Discord session initialization failed: %v", err)
	}

	// Register message event handler.
	dg.AddHandler(bot.handleMessageCreate)

	// Set required intents: GuildMessages to read messages and GuildWebhooks to mirror identities.
	dg.Identify.Intents = discordgo.IntentsGuildMessages | discordgo.IntentsGuildWebhooks

	// Establish a connection to Discord.
	if err := dg.Open(); err != nil {
		log.Fatalf("Failed to open Discord connection: %v", err)
	}
	log.Println("Bridge Translator is live! Webhook mirroring enabled.")

	// Keep the application running until a termination signal is received.
	<-ctx.Done()
	dg.Close()
}

// handleMessageCreate processes incoming messages and sends translations to the target channel.
func (b *Bot) handleMessageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	// Ignore bot messages and webhooks to prevent infinite loops.
	if m.Author == nil || m.Author.Bot || m.WebhookID != "" {
		return
	}

	// Determine the target channel for the translation bridge.
	var targetChannelID string
	switch m.ChannelID {
	case b.channelRU:
		targetChannelID = b.channelINT
	case b.channelINT:
		targetChannelID = b.channelRU
	default:
		return
	}

	// Prepare the AI prompt with explicit language routing.
	// We tell Gemini to detect the source and translate to the opposite language.
	prompt := "You are a professional translator between Russian and English. " +
		"If the text is in Russian, translate it to English. If it is in English, translate to Russian. " +
		"Preserve the original tone and style (including profanity or slang). " +
		"Return ONLY the translated text.\n\nMessage:\n" + m.Content
	
	// Request translation from the Gemini model.
	resp, err := b.client.Models.GenerateContent(context.Background(), b.modelName, genai.Text(prompt), &genai.GenerateContentConfig{
		SafetySettings: b.safety,
	})
	if err != nil {
		log.Printf("Gemini Error: %v", err)
		return
	}

	// Validate the AI response.
	if len(resp.Candidates) == 0 || len(resp.Candidates[0].Content.Parts) == 0 {
		return
	}

	translated := ""
	for _, part := range resp.Candidates[0].Content.Parts {
		translated += part.Text
	}

	if translated == "" {
		return
	}

	// --- WEBHOOK LOGIC: Identity Mirroring ---
	
	// Fetch existing webhooks in the target channel to find a reusable one.
	webhooks, err := s.ChannelWebhooks(targetChannelID)
	if err != nil {
		log.Printf("Failed to retrieve webhooks: %v", err)
		return
	}

	var targetWebhook *discordgo.Webhook
	for _, w := range webhooks {
		if w.Name == "Bridge-Translator" {
			targetWebhook = w
			break
		}
	}

	// Create a new webhook if one doesn't exist.
	if targetWebhook == nil {
		targetWebhook, err = s.WebhookCreate(targetChannelID, "Bridge-Translator", "")
		if err != nil {
			log.Printf("Failed to create webhook: %v", err)
			return
		}
	}

	// Execute the webhook to send the translation while mimicking the original sender's profile.
	_, err = s.WebhookExecute(targetWebhook.ID, targetWebhook.Token, false, &discordgo.WebhookParams{
		Content:   translated,
		Username:  m.Author.Username,
		AvatarURL: m.Author.AvatarURL("128"),
	})

	if err != nil {
		log.Printf("Webhook execution error: %v", err)
	}
}
