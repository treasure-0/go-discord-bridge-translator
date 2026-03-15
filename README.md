# AI-Powered Discord Bridge Translator

A high-performance Discord bridge written in **Go**, designed to connect multilingual communities seamlessly. It leverages **Google Gemini 3.0/2.0** for context-aware, natural translations and uses **Discord Webhooks** to preserve user identities (names and avatars) across channels.

## 🚀 Key Features
- **Identity Mirroring:** Uses Discord Webhooks to replicate the original sender's username and avatar in the target channel, maintaining a natural conversation flow.
- **Contextual AI Translation:** Powered by the latest Google Gemini API (v1beta) to ensure slang, idioms, and emotional tone are preserved.
- **Bi-directional Sync:** Automatically synchronizes and translates messages between two designated channels (e.g., Russian ↔ International).
- **Customizable Safety:** Integrates Gemini's safety settings to handle sensitive content according to community guidelines.

## 🛠 Tech Stack
- **Language:** Go (Golang)
- **Frameworks:** [discordgo](https://github.com/bwmarrin/discordgo), [google-genai](https://github.com/google/generative-ai-go)
- **AI Engine:** Google Gemini 2.0 Flash / 3.0 Flash Preview
- **Cloud:** Deployed on Oracle Cloud Infrastructure (OCI)

## 📋 Installation & Setup

1. **Clone the repository:**
   ```bash
   git clone [https://github.com/YOUR_USERNAME/go-discord-bridge-translator.git](https://github.com/YOUR_USERNAME/go-discord-bridge-translator.git)
   cd go-discord-bridge-translator

2. **Configure Environment:**
   Create a `.env` file in the root directory and fill in your credentials:
   ```env
   DISCORD_TOKEN=your_discord_bot_token
   GEMINI_API_KEY=your_google_ai_api_key
   CHANNEL_RU=your_russian_channel_id
   CHANNEL_INT=your_international_channel_id

3. **Install dependencies:**
   ```bash
   go mod tidy

4. **Run the application:**
   ```bash
   go run main.go

## 🛡 License
Distributed under the MIT License. See `LICENSE` for more information.

---
*Developed as a part of a personal portfolio to demonstrate Go backend capabilities and AI integration.*
