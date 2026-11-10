package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

// ---------------------------------------------------------------------------
// Usage counter (for providers that don't report quota themselves)
// ---------------------------------------------------------------------------

// UsageCounter counts characters per provider per calendar month, persisted to disk.
type UsageCounter struct {
	path string
	mu   sync.Mutex
	data map[string]int64 // "provider/2026-11" -> characters
}

func OpenUsageCounter(path string) (*UsageCounter, error) {
	c := &UsageCounter{path: path, data: map[string]int64{}}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	return c, json.Unmarshal(raw, &c.data)
}

func monthKey(provider string) string { return provider + "/" + time.Now().UTC().Format("2006-01") }

func (c *UsageCounter) Add(provider string, n int64) {
	c.mu.Lock()
	c.data[monthKey(provider)] += n
	c.mu.Unlock()
}

func (c *UsageCounter) ThisMonth(provider string) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.data[monthKey(provider)]
}

func (c *UsageCounter) Flush() error {
	c.mu.Lock()
	raw, err := json.Marshal(c.data)
	c.mu.Unlock()
	if err != nil {
		return err
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, c.path)
}

// ---------------------------------------------------------------------------
// Alerts to the bot owner
// ---------------------------------------------------------------------------

// Notifier sends operational alerts to a Discord DM and/or an admin channel.
// Repeated alerts with the same key are suppressed for a while so a bad hour
// doesn't turn into a flood of messages.
type Notifier struct {
	s         *discordgo.Session
	userID    string
	channelID string

	mu   sync.Mutex
	last map[string]time.Time
}

func NewNotifier(s *discordgo.Session, userID, channelID string) *Notifier {
	return &Notifier{s: s, userID: userID, channelID: channelID, last: map[string]time.Time{}}
}

func (n *Notifier) Enabled() bool { return n != nil && (n.userID != "" || n.channelID != "") }

// Alert sends msg unless an alert with the same key was sent within `every`.
func (n *Notifier) Alert(key string, every time.Duration, msg string) {
	if !n.Enabled() {
		return
	}
	n.mu.Lock()
	if t, ok := n.last[key]; ok && time.Since(t) < every {
		n.mu.Unlock()
		return
	}
	n.last[key] = time.Now()
	n.mu.Unlock()

	log.Printf("alert: %s", msg)
	go func() {
		if n.channelID != "" {
			if _, err := n.s.ChannelMessageSend(n.channelID, msg); err != nil {
				log.Printf("alert to channel: %v", err)
			}
		}
		if n.userID != "" {
			dm, err := n.s.UserChannelCreate(n.userID)
			if err == nil {
				_, err = n.s.ChannelMessageSend(dm.ID, msg)
			}
			if err != nil {
				log.Printf("alert to DM: %v", err)
			}
		}
	}()
}

// WatchQuotas periodically checks providers' remaining quota and warns when it
// drops below warnPercent.
func (n *Notifier) WatchQuotas(ctx context.Context, providers []QuotaReporter, warnPercent int64, every time.Duration) {
	check := func() {
		for _, p := range providers {
			cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			used, limit, err := p.Usage(cctx)
			cancel()
			if err != nil {
				log.Printf("quota check %s: %v", p.Name(), err)
				continue
			}
			if limit <= 0 {
				continue
			}
			left := limit - used
			pct := left * 100 / limit
			log.Printf("quota %s: %d/%d used (%d%% left)", p.Name(), used, limit, pct)
			if pct <= warnPercent {
				n.Alert("quota-"+p.Name(), 24*time.Hour, fmt.Sprintf(
					"⚠️ У переводчика **%s** осталось %d%% лимита (%s из %s символов). "+
						"Когда он закончится, бот автоматически переключится на следующий переводчик.",
					p.Name(), pct, humanInt(left), humanInt(limit)))
			}
		}
	}
	check()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			check()
		}
	}
}

func humanInt(n int64) string {
	s := fmt.Sprintf("%d", n)
	var out []byte
	for i := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ' ')
		}
		out = append(out, s[i])
	}
	return string(out)
}
