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

type Bot struct {
	client     *genai.Client
	modelName  string
	safety     []*genai.SafetySetting
	channelRU  string
	channelINT string
}

func main() {
	_ = godotenv.Load()

	discordToken := os.Getenv("DISCORD_TOKEN")
	geminiAPIKey := os.Getenv("GEMINI_API_KEY")
	channelRU := os.Getenv("CHANNEL_RU")
	channelINT := os.Getenv("CHANNEL_INT")

	if discordToken == "" || geminiAPIKey == "" || channelRU == "" || channelINT == "" {
		log.Fatal("Ошибка: Проверь файл .env, отсутствуют токены или ID каналов")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Инициализация Gemini
	genClient, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey: geminiAPIKey,
		Backend: genai.BackendGeminiAPI,
		HTTPOptions: genai.HTTPOptions{
			APIVersion: "v1beta",
		},
	})
	if err != nil {
		log.Fatalf("Ошибка клиента Gemini: %v", err)
	}

	safety := []*genai.SafetySetting{
		{Category: genai.HarmCategoryDangerousContent, Threshold: genai.HarmBlockThresholdBlockNone},
		{Category: genai.HarmCategoryHarassment, Threshold: genai.HarmBlockThresholdBlockNone},
		{Category: genai.HarmCategoryHateSpeech, Threshold: genai.HarmBlockThresholdBlockNone},
		{Category: genai.HarmCategorySexuallyExplicit, Threshold: genai.HarmBlockThresholdBlockNone},
	}

	bot := &Bot{
		client:     genClient,
		modelName:  "gemini-2.0-flash", // Оставляем рабочую версию
		safety:     safety,
		channelRU:  channelRU,
		channelINT: channelINT,
	}

	dg, err := discordgo.New("Bot " + discordToken)
	if err != nil {
		log.Fatalf("Ошибка сессии Discord: %v", err)
	}

	dg.AddHandler(bot.handleMessageCreate)

	// Нам нужны права на управление вебхуками
	dg.Identify.Intents = discordgo.IntentsGuildMessages | discordgo.IntentsGuildWebhooks

	if err := dg.Open(); err != nil {
		log.Fatalf("Не удалось открыть соединение: %v", err)
	}
	log.Println("🚀 Бот запущен через Вебхуки! Руслан, теперь всё будет красиво.")

	<-ctx.Done()
	dg.Close()
}

func (b *Bot) handleMessageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	// Игнорируем всех ботов и вебхуки, чтобы избежать зацикливания
	if m.Author == nil || m.Author.Bot || m.WebhookID != "" {
		return
	}

	var targetChannelID string
	switch m.ChannelID {
	case b.channelRU:
		targetChannelID = b.channelINT
	case b.channelINT:
		targetChannelID = b.channelRU
	default:
		return
	}

	// Формируем промпт
	prompt := "Ты — профессиональный переводчик. Переведи сообщение максимально естественно. " +
		"Сохраняй оригинальный стиль: если есть мат — оставь, если текст вежливый — переводи вежливо. " +
		"Не добавляй ничего от себя. Верни ТОЛЬКО текст перевода.\n\nСообщение:\n" + m.Content

	resp, err := b.client.Models.GenerateContent(context.Background(), b.modelName, genai.Text(prompt), &genai.GenerateContentConfig{
		SafetySettings: b.safety,
	})
	if err != nil {
		log.Printf("❌ Gemini Error: %v", err)
		return
	}

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

	// --- ЛОГИКА ВЕБХУКОВ ---
	// Ищем существующий вебхук в целевом канале
	webhooks, err := s.ChannelWebhooks(targetChannelID)
	if err != nil {
		log.Printf("Ошибка получения вебхуков: %v", err)
		return
	}

	var targetWebhook *discordgo.Webhook
	for _, w := range webhooks {
		if w.Name == "Bridge-Translator" {
			targetWebhook = w
			break
		}
	}

	// Если вебхука нет, создаем его
	if targetWebhook == nil {
		targetWebhook, err = s.WebhookCreate(targetChannelID, "Bridge-Translator", "")
		if err != nil {
			log.Printf("Ошибка создания вебхука: %v", err)
			return
		}
	}

	// Отправляем сообщение от имени и с аватаром отправителя
	_, err = s.WebhookExecute(targetWebhook.ID, targetWebhook.Token, false, &discordgo.WebhookParams{
		Content:   translated,
		Username:  m.Author.Username,
		AvatarURL: m.Author.AvatarURL("128"),
	})

	if err != nil {
		log.Printf("Ошибка отправки через вебхук: %v", err)
	}
}