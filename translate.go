package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"google.golang.org/genai"
)

// Lang is a translation target language.
type Lang struct {
	Name  string // human-readable name used in the LLM prompt
	DeepL string // DeepL target_lang code
	ISO   string // ISO 639-1 code (Azure)
}

var (
	LangEN = Lang{Name: "English", DeepL: "EN-US", ISO: "en"}
	LangRU = Lang{Name: "Russian", DeepL: "RU", ISO: "ru"}
)

// Translator translates a single chat message. hint is optional surrounding
// text (e.g. the message being replied to) that helps disambiguate meaning.
type Translator interface {
	Name() string
	Translate(ctx context.Context, text, hint string, to Lang) (string, error)
}

// errRateLimited marks provider errors caused by quotas or overload, so the
// fallback chain can temporarily skip that provider.
var errRateLimited = errors.New("rate limited")

// limitError is a rate-limit error with a provider-specific cooldown
// (e.g. DeepL's monthly quota is skipped much longer than a per-minute limit).
type limitError struct {
	msg      string
	cooldown time.Duration
}

func (e *limitError) Error() string        { return e.msg }
func (e *limitError) Is(target error) bool { return target == errRateLimited }

// ---------------------------------------------------------------------------
// Protecting Discord syntax from the translator
// ---------------------------------------------------------------------------

// protectedRe matches fragments that must survive translation untouched:
// code blocks, inline code, custom emoji, mentions, channels, timestamps, URLs.
var protectedRe = regexp.MustCompile("```[\\s\\S]*?```|`[^`\\n]+`|<a?:\\w+:\\d+>|<@[!&]?\\d+>|<#\\d+>|<t:\\d+(?::[a-zA-Z])?>|https?://\\S+")

var placeholderRe = regexp.MustCompile(`<x i="(\d+)"\s*/>`)

// masked is a message with protected fragments replaced by <x i="N"/> tags.
type masked struct {
	pieces []string // text pieces; len(pieces) == len(kept)+1
	kept   []string // protected originals
}

func mask(s string) masked {
	var m masked
	last := 0
	for _, loc := range protectedRe.FindAllStringIndex(s, -1) {
		m.pieces = append(m.pieces, s[last:loc[0]])
		m.kept = append(m.kept, s[loc[0]:loc[1]])
		last = loc[1]
	}
	m.pieces = append(m.pieces, s[last:])
	return m
}

// render builds the string sent to a translator. escape=true XML-escapes the
// text pieces (needed for DeepL's tag_handling=xml).
func (m masked) render(escape bool) string {
	var b strings.Builder
	for i, p := range m.pieces {
		if escape {
			p = html.EscapeString(p)
		}
		b.WriteString(p)
		if i < len(m.kept) {
			fmt.Fprintf(&b, `<x i="%d"/>`, i)
		}
	}
	return b.String()
}

// restore puts protected fragments back. Fragments the translator dropped are
// appended at the end so nothing (links, mentions) is ever lost.
func (m masked) restore(out string, unescape bool) string {
	used := make([]bool, len(m.kept))
	var b strings.Builder
	last := 0
	for _, loc := range placeholderRe.FindAllStringSubmatchIndex(out, -1) {
		text := out[last:loc[0]]
		if unescape {
			text = html.UnescapeString(text)
		}
		b.WriteString(text)
		idx, _ := strconv.Atoi(out[loc[2]:loc[3]])
		if idx >= 0 && idx < len(m.kept) && !used[idx] {
			b.WriteString(m.kept[idx])
			used[idx] = true
		}
		last = loc[1]
	}
	tail := out[last:]
	if unescape {
		tail = html.UnescapeString(tail)
	}
	b.WriteString(tail)
	res := strings.TrimSpace(b.String())
	for i, ok := range used {
		if !ok {
			res += " " + m.kept[i]
		}
	}
	return res
}

// needsTranslation reports whether the text contains any words at all
// (messages made only of emoji, links, mentions or code are passed through).
func needsTranslation(s string) bool {
	for _, r := range protectedRe.ReplaceAllString(s, "") {
		if unicode.IsLetter(r) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Gemini
// ---------------------------------------------------------------------------

type Gemini struct {
	client *genai.Client
	model  string
}

func NewGemini(ctx context.Context, apiKey, model string) (*Gemini, error) {
	c, err := genai.NewClient(ctx, &genai.ClientConfig{APIKey: apiKey, Backend: genai.BackendGeminiAPI})
	if err != nil {
		return nil, err
	}
	return &Gemini{client: c, model: model}, nil
}

func (g *Gemini) Name() string { return "gemini" }

// Label includes the model, for logs.
func (g *Gemini) Label() string { return "gemini(" + g.model + ")" }

func (g *Gemini) Translate(ctx context.Context, text, hint string, to Lang) (string, error) {
	m := mask(text)
	system := "You translate Discord chat messages into " + to.Name + ". " +
		"If the message is already in " + to.Name + ", return it unchanged. " +
		"Convey the original meaning naturally, keeping tone, slang, profanity, emoji and Discord markdown " +
		"(**bold**, *italic*, ||spoiler||, > quotes, line breaks). " +
		`Keep every <x i="N"/> tag exactly as written, in the matching position. ` +
		"Output only the translation, with no quotes, notes or explanations."

	prompt := m.render(false)
	if hint != "" {
		prompt = "Earlier message, for context only (do not translate or output it):\n" + hint +
			"\n\nMessage to translate:\n" + prompt
	}

	temp := float32(0.2)
	cfg := &genai.GenerateContentConfig{
		SystemInstruction: genai.NewContentFromText(system, genai.RoleUser),
		Temperature:       &temp,
		SafetySettings: []*genai.SafetySetting{
			{Category: genai.HarmCategoryDangerousContent, Threshold: genai.HarmBlockThresholdBlockNone},
			{Category: genai.HarmCategoryHarassment, Threshold: genai.HarmBlockThresholdBlockNone},
			{Category: genai.HarmCategoryHateSpeech, Threshold: genai.HarmBlockThresholdBlockNone},
			{Category: genai.HarmCategorySexuallyExplicit, Threshold: genai.HarmBlockThresholdBlockNone},
		},
	}

	resp, err := g.client.Models.GenerateContent(ctx, g.model, genai.Text(prompt), cfg)
	if err != nil {
		var apiErr genai.APIError
		// 429 = quota, 503 = "model is experiencing high demand" (common on the free tier).
		if errors.As(err, &apiErr) && (apiErr.Code == http.StatusTooManyRequests || apiErr.Code == http.StatusServiceUnavailable) {
			return "", &limitError{msg: err.Error(), cooldown: 30 * time.Second}
		}
		return "", err
	}
	if resp.UsageMetadata != nil {
		ops.Add(g.Name(), "tokens", int64(resp.UsageMetadata.TotalTokenCount))
	}
	out := strings.TrimSpace(resp.Text())
	if out == "" {
		return "", errors.New("gemini returned an empty response")
	}
	return m.restore(out, false), nil
}

// ---------------------------------------------------------------------------
// DeepL
// ---------------------------------------------------------------------------

type DeepL struct {
	key      string
	endpoint string
	http     *http.Client
}

func NewDeepL(key string) *DeepL {
	endpoint := "https://api.deepl.com/v2/translate"
	if strings.HasSuffix(key, ":fx") { // free-plan keys end with :fx
		endpoint = "https://api-free.deepl.com/v2/translate"
	}
	return &DeepL{key: key, endpoint: endpoint, http: &http.Client{Timeout: 15 * time.Second}}
}

func (d *DeepL) Name() string { return "deepl" }

func (d *DeepL) Translate(ctx context.Context, text, hint string, to Lang) (string, error) {
	m := mask(text)
	body := map[string]any{
		"text":                []string{m.render(true)},
		"target_lang":         to.DeepL,
		"tag_handling":        "xml",
		"preserve_formatting": true,
	}
	if hint != "" {
		body["context"] = hint
	}
	raw, _ := json.Marshal(body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.endpoint, bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "DeepL-Auth-Key "+d.key)
	req.Header.Set("Content-Type", "application/json")

	resp, err := d.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	switch {
	case resp.StatusCode == 456: // monthly character quota used up
		return "", &limitError{msg: "deepl: monthly quota exceeded", cooldown: time.Hour}
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable:
		return "", &limitError{msg: fmt.Sprintf("deepl status %d", resp.StatusCode), cooldown: 30 * time.Second}
	case resp.StatusCode != http.StatusOK:
		return "", fmt.Errorf("deepl status %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}

	var parsed struct {
		Translations []struct {
			DetectedSourceLanguage string `json:"detected_source_language"`
			Text                   string `json:"text"`
		} `json:"translations"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil || len(parsed.Translations) == 0 {
		return "", fmt.Errorf("deepl: bad response: %s", strings.TrimSpace(string(data)))
	}
	tr := parsed.Translations[0]
	// Already in the target language: keep the author's original wording.
	if strings.HasPrefix(to.DeepL, tr.DetectedSourceLanguage) {
		return text, nil
	}
	return m.restore(tr.Text, true), nil
}

// Usage reports characters used and the plan limit (QuotaReporter).
func (d *DeepL) Usage(ctx context.Context) (used, limit int64, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(d.endpoint, "/translate")+"/usage", nil)
	if err != nil {
		return 0, 0, err
	}
	req.Header.Set("Authorization", "DeepL-Auth-Key "+d.key)
	resp, err := d.http.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, 0, fmt.Errorf("deepl usage: status %d", resp.StatusCode)
	}
	var u struct {
		Count int64 `json:"character_count"`
		Limit int64 `json:"character_limit"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&u); err != nil {
		return 0, 0, err
	}
	return u.Count, u.Limit, nil
}

// QuotaReporter is implemented by providers that can tell how much quota is left.
type QuotaReporter interface {
	Name() string
	Usage(ctx context.Context) (used, limit int64, err error)
}

// ---------------------------------------------------------------------------
// Fallback chain
// ---------------------------------------------------------------------------

// Fallback tries translators in order. A provider that hit its quota is skipped
// for a cooldown period so the chat isn't slowed down by doomed requests.
type Fallback struct {
	chain    []Translator
	timeout  time.Duration
	cooldown time.Duration

	mu        sync.Mutex
	skipUntil map[string]time.Time

	// OnLimit is called when a provider is put on cooldown (optional).
	OnLimit func(provider string, cooldown time.Duration, err error)
}

func NewFallback(chain ...Translator) *Fallback {
	return &Fallback{chain: chain, timeout: 10 * time.Second, cooldown: time.Minute, skipUntil: map[string]time.Time{}}
}

func (f *Fallback) Name() string {
	names := make([]string, len(f.chain))
	for i, t := range f.chain {
		names[i] = t.Name()
		if l, ok := t.(interface{ Label() string }); ok {
			names[i] = l.Label()
		}
	}
	return strings.Join(names, " -> ")
}

func (f *Fallback) Translate(ctx context.Context, text, hint string, to Lang) (string, error) {
	out, _, err := f.TranslateNamed(ctx, text, hint, to)
	return out, err
}

// TranslateNamed also reports which provider produced the translation.
func (f *Fallback) TranslateNamed(ctx context.Context, text, hint string, to Lang) (string, string, error) {
	var errs []error
	for i, t := range f.chain {
		last := i == len(f.chain)-1
		f.mu.Lock()
		skip := time.Now().Before(f.skipUntil[t.Name()])
		f.mu.Unlock()
		if skip && !last {
			continue
		}

		cctx, cancel := context.WithTimeout(ctx, f.timeout)
		out, err := t.Translate(cctx, text, hint, to)
		cancel()
		if err == nil {
			ops.Add(t.Name(), "requests", 1)
			ops.Add(t.Name(), "chars", int64(utf8.RuneCountInString(text)))
			return out, t.Name(), nil
		}
		ops.Add(t.Name(), "errors", 1)
		recordError("%s: %v", t.Name(), err)
		if errors.Is(err, errRateLimited) {
			cd := f.cooldown
			var le *limitError
			if errors.As(err, &le) && le.cooldown > 0 {
				cd = le.cooldown
			}
			f.mu.Lock()
			f.skipUntil[t.Name()] = time.Now().Add(cd)
			f.mu.Unlock()
			log.Printf("%s unavailable, skipping it for %s: %v", t.Name(), cd, err)
			if f.OnLimit != nil {
				f.OnLimit(t.Name(), cd, err)
			}
		}
		errs = append(errs, fmt.Errorf("%s: %w", t.Name(), err))
	}
	return "", "", errors.Join(errs...)
}
