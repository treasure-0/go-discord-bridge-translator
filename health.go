package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
)

// ---------------------------------------------------------------------------
// Discord connection monitor
// ---------------------------------------------------------------------------

// ConnMonitor journals every gateway disconnect with when it dropped, when it
// came back, why (from discordgo's own log messages) and whether the session
// was resumed (no events lost) or re-identified (events during the gap may be
// lost). Discord asks bots to reconnect several times a day; those routine
// reconnects take seconds. Outages longer than `outage` are also alerted.
type ConnMonitor struct {
	n      *Notifier
	outage time.Duration

	mu         sync.Mutex
	downSince  time.Time
	reason     string    // why the connection dropped
	reasonAt   time.Time // when the reason was logged
	details    []string  // what happened while reconnecting
	resumed    bool
	identified bool
	alerted    bool
	timer      *time.Timer
}

func NewConnMonitor(n *Notifier, outage time.Duration) *ConnMonitor {
	return &ConnMonitor{n: n, outage: outage}
}

// Register hooks the session events and takes over discordgo's logger so the
// monitor sees reconnect reasons. Library warnings and errors still go to the log.
func (c *ConnMonitor) Register(s *discordgo.Session) {
	s.LogLevel = discordgo.LogInformational
	discordgo.Logger = func(level, _ int, format string, a ...any) {
		msg := fmt.Sprintf(format, a...)
		c.libraryLog(time.Now(), msg)
		if level <= discordgo.LogWarning {
			log.Printf("[discordgo] %s", strings.TrimSpace(msg))
		}
	}
	s.AddHandler(func(_ *discordgo.Session, _ *discordgo.Disconnect) { c.down(time.Now()) })
	s.AddHandler(func(_ *discordgo.Session, _ *discordgo.Resumed) { c.up(time.Now()) })
	s.AddHandler(func(_ *discordgo.Session, _ *discordgo.Ready) { c.up(time.Now()) })
}

// libraryLog classifies discordgo's messages into human-readable reasons.
func (c *ConnMonitor) libraryLog(now time.Time, msg string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case strings.Contains(msg, "sending resume packet"):
		c.resumed = true
		return
	case strings.Contains(msg, "in response to Op9"):
		c.identified = true
	case strings.Contains(msg, "First Packet") || strings.Contains(msg, "sending identify"):
		// identify on a fresh connection; resume flag decides below
	}
	reason := explainGatewayLog(msg)
	if reason == "" {
		return
	}
	if c.downSince.IsZero() {
		c.reason, c.reasonAt = reason, now // logged right before the Disconnect event
		return
	}
	if len(c.details) < 5 && (len(c.details) == 0 || c.details[len(c.details)-1] != reason) {
		c.details = append(c.details, reason)
	}
}

var closeCodeRe = regexp.MustCompile(`close (\d{4})`)

// explainGatewayLog turns a discordgo log line into a reason, or "" if irrelevant.
func explainGatewayLog(msg string) string {
	switch {
	case strings.Contains(msg, "in response to Op7"):
		return "Discord сам попросил переподключиться (Op 7 Reconnect) — плановая операция Discord: перебалансировка или обновление его серверов"
	case strings.Contains(msg, "in response to Op9"):
		return "Discord признал сессию недействительной (Op 9 Invalid Session), бот авторизовался заново"
	case strings.Contains(msg, "heartbeat ACK"):
		return "Discord перестал подтверждать сигнал «я на связи» (нет heartbeat ACK) — соединение зависло: сетевая задержка или сбой на стороне Discord"
	case strings.Contains(msg, "error sending heartbeat"):
		return "Не удалось отправить сигнал «я на связи» (heartbeat) — соединение уже оборвано: " + lastPart(msg)
	case strings.Contains(msg, "error reading from gateway"):
		return explainSocketError(lastPart(msg))
	case strings.Contains(msg, "error reconnecting to gateway"), strings.Contains(msg, "error connecting to gateway"):
		return "Попытка переподключения не удалась: " + lastPart(msg)
	}
	return ""
}

func explainSocketError(err string) string {
	if m := closeCodeRe.FindStringSubmatch(err); m != nil {
		code := m[1]
		meaning := map[string]string{
			"1000": "штатное закрытие со стороны Discord",
			"1001": "сервер Discord уходит на перезапуск (going away)",
			"1006": "соединение оборвалось без закрытия — сбой сети между сервером и Discord",
			"1011": "внутренняя ошибка сервера Discord",
			"1012": "сервер Discord перезапускается",
			"4000": "неизвестная ошибка Discord, можно переподключаться",
			"4007": "сбилась нумерация событий, сессию пришлось начать заново",
			"4008": "бот слишком часто отправлял запросы (rate limit)",
			"4009": "сессия истекла по таймауту",
			"4014": "не выданы нужные разрешения (intents) в Developer Portal",
		}[code]
		if meaning == "" {
			meaning = "код закрытия Discord"
		}
		return fmt.Sprintf("Discord закрыл соединение, код %s: %s (%s)", code, meaning, err)
	}
	switch {
	case strings.Contains(err, "timeout"):
		return "Таймаут сети при чтении из Discord: " + err
	case strings.Contains(err, "reset by peer"), strings.Contains(err, "EOF"), strings.Contains(err, "broken pipe"):
		return "Соединение сброшено (сеть или сервер Discord): " + err
	}
	return "Ошибка соединения с Discord: " + err
}

func lastPart(msg string) string {
	msg = strings.TrimSpace(msg)
	if i := strings.LastIndex(msg, ", "); i >= 0 {
		return msg[i+2:]
	}
	return msg
}

func (c *ConnMonitor) down(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.downSince.IsZero() {
		return
	}
	c.downSince = now
	if now.Sub(c.reasonAt) > 10*time.Second {
		c.reason = "" // the logged reason is stale, not about this disconnect
	}
	c.details, c.resumed, c.identified, c.alerted = nil, false, false, false
	c.timer = time.AfterFunc(c.outage, func() {
		c.mu.Lock()
		since, reason := c.downSince, c.reason
		if since.IsZero() {
			c.mu.Unlock()
			return
		}
		c.alerted = true
		c.mu.Unlock()
		msg := fmt.Sprintf("⚠️ Нет связи с Discord уже %s (с %s UTC). Бот продолжает переподключаться.",
			c.outage.Round(time.Second), since.UTC().Format("15:04:05"))
		if reason != "" {
			msg += "\nПричина: " + reason
		}
		c.n.Alert("discord-down", 15*time.Minute, msg)
	})
}

func (c *ConnMonitor) up(now time.Time) {
	c.mu.Lock()
	since, alerted := c.downSince, c.alerted
	reason, details, resumed := c.reason, c.details, c.resumed && !c.identified
	c.downSince, c.reason = time.Time{}, ""
	if c.timer != nil {
		c.timer.Stop()
	}
	c.mu.Unlock()
	if since.IsZero() {
		return // initial connect
	}
	gap := now.Sub(since)
	ops.Add("discord", "reconnects", 1)
	if reason == "" {
		reason = "Discord закрыл соединение без объяснения (обычно плановое обслуживание его серверов)"
	}
	how := "сессия восстановлена (resume) — сообщения за перерыв не потеряны"
	if !resumed {
		how = "начата новая сессия — сообщения, отправленные во время перерыва, бот мог не увидеть"
	}
	if len(details) > 0 {
		how += "; при переподключении: " + strings.Join(details, "; ")
	}
	ops.RecordDisconnect(since, now, reason, how)
	if gap >= c.outage {
		ops.Add("discord", "outages", 1)
		if alerted {
			c.n.Alert("discord-up", time.Minute, "✅ Связь с Discord восстановлена после перерыва "+humanDuration(gap)+".")
		}
	}
}

func humanDuration(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%d с", int(d.Seconds()))
	}
	m := int(d.Minutes())
	s := int(d.Seconds()) % 60
	if m < 60 {
		return fmt.Sprintf("%d мин %d с", m, s)
	}
	return fmt.Sprintf("%d ч %d мин", m/60, m%60)
}

// ---------------------------------------------------------------------------
// Server resources
// ---------------------------------------------------------------------------

// WatchServer samples CPU load every minute and counts overloaded minutes
// (1-minute load average above the CPU count) and CPU steal (time the cloud
// host took the CPU away from this VM — typical for small free-tier VMs).
func WatchServer(ctx context.Context) {
	prevTotal, prevSteal := cpuStat()
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if l, ok := loadAvg1(); ok && l > float64(cpuCount()) {
				ops.Add("server", "overload_minutes", 1)
			}
			total, steal := cpuStat()
			if dt := total - prevTotal; dt > 0 && float64(steal-prevSteal)/float64(dt) > 0.5 {
				ops.Add("server", "steal_minutes", 1) // more than half of the CPU time was taken away
			}
			prevTotal, prevSteal = total, steal
		}
	}
}

func loadAvg1() (float64, bool) {
	raw, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, false
	}
	f := strings.Fields(string(raw))
	if len(f) == 0 {
		return 0, false
	}
	v, err := strconv.ParseFloat(f[0], 64)
	return v, err == nil
}

func cpuCount() int {
	raw, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return 1
	}
	n := strings.Count(string(raw), "\nprocessor")
	if strings.HasPrefix(string(raw), "processor") {
		n++
	}
	if n < 1 {
		n = 1
	}
	return n
}

// cpuStat returns total and steal jiffies from the aggregate "cpu" line.
func cpuStat() (total, steal uint64) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return 0, 0
	}
	fields := strings.Fields(sc.Text())
	if len(fields) < 9 || fields[0] != "cpu" {
		return 0, 0
	}
	for i, v := range fields[1:] {
		n, _ := strconv.ParseUint(v, 10, 64)
		total += n
		if i == 7 { // user nice system idle iowait irq softirq steal
			steal = n
		}
	}
	return total, steal
}

// serverSnapshot describes the server right now, for the daily report.
func serverSnapshot() string {
	var b strings.Builder
	if raw, err := os.ReadFile("/proc/loadavg"); err == nil {
		f := strings.Fields(string(raw))
		if len(f) >= 3 {
			fmt.Fprintf(&b, "  Нагрузка CPU (1/5/15 мин): %s / %s / %s, ядер: %d\n", f[0], f[1], f[2], cpuCount())
		}
	}
	mem := map[string]int64{}
	if f, err := os.Open("/proc/meminfo"); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			p := strings.Fields(sc.Text())
			if len(p) >= 2 {
				v, _ := strconv.ParseInt(p[1], 10, 64)
				mem[strings.TrimSuffix(p[0], ":")] = v / 1024 // MB
			}
		}
		f.Close()
		if mem["MemTotal"] > 0 {
			fmt.Fprintf(&b, "  Память: занято %d из %d МБ, подкачка: занято %d из %d МБ\n",
				mem["MemTotal"]-mem["MemAvailable"], mem["MemTotal"], mem["SwapTotal"]-mem["SwapFree"], mem["SwapTotal"])
		}
	}
	var st syscall.Statfs_t
	if syscall.Statfs("/", &st) == nil {
		free := int64(st.Bavail) * int64(st.Bsize) / (1 << 30)
		total := int64(st.Blocks) * int64(st.Bsize) / (1 << 30)
		fmt.Fprintf(&b, "  Диск: свободно %d из %d ГБ\n", free, total)
	}
	if bt := serverBootTime(); !bt.IsZero() {
		fmt.Fprintf(&b, "  Сервер работает без перезагрузки: %s\n", humanDuration(time.Since(bt)))
	}
	return b.String()
}
