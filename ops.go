package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Ops keeps operational data for alerts and the daily report:
//   - usage metrics in hourly buckets (characters, requests, tokens per provider),
//   - an event journal (errors, crashes, restarts, server reboots, disconnects),
//   - run state used to detect crashes and reboots across restarts.
//
// Everything is persisted to one JSON file so it survives restarts.
type Ops struct {
	path string

	mu    sync.Mutex
	state opsState
	dirty bool
}

type opsState struct {
	Usage map[string]int64 `json:"usage"` // "2026-11-10T21|azure|chars" -> n (UTC hour buckets)

	Events []Event `json:"events"`

	Running       bool      `json:"running"`        // true while the bot runs; false after a clean shutdown
	LastHeartbeat time.Time `json:"last_heartbeat"` // last sign of life (for crash time)
	BootTime      time.Time `json:"boot_time"`      // server boot time seen on the previous start
	StartedAt     time.Time `json:"started_at"`
	LastReport    time.Time `json:"last_report"`
}

// Event kinds.
const (
	EvError      = "error"
	EvCrash      = "crash"
	EvRestart    = "restart"
	EvReboot     = "reboot"
	EvDisconnect = "disconnect"
)

type Event struct {
	Time time.Time `json:"t"`
	Kind string    `json:"k"`
	Text string    `json:"x"`
}

const (
	usageRetention = 62 * 24 * time.Hour
	eventRetention = 14 * 24 * time.Hour
	maxEvents      = 1000
)

// ops is the process-wide recorder; nil-safe helpers below make it optional (e.g. in tests).
var ops *Ops

func OpenOps(path string) (*Ops, error) {
	o := &Ops{path: path, state: opsState{Usage: map[string]int64{}}}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return o, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &o.state); err != nil {
		return nil, err
	}
	if o.state.Usage == nil {
		o.state.Usage = map[string]int64{}
	}
	return o, nil
}

// Start records how the previous run ended and whether the server rebooted.
// It reports whether the previous run crashed.
func (o *Ops) Start(now, bootTime time.Time) (crashed bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	s := &o.state
	if !s.BootTime.IsZero() && !bootTime.IsZero() && bootTime.Sub(s.BootTime).Abs() > time.Minute {
		o.addEvent(bootTime, EvReboot, "Сервер перезагружался")
	}
	switch {
	case s.Running:
		crashed = true
		at := s.LastHeartbeat
		if at.IsZero() {
			at = now
		}
		o.addEvent(at, EvCrash, "Бот аварийно остановился (не было штатного завершения), systemd его перезапустил")
	case !s.StartedAt.IsZero():
		o.addEvent(now, EvRestart, "Бот перезапущен штатно (обновление, смена ключа или ручной перезапуск)")
	}
	s.Running = true
	s.StartedAt = now
	s.LastHeartbeat = now
	if !bootTime.IsZero() {
		s.BootTime = bootTime
	}
	o.dirty = true
	return crashed
}

// Stop marks a clean shutdown.
func (o *Ops) Stop() {
	o.mu.Lock()
	o.state.Running = false
	o.dirty = true
	o.mu.Unlock()
}

func (o *Ops) Heartbeat(now time.Time) {
	o.mu.Lock()
	o.state.LastHeartbeat = now
	o.dirty = true
	o.mu.Unlock()
}

func (o *Ops) addEvent(t time.Time, kind, text string) {
	o.state.Events = append(o.state.Events, Event{Time: t, Kind: kind, Text: text})
	if len(o.state.Events) > maxEvents {
		o.state.Events = o.state.Events[len(o.state.Events)-maxEvents:]
	}
	o.dirty = true
}

// Record adds an event to the journal (nil-safe).
func (o *Ops) Record(kind, text string) {
	if o == nil {
		return
	}
	o.mu.Lock()
	o.addEvent(time.Now(), kind, text)
	o.mu.Unlock()
}

// recordError logs an error and adds it to the journal for the daily report.
func recordError(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	log.Print(msg)
	ops.Record(EvError, msg)
}

// ---------------------------------------------------------------------------
// Usage metrics
// ---------------------------------------------------------------------------

func usageKey(t time.Time, provider, metric string) string {
	return t.UTC().Format("2006-01-02T15") + "|" + provider + "|" + metric
}

// Add increments a usage metric for the current hour (nil-safe).
func (o *Ops) Add(provider, metric string, n int64) {
	if o == nil || n == 0 {
		return
	}
	o.mu.Lock()
	o.state.Usage[usageKey(time.Now(), provider, metric)] += n
	o.dirty = true
	o.mu.Unlock()
}

// Sum returns a metric over [from, to) with hour granularity: an hourly bucket
// counts if it overlaps the period.
func (o *Ops) Sum(provider, metric string, from, to time.Time) int64 {
	if o == nil || !to.After(from) {
		return 0
	}
	lo := from.UTC().Format("2006-01-02T15")
	hi := to.Add(-time.Nanosecond).UTC().Format("2006-01-02T15") // inclusive
	suffix := "|" + provider + "|" + metric
	o.mu.Lock()
	defer o.mu.Unlock()
	var total int64
	for k, v := range o.state.Usage {
		if strings.HasSuffix(k, suffix) && k[:13] >= lo && k[:13] <= hi {
			total += v
		}
	}
	return total
}

// MonthSum returns a metric for the current calendar month (UTC).
func (o *Ops) MonthSum(provider, metric string) int64 {
	now := time.Now().UTC()
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	return o.Sum(provider, metric, start, now.Add(time.Nanosecond))
}

// Providers lists providers that have any usage recorded.
func (o *Ops) Providers() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	seen := map[string]bool{}
	for k := range o.state.Usage {
		if parts := strings.SplitN(k, "|", 3); len(parts) == 3 {
			seen[parts[1]] = true
		}
	}
	var out []string
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// EventsBetween returns journal events in [from, to).
func (o *Ops) EventsBetween(from, to time.Time) []Event {
	o.mu.Lock()
	defer o.mu.Unlock()
	var out []Event
	for _, e := range o.state.Events {
		if !e.Time.Before(from) && e.Time.Before(to) {
			out = append(out, e)
		}
	}
	return out
}

func (o *Ops) LastReport() time.Time {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.state.LastReport
}

func (o *Ops) SetLastReport(t time.Time) {
	o.mu.Lock()
	o.state.LastReport = t
	o.dirty = true
	o.mu.Unlock()
}

func (o *Ops) StartedAt() time.Time {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.state.StartedAt
}

// Flush prunes old data and writes the state to disk if it changed.
func (o *Ops) Flush() error {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	if !o.dirty {
		o.mu.Unlock()
		return nil
	}
	cut := time.Now().Add(-usageRetention).UTC().Format("2006-01-02T15")
	for k := range o.state.Usage {
		if k[:13] < cut {
			delete(o.state.Usage, k)
		}
	}
	evCut := time.Now().Add(-eventRetention)
	i := 0
	for i < len(o.state.Events) && o.state.Events[i].Time.Before(evCut) {
		i++
	}
	o.state.Events = o.state.Events[i:]
	raw, err := json.Marshal(o.state)
	o.dirty = false
	o.mu.Unlock()
	if err != nil {
		return err
	}
	tmp := o.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, o.path)
}

// serverBootTime reads the kernel boot time (Linux); zero if unavailable.
func serverBootTime() time.Time {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return time.Time{}
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "btime "); ok {
			if sec, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
				return time.Unix(sec, 0)
			}
		}
	}
	return time.Time{}
}
