# Discord Bridge Translator

A Discord bot written in **Go** that bridges two channels in different languages (Russian ↔ English).
Every message is translated and re-posted in the other channel **under the author's own name and avatar**,
so the conversation reads naturally on both sides.

## Features

- **Identity mirroring** — translations are posted through a webhook with the author's server nickname and avatar.
- **Fast, fault-tolerant translation** — **DeepL API Free** answers in a fraction of a second; if it is down or its
  monthly quota is used up, the bot switches to **Google Gemini** (Flash-Lite, good with slang) automatically.
  Overloaded or rate-limited providers are skipped for a cooldown. The order is configurable via `TRANSLATORS`.
- **Replies** — webhooks can't create native Discord replies, so a translated reply starts with a quote line
  (`↪ Name: preview`) that links to the counterpart of the replied-to message in the other channel.
- **Attachments** — files are re-uploaded (Discord CDN links expire); files over the upload limit are posted as links.
  Stickers become image links, forwarded messages are mirrored too.
- **Edits and deletions** follow the original message.
- **Discord syntax is protected** — mentions, custom emoji, links, timestamps and code are never sent through the
  translator, and messages made only of emoji/links are passed through as-is.
- **No accidental pings** — translations never trigger `@everyone`, role or user mentions.
- **Ordered and resilient** — per-channel queues keep message order, the gateway reconnects automatically,
  and systemd restarts the process if it ever crashes.

## How it works

```
#russian ──MessageCreate──▶ queue ──▶ DeepL ─(fail/quota)─▶ Gemini ──▶ webhook (author name + avatar) ──▶ #international
#international ─────────────────────────── same pipeline, translated to Russian ──────────────────────▶ #russian
```

A small JSON message map (`data/messages.json`, last 20 000 pairs) links originals to translations for replies,
edits and deletions.

| File | Purpose |
| --- | --- |
| `main.go` | config, startup, graceful shutdown |
| `bridge.go` | Discord events, webhooks, replies, attachments, edits/deletes |
| `translate.go` | Gemini and DeepL clients, fallback chain, protection of Discord syntax |
| `store.go` | persistent original ↔ translation map |
| `text.go` | message splitting, previews, helpers |
| `deploy/` | systemd unit and server install script |

## Setup

### 1. Discord bot

1. Create an application at <https://discord.com/developers/applications> → **Bot**.
2. Enable **Message Content Intent** (Privileged Gateway Intents). Without it the bot receives empty messages.
3. Invite the bot with the permissions **View Channel**, **Read Message History**, **Send Messages** and
   **Manage Webhooks** on both channels.

### 2. API keys (free tiers are enough for a community server)

- Gemini: <https://aistudio.google.com/apikey>
- DeepL API Free (key ends with `:fx`): <https://www.deepl.com/pro-api>

One of them is enough; with both you get the fallback.

### 3. Run locally

```bash
cp .env.example .env   # fill in tokens and channel IDs
go run .
```

## Deploy 24/7 (Ubuntu server, e.g. Oracle Cloud Always Free)

```bash
curl -fsSL https://raw.githubusercontent.com/treasure-0/go-discord-bridge-translator/main/deploy/install.sh | sudo bash
sudo nano /etc/discord-translator.env        # paste tokens
sudo systemctl start discord-translator
journalctl -u discord-translator -f          # live logs
```

The script installs Go, builds the bot, and registers a hardened systemd service with `Restart=always`
(it also adds swap on 1 GB machines). Run the same command again to update to the latest commit.
The bot only makes outbound connections, so no ports need to be opened.

## Tests

```bash
go test -race ./...
```

## License

MIT — see [LICENSE](LICENSE).
