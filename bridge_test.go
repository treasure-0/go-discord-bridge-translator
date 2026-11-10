package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

func TestMaskRestoreKeepsDiscordSyntax(t *testing.T) {
	in := "привет <@123> смотри https://x.com/a?b=1&c=2 и `code` <:pepe:42> <t:1700000000:R>"
	m := mask(in)
	rendered := m.render(true)
	if strings.Contains(rendered, "<@123>") || strings.Contains(rendered, "https://") {
		t.Fatalf("protected parts leaked into translator input: %q", rendered)
	}
	// Simulate a translator that keeps tags but reorders text.
	out := strings.NewReplacer("привет", "hi", "смотри", "look", " и ", " and ").Replace(rendered)
	got := m.restore(out, true)
	want := "hi <@123> look https://x.com/a?b=1&c=2 and `code` <:pepe:42> <t:1700000000:R>"
	if got != want {
		t.Fatalf("restore:\n got %q\nwant %q", got, want)
	}
}

func TestRestoreAppendsDroppedFragments(t *testing.T) {
	m := mask("смотри https://example.com")
	got := m.restore("look", false)
	if got != "look https://example.com" {
		t.Fatalf("got %q", got)
	}
}

func TestRestoreUnescapesDeepLText(t *testing.T) {
	m := mask("a < b & c")
	if got := m.restore(m.render(true), true); got != "a < b & c" {
		t.Fatalf("got %q", got)
	}
}

func TestNeedsTranslation(t *testing.T) {
	cases := map[string]bool{
		"привет":                     true,
		"😂😂":                         false,
		"https://example.com <@1>":   false,
		"```go\nfunc main() {}\n```": false,
		"lol https://x.com":          true,
		"":                           false,
	}
	for in, want := range cases {
		if got := needsTranslation(in); got != want {
			t.Errorf("needsTranslation(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestSplitMessage(t *testing.T) {
	body := strings.Repeat("слово ", 700) // ~4200 chars
	chunks := splitMessage("> quote\n", body, discordLimit)
	if len(chunks) != 3 {
		t.Fatalf("expected 3 chunks, got %d", len(chunks))
	}
	if !strings.HasPrefix(chunks[0], "> quote\n") {
		t.Fatal("prefix must stay in the first chunk")
	}
	for i, c := range chunks {
		if utf16Len(c) > discordLimit {
			t.Fatalf("chunk %d too long: %d", i, utf16Len(c))
		}
		if strings.HasPrefix(c, "лово") {
			t.Fatalf("chunk %d cut mid-word", i)
		}
	}
	if got := splitMessage("", "short", discordLimit); len(got) != 1 || got[0] != "short" {
		t.Fatalf("short message: %q", got)
	}
}

func TestSnippetIsLinkSafe(t *testing.T) {
	s := snippet("look [here](http://x) **bold**\nnext <@1>", 80)
	if strings.ContainsAny(s, "[]()*\n") {
		t.Fatalf("unsafe snippet %q", s)
	}
}

func TestWebhookName(t *testing.T) {
	if n := webhookName("DiscordFan"); strings.Contains(strings.ToLower(n), "discord") {
		t.Fatalf("banned word left in %q", n)
	}
	if n := webhookName("   "); n != "user" {
		t.Fatalf("empty name -> %q", n)
	}
}

func TestStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.json")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Put(Record{SourceID: "10", TargetIDs: []string{"20", "21"}, Author: "A"})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	s2, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if r, ok := s2.ByTarget("21"); !ok || r.SourceID != "10" {
		t.Fatalf("ByTarget after reload: %+v %v", r, ok)
	}
	s2.Delete("10")
	if _, ok := s2.ByTarget("20"); ok {
		t.Fatal("delete must drop reverse index")
	}
}

func TestStorePrunesOldest(t *testing.T) {
	s, _ := OpenStore(filepath.Join(t.TempDir(), "m.json"))
	s.maxRecords = 8
	for _, id := range []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "10"} {
		s.Put(Record{SourceID: id})
	}
	if _, ok := s.BySource("1"); ok {
		t.Fatal("oldest record should be pruned")
	}
	if _, ok := s.BySource("10"); !ok {
		t.Fatal("newest record must stay")
	}
}

type fakeTr struct {
	name string
	err  error
	out  string
	hits int
}

func (f *fakeTr) Name() string { return f.name }
func (f *fakeTr) Translate(context.Context, string, string, Lang) (string, error) {
	f.hits++
	return f.out, f.err
}

func TestFallbackSkipsRateLimitedProvider(t *testing.T) {
	a := &fakeTr{name: "a", err: errRateLimited}
	b := &fakeTr{name: "b", out: "ok"}
	f := NewFallback(a, b)
	for i := 0; i < 3; i++ {
		out, err := f.Translate(context.Background(), "x", "", LangEN)
		if err != nil || out != "ok" {
			t.Fatalf("got %q %v", out, err)
		}
	}
	if a.hits != 1 {
		t.Fatalf("rate-limited provider should be on cooldown, hits=%d", a.hits)
	}
}

func TestFallbackAllFail(t *testing.T) {
	f := NewFallback(&fakeTr{name: "a", err: errors.New("boom")})
	if _, err := f.Translate(context.Background(), "x", "", LangEN); err == nil {
		t.Fatal("expected error")
	}
}

func TestReplyPrefixPointsAtCounterpart(t *testing.T) {
	s, _ := OpenStore(filepath.Join(t.TempDir(), "m.json"))
	// Original "100" in RU was translated to "200" in INT.
	s.Put(Record{SourceID: "100", SourceChannel: "ru", TargetChannel: "int", TargetIDs: []string{"200"},
		Author: "Vasya", SourceText: "привет", TargetText: "hi"})
	b := &Bridge{store: s, routes: map[string]Route{"ru": {Target: "int", Lang: LangEN}, "int": {Target: "ru", Lang: LangRU}}}

	// Someone in RU replies to the original -> quote links to the translation in INT.
	m := &discordgo.Message{GuildID: "g", ChannelID: "ru", MessageReference: &discordgo.MessageReference{},
		ReferencedMessage: &discordgo.Message{ID: "100", ChannelID: "ru"}}
	got := b.replyPrefix(m, b.routes["ru"])
	if !strings.Contains(got, "/g/int/200") || !strings.Contains(got, "hi") || !strings.Contains(got, "Vasya") {
		t.Fatalf("reply to original: %q", got)
	}

	// Someone in INT replies to the translation -> quote links back to the original in RU.
	m = &discordgo.Message{GuildID: "g", ChannelID: "int", MessageReference: &discordgo.MessageReference{},
		ReferencedMessage: &discordgo.Message{ID: "200", ChannelID: "int"}}
	got = b.replyPrefix(m, b.routes["int"])
	if !strings.Contains(got, "/g/ru/100") || !strings.Contains(got, "привет") {
		t.Fatalf("reply to translation: %q", got)
	}
}

func TestFallbackUsesProviderCooldown(t *testing.T) {
	a := &fakeTr{name: "a", err: &limitError{msg: "quota", cooldown: time.Hour}}
	b := &fakeTr{name: "b", out: "ok"}
	f := NewFallback(a, b)
	_, name, err := f.TranslateNamed(context.Background(), "x", "", LangEN)
	if err != nil || name != "b" {
		t.Fatalf("got %q %v", name, err)
	}
	f.mu.Lock()
	until := f.skipUntil["a"]
	f.mu.Unlock()
	if time.Until(until) < 50*time.Minute {
		t.Fatalf("expected ~1h cooldown, got %s", time.Until(until))
	}
	if !errors.Is(&limitError{}, errRateLimited) {
		t.Fatal("limitError must match errRateLimited")
	}
}

func TestHumanIntAndNilNotifier(t *testing.T) {
	if got := humanInt(2000000); got != "2 000 000" {
		t.Fatalf("humanInt: %q", got)
	}
	var n *Notifier
	n.Alert("x", time.Minute, "nil notifier must be a no-op")
}

func TestOpsCrashRebootAndUsage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ops.json")
	o, _ := OpenOps(path)
	boot := time.Now().Add(-48 * time.Hour)
	if o.Start(time.Now(), boot) {
		t.Fatal("first start is not a crash")
	}
	o.Add("azure", "chars", 1500)
	o.Add("azure", "chars", 500)
	o.Flush() // process dies here without Stop()

	o2, _ := OpenOps(path)
	if !o2.Start(time.Now(), boot.Add(time.Hour)) { // and the server rebooted
		t.Fatal("expected crash to be detected")
	}
	kinds := map[string]bool{}
	for _, e := range o2.EventsBetween(time.Now().Add(-72*time.Hour), time.Now().Add(time.Minute)) {
		kinds[e.Kind] = true
	}
	if !kinds[EvCrash] || !kinds[EvReboot] {
		t.Fatalf("events: %v", kinds)
	}
	if got := o2.MonthSum("azure", "chars"); got != 2000 {
		t.Fatalf("usage after reload: %d", got)
	}
	o2.Stop()
	o2.Flush()
	o3, _ := OpenOps(path)
	if o3.Start(time.Now(), boot.Add(time.Hour)) {
		t.Fatal("clean shutdown must not count as a crash")
	}
}

func TestReportBuild(t *testing.T) {
	o, _ := OpenOps(filepath.Join(t.TempDir(), "ops.json"))
	o.Start(time.Now(), time.Time{})
	o.Add("bridge", "messages", 5)
	o.Add("azure", "requests", 4)
	o.Add("azure", "chars", 321)
	o.Add("gemini", "tokens", 77)
	o.Record(EvError, "deepl: status 503")
	o.Record(EvError, "deepl: status 503")
	o.Record(EvCrash, "Бот аварийно остановился")
	r, err := NewReporter(nil, o, nil, []string{"azure", "deepl", "gemini"}, "Europe/Berlin", "09:00")
	if err != nil {
		t.Fatal(err)
	}
	txt := r.Build(time.Now().Add(-24*time.Hour), time.Now()) // "to" = now must include the current hour
	for _, want := range []string{"Переслано всего: 5", "символов: 321", "токенов: 77", "×2", "deepl: status 503", "аварийно"} {
		if !strings.Contains(txt, want) {
			t.Fatalf("report misses %q:\n%s", want, txt)
		}
	}
	slot := r.lastSlot(time.Date(2026, 11, 11, 8, 59, 0, 0, r.loc))
	if slot.Day() != 10 || slot.Hour() != 9 {
		t.Fatalf("lastSlot before 9:00 must be yesterday 9:00, got %v", slot)
	}
	if _, err := NewReporter(nil, o, nil, nil, "Europe/Berlin", "25:00"); err == nil {
		t.Fatal("bad time must be rejected")
	}
}

func TestAzureTranslateAndQuota(t *testing.T) {
	var gotRegion string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRegion = r.Header.Get("Ocp-Apim-Subscription-Region")
		if r.URL.Query().Get("to") == "ru" {
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"error":{"code":403001,"message":"free quota exceeded"}}`))
			return
		}
		var body []map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		out := strings.Replace(body[0]["Text"], "привет", "hello", 1)
		json.NewEncoder(w).Encode([]map[string]any{{"detectedLanguage": map[string]any{"language": "ru"}, "translations": []map[string]string{{"text": out}}}})
	}))
	defer srv.Close()
	ops, _ = OpenOps(filepath.Join(t.TempDir(), "ops.json"))
	defer func() { ops = nil }()
	f := NewFallback(NewAzure("k", "westeurope", 100))
	a := f.chain[0].(*Azure)
	a.endpoint = srv.URL
	out, err := f.Translate(context.Background(), "привет <@1> https://x.y/?a=1&b=2", "", LangEN)
	if err != nil || out != "hello <@1> https://x.y/?a=1&b=2" {
		t.Fatalf("got %q %v", out, err)
	}
	if used, _, _ := a.Usage(context.Background()); gotRegion != "westeurope" || used == 0 {
		t.Fatalf("region %q, usage %d", gotRegion, used)
	}
	_, err = a.Translate(context.Background(), "hi", "", LangRU)
	var le *limitError
	if !errors.As(err, &le) || le.cooldown < time.Hour {
		t.Fatalf("expected long cooldown on quota error, got %v", err)
	}
}

func TestGroqTranslateAndRateLimit(t *testing.T) {
	var gotBody map[string]any
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls > 1 {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":{"message":"rate limit"}}`))
			return
		}
		json.NewDecoder(r.Body).Decode(&gotBody)
		msgs := gotBody["messages"].([]any)
		user := msgs[1].(map[string]any)["content"].(string)
		out := strings.Replace(user, "кек, имхо", "lol, imo", 1)
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": out}}},
			"usage":   map[string]int{"total_tokens": 42},
		})
	}))
	defer srv.Close()
	g := NewGroq("k", "openai/gpt-oss-120b")
	g.endpoint = srv.URL
	out, err := g.Translate(context.Background(), "кек, имхо <@5>", "", LangEN)
	if err != nil || out != "lol, imo <@5>" {
		t.Fatalf("got %q %v", out, err)
	}
	if gotBody["reasoning_effort"] != "low" || !strings.Contains(gotBody["messages"].([]any)[0].(map[string]any)["content"].(string), "slang") {
		t.Fatalf("request: %v", gotBody)
	}
	_, err = g.Translate(context.Background(), "x", "", LangEN)
	var le *limitError
	if !errors.As(err, &le) || le.cooldown != 8*time.Second {
		t.Fatalf("expected Retry-After based cooldown, got %v", err)
	}
}
