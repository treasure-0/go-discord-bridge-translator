package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
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

// ConnMonitor tells routine gateway reconnects (Discord asks bots to reconnect
// several times a day; the library resumes within seconds) apart from real
// outages. Short reconnects are only counted; outages longer than `outage`
// are journaled with their duration and reported to the owner.
type ConnMonitor struct {
	n      *Notifier
	outage time.Duration

	mu        sync.Mutex
	downSince time.Time
	alerted   bool
	timer     *time.Timer
}

func NewConnMonitor(n *Notifier, outage time.Duration) *ConnMonitor {
	return &ConnMonitor{n: n, outage: outage}
}

func (c *ConnMonitor) Register(s *discordgo.Session) {
	s.AddHandler(func(_ *discordgo.Session, _ *discordgo.Disconnect) { c.down(time.Now()) })
	s.AddHandler(func(_ *discordgo.Session, _ *discordgo.Connect) { c.up(time.Now()) })
	s.AddHandler(func(_ *discordgo.Session, _ *discordgo.Resumed) { c.up(time.Now()) })
	s.AddHandler(func(_ *discordgo.Session, _ *discordgo.Ready) { c.up(time.Now()) })
}

func (c *ConnMonitor) down(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.downSince.IsZero() {
		return
	}
	c.downSince = now
	c.alerted = false
	c.timer = time.AfterFunc(c.outage, func() {
		c.mu.Lock()
		since := c.downSince
		if since.IsZero() {
			c.mu.Unlock()
			return
		}
		c.alerted = true
		c.mu.Unlock()
		c.n.Alert("discord-down", 15*time.Minute, fmt.Sprintf(
			"⚠️ Нет связи с Discord уже %s (с %s UTC). Бот продолжает переподключаться.",
			c.outage.Round(time.Second), since.UTC().Format("15:04:05")))
	})
}

func (c *ConnMonitor) up(now time.Time) {
	c.mu.Lock()
	since, alerted := c.downSince, c.alerted
	c.downSince = time.Time{}
	if c.timer != nil {
		c.timer.Stop()
	}
	c.mu.Unlock()
	if since.IsZero() {
		return
	}
	gap := now.Sub(since)
	ops.Add("discord", "reconnects", 1)
	if gap < c.outage {
		return // routine reconnect
	}
	ops.Add("discord", "outages", 1)
	ops.Record(EvDisconnect, fmt.Sprintf("Нет связи с Discord %s (с %s UTC)", humanDuration(gap), since.UTC().Format("15:04:05")))
	if alerted {
		c.n.Alert("discord-up", time.Minute, "✅ Связь с Discord восстановлена после перерыва "+humanDuration(gap)+".")
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
