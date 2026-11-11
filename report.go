package main

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
	_ "time/tzdata" // embedded time zones, so Europe/Berlin works on any server
)

// Reporter sends a daily .txt summary (usage, errors, crashes) to the alert channel/DM.
type Reporter struct {
	n         *Notifier
	ops       *Ops
	quotas    []QuotaReporter
	providers []string // in order of preference
	loc       *time.Location
	hour, min int
}

func NewReporter(n *Notifier, o *Ops, quotas []QuotaReporter, providers []string, tz, at string) (*Reporter, error) {
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, fmt.Errorf("report time zone %q: %w", tz, err)
	}
	var h, m int
	if _, err := fmt.Sscanf(at, "%d:%d", &h, &m); err != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return nil, fmt.Errorf("report time %q must look like 09:00", at)
	}
	return &Reporter{n: n, ops: o, quotas: quotas, providers: providers, loc: loc, hour: h, min: m}, nil
}

// lastSlot is the most recent scheduled report time at or before now.
func (r *Reporter) lastSlot(now time.Time) time.Time {
	t := now.In(r.loc)
	slot := time.Date(t.Year(), t.Month(), t.Day(), r.hour, r.min, 0, 0, r.loc)
	if slot.After(t) {
		slot = slot.AddDate(0, 0, -1)
	}
	return slot
}

// Run sends the report every day at the configured time. If a report was
// missed while the bot was down, it is sent right after startup.
func (r *Reporter) Run(ctx context.Context) {
	if r.ops.LastReport().IsZero() {
		r.ops.SetLastReport(time.Now()) // first run: start counting from now
	} else if r.ops.LastReport().Before(r.lastSlot(time.Now())) {
		r.send(time.Now())
	}
	// SIGUSR1 sends a report on demand: sudo systemctl kill -s USR1 discord-translator
	now := make(chan os.Signal, 1)
	signal.Notify(now, syscall.SIGUSR1)
	defer signal.Stop(now)
	for {
		next := r.lastSlot(time.Now()).AddDate(0, 0, 1)
		select {
		case <-ctx.Done():
			return
		case <-now:
			r.sendOnDemand(time.Now())
		case <-time.After(time.Until(next)):
			r.send(time.Now())
		}
	}
}

// sendOnDemand reports the last 24 hours without moving the daily report window.
func (r *Reporter) sendOnDemand(now time.Time) {
	text := r.Build(now.Add(-24*time.Hour), now)
	name := "bot-report-" + now.In(r.loc).Format("2006-01-02_1504") + ".txt"
	if err := r.n.SendFile("📊 Сводка бота-переводчика по запросу (последние 24 часа)", name, text); err != nil {
		log.Printf("on-demand report: %v", err)
	}
}

func (r *Reporter) send(now time.Time) {
	from := r.ops.LastReport()
	if from.IsZero() || now.Sub(from) > 7*24*time.Hour {
		from = now.Add(-24 * time.Hour)
	}
	text := r.Build(from, now)
	name := "bot-report-" + now.In(r.loc).Format("2006-01-02") + ".txt"
	if err := r.n.SendFile("📊 Ежедневная сводка бота-переводчика", name, text); err != nil {
		log.Printf("daily report: %v", err)
		return
	}
	r.ops.SetLastReport(now)
	_ = r.ops.Flush()
}

// Build renders the report for [from, to).
func (r *Reporter) Build(from, to time.Time) string {
	var b strings.Builder
	tf := func(t time.Time) string { return t.In(r.loc).Format("02.01.2006 15:04") }
	fmt.Fprintf(&b, "СВОДКА БОТА-ПЕРЕВОДЧИКА\nПериод: %s — %s (%s)\n\n", tf(from), tf(to), r.loc)

	// --- Messages
	msgs := r.ops.Sum("bridge", "messages", from, to)
	pass := r.ops.Sum("bridge", "passthrough", from, to)
	failed := r.ops.Sum("bridge", "untranslated", from, to)
	local := r.ops.Sum("bridge", "localcopies", from, to)
	fmt.Fprintf(&b, "СООБЩЕНИЯ\n  Переслано всего: %d\n  Без перевода (эмодзи, ссылки, код): %d\n  Не удалось перевести (ушли как есть): %d\n  Переводов на местный язык (чужие языки): %d\n\n", msgs, pass, failed, local)

	// --- Usage per provider
	quota := map[string][2]int64{}
	for _, q := range r.quotas {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		used, limit, err := q.Usage(ctx)
		cancel()
		if err == nil {
			quota[q.Name()] = [2]int64{used, limit}
		}
	}
	b.WriteString("ИСПОЛЬЗОВАНИЕ ПЕРЕВОДЧИКОВ (за период)\n")
	b.WriteString("  Azure и DeepL считают символы, ИИ-модели (Groq, Gemini) — токены.\n")
	for _, p := range r.providers {
		req := r.ops.Sum(p, "requests", from, to)
		chars := r.ops.Sum(p, "chars", from, to)
		errs := r.ops.Sum(p, "errors", from, to)
		fmt.Fprintf(&b, "  %s:\n    запросов: %d, символов: %s, ошибок: %d\n", p, req, humanInt(chars), errs)
		if p == "gemini" || p == "groq" {
			fmt.Fprintf(&b, "    токенов: %s (за месяц: %s)\n",
				humanInt(r.ops.Sum(p, "tokens", from, to)), humanInt(r.ops.MonthSum(p, "tokens")))
		}
		if q, ok := quota[p]; ok && q[1] > 0 {
			left := q[1] - q[0]
			period := "за месяц"
			if p == "deepl" {
				period = "всего"
			}
			fmt.Fprintf(&b, "    лимит %s: использовано %s из %s, осталось %s (%d%%)\n",
				period, humanInt(q[0]), humanInt(q[1]), humanInt(left), left*100/q[1])
		}
	}
	b.WriteString("\n")

	// --- Crashes, restarts, reboots, disconnects
	events := r.ops.EventsBetween(from, to)
	var incidents, errs []Event
	for _, e := range events {
		if e.Kind == EvError {
			errs = append(errs, e)
		} else {
			incidents = append(incidents, e)
		}
	}
	fmt.Fprintf(&b, "СВЯЗЬ С DISCORD\n  Плановых переподключений (норма, секунды): %d\n  Обрывов дольше минуты: %d\n\n",
		r.ops.Sum("discord", "reconnects", from, to)-r.ops.Sum("discord", "outages", from, to),
		r.ops.Sum("discord", "outages", from, to))
	fmt.Fprintf(&b, "СЕРВЕР\n%s  Минут с перегрузкой CPU за период: %d\n  Минут, когда облако отбирало CPU (steal > 50%%): %d\n\n",
		serverSnapshot(), r.ops.Sum("server", "overload_minutes", from, to), r.ops.Sum("server", "steal_minutes", from, to))
	b.WriteString("ПАДЕНИЯ И ПЕРЕЗАПУСКИ\n")
	if len(incidents) == 0 {
		b.WriteString("  Не было.\n")
	}
	for _, e := range incidents {
		fmt.Fprintf(&b, "  [%s] %s\n", tf(e.Time), e.Text)
	}
	fmt.Fprintf(&b, "  Бот работает без перерыва с: %s\n\n", tf(r.ops.StartedAt()))

	// --- Errors, grouped by text
	b.WriteString("ОШИБКИ\n")
	if len(errs) == 0 {
		b.WriteString("  Не было.\n")
	}
	type group struct {
		text        string
		count       int
		first, last time.Time
	}
	groups := map[string]*group{}
	var order []string
	for _, e := range errs {
		g, ok := groups[e.Text]
		if !ok {
			g = &group{text: e.Text, first: e.Time}
			groups[e.Text] = g
			order = append(order, e.Text)
		}
		g.count++
		g.last = e.Time
	}
	sort.SliceStable(order, func(i, j int) bool { return groups[order[i]].count > groups[order[j]].count })
	for _, k := range order {
		g := groups[k]
		when := tf(g.first)
		if g.count > 1 {
			when += " … " + tf(g.last)
		}
		fmt.Fprintf(&b, "  ×%d [%s] %s\n", g.count, when, g.text)
	}
	return b.String()
}

// SendFile posts a text file to the alert channel and/or DM.
func (n *Notifier) SendFile(caption, name, content string) error {
	if !n.Enabled() {
		return fmt.Errorf("no ALERT_CHANNEL_ID or ALERT_USER_ID configured")
	}
	var targets []string
	if n.channelID != "" {
		targets = append(targets, n.channelID)
	}
	if n.userID != "" {
		if dm, err := n.s.UserChannelCreate(n.userID); err == nil {
			targets = append(targets, dm.ID)
		}
	}
	var lastErr error
	for _, ch := range targets {
		_, err := n.s.ChannelMessageSendComplex(ch, &discordgo.MessageSend{
			Content: caption,
			Files: []*discordgo.File{{
				Name:        name,
				ContentType: "text/plain; charset=utf-8",
				Reader:      bytes.NewReader([]byte(content)),
			}},
		})
		if err != nil {
			lastErr = err
		}
	}
	return lastErr
}
