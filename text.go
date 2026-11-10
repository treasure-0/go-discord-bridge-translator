package main

import (
	"fmt"
	"hash/fnv"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Discord counts message length in UTF-16 code units.
const discordLimit = 2000

func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r > 0xFFFF {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// splitMessage splits prefix+body into chunks that fit Discord's limit.
// The prefix (reply quote) goes into the first chunk; cuts prefer line breaks, then spaces.
func splitMessage(prefix, body string, limit int) []string {
	text := prefix + body
	var chunks []string
	for utf16Len(text) > limit {
		cut := cutIndex(text, limit)
		chunks = append(chunks, strings.TrimRight(text[:cut], " \n"))
		text = strings.TrimLeft(text[cut:], " \n")
	}
	if text != "" || len(chunks) == 0 {
		chunks = append(chunks, text)
	}
	return chunks
}

// cutIndex returns a byte index <= limit (in UTF-16 units) to split s at.
func cutIndex(s string, limit int) int {
	units, maxByte := 0, 0
	for i, r := range s {
		w := 1
		if r > 0xFFFF {
			w = 2
		}
		if units+w > limit {
			break
		}
		units += w
		maxByte = i + utf8.RuneLen(r)
	}
	window := s[:maxByte]
	if i := strings.LastIndex(window, "\n"); i > maxByte/2 {
		return i + 1
	}
	if i := strings.LastIndex(window, " "); i > maxByte/2 {
		return i + 1
	}
	return maxByte
}

// truncate shortens s to at most n UTF-16 units, adding an ellipsis.
func truncate(s string, n int) string {
	if utf16Len(s) <= n {
		return s
	}
	return strings.TrimSpace(s[:cutIndex(s, n-1)]) + "…"
}

var snippetStrip = regexp.MustCompile("[\\[\\]()`*_|~>\\\\]")

// snippet makes a short one-line preview that is safe inside a markdown link.
func snippet(s string, n int) string {
	s = protectedRe.ReplaceAllString(s, "…")
	s = snippetStrip.ReplaceAllString(s, "")
	s = strings.Join(strings.Fields(s), " ")
	return truncate(s, n)
}

// replyQuote builds the line shown above a translated reply, e.g.
// "> ↪ **Name**: [preview of the message](https://discord.com/channels/...)".
func replyQuote(author, preview, link string) string {
	if preview == "" {
		preview = "📎"
	}
	return fmt.Sprintf("> ↪ **%s**: [%s](%s)\n", escapeMarkdown(author), preview, link)
}

func messageLink(guildID, channelID, messageID string) string {
	return fmt.Sprintf("https://discord.com/channels/%s/%s/%s", guildID, channelID, messageID)
}

var markdownChars = strings.NewReplacer("*", "\\*", "_", "\\_", "~", "\\~", "|", "\\|", "`", "\\`", "[", "\\[", "]", "\\]")

func escapeMarkdown(s string) string { return markdownChars.Replace(s) }

// webhookName adapts a display name to Discord's webhook username rules
// (1–80 chars, must not contain "discord" or "clyde").
func webhookName(name string) string {
	lower := strings.ToLower(name)
	for _, banned := range []string{"discord", "clyde"} {
		for {
			i := strings.Index(lower, banned)
			if i < 0 {
				break
			}
			// Break the banned word with an invisible zero-width space.
			name = name[:i+1] + "​" + name[i+1:]
			lower = strings.ToLower(name)
		}
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = "user"
	}
	if utf8.RuneCountInString(name) > 80 {
		name = string([]rune(name)[:80])
	}
	return name
}

func contentHash(s string) string {
	h := fnv.New64a()
	h.Write([]byte(s))
	return fmt.Sprintf("%x", h.Sum64())
}
