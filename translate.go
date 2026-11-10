package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"google.golang.org/genai"
)

// Lang is a translation target language.
type Lang struct {
	Name  string // human-readable name used in the LLM prompt
	DeepL string // DeepL target_lang code
}

var (
	LangEN = Lang{Name: "English", DeepL: "EN-US"}
	LangRU = Lang{Name: "Russian", DeepL: "RU"}
)

// Translator translates a single chat message. hint is optional surrounding
// text (e.g. the message being replied to) that helps disambiguate meaning.
type Translator interface {
	Name() string
	Translate(ctx context.Context, text, hint string, to Lang) (string, error)
}

// errRateLimited marks provider errors caused by quotas, so the fallback chain
// can temporarily skip that provider.
var errRateLimited = errors.New("rate limited")

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

func (g *Gemini) Name() string { return "gemini(" + g.model + ")" }

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
		if errors.As(err, &apiErr) && apiErr.Code == http.StatusTooManyRequests {
			return "", fmt.Errorf("%w: %v", errRateLimited, err)
		}
		return "", err
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
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == 456: // 456 = monthly quota used up
		return "", fmt.Errorf("%w: deepl status %d", errRateLimited, resp.StatusCode)
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
}

func NewFallback(chain ...Translator) *Fallback {
	return &Fallback{chain: chain, timeout: 10 * time.Second, cooldown: time.Minute, skipUntil: map[string]time.Time{}}
}

func (f *Fallback) Name() string {
	names := make([]string, len(f.chain))
	for i, t := range f.chain {
		names[i] = t.Name()
	}
	return strings.Join(names, " -> ")
}

func (f *Fallback) Translate(ctx context.Context, text, hint string, to Lang) (string, error) {
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
			return out, nil
		}
		if errors.Is(err, errRateLimited) {
			f.mu.Lock()
			f.skipUntil[t.Name()] = time.Now().Add(f.cooldown)
			f.mu.Unlock()
		}
		errs = append(errs, fmt.Errorf("%s: %w", t.Name(), err))
	}
	return "", errors.Join(errs...)
}
