package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Groq runs open LLMs (default qwen/qwen3.8-27b: ~0.2s, the most natural chat
// translations in our tests) on its own hardware with an OpenAI-compatible API.
// It answers in well under a second, so the bot gets LLM-quality translation
// (slang, abbreviations, swearing) without the latency of free Gemini.
type Groq struct {
	key, model string
	endpoint   string
	http       *http.Client
}

func NewGroq(key, model string) *Groq {
	return &Groq{
		key: key, model: model,
		endpoint: "https://api.groq.com/openai/v1/chat/completions",
		http:     &http.Client{Timeout: 15 * time.Second},
	}
}

func (g *Groq) Name() string  { return "groq" }
func (g *Groq) Label() string { return "groq(" + g.model + ")" }

func (g *Groq) Translate(ctx context.Context, text, hint string, to Lang) (string, error) {
	m := mask(text)
	system, user := llmPrompts(m, hint, to)
	body := map[string]any{
		"model": g.model,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
		"temperature": 0.2,
		"max_tokens":  2048,
	}
	// Reasoning models: keep thinking short and out of the reply.
	switch {
	case strings.Contains(g.model, "gpt-oss"):
		body["reasoning_effort"] = "low"
		body["include_reasoning"] = false
	case strings.Contains(g.model, "qwen"):
		body["reasoning_format"] = "hidden"
	}
	raw, _ := json.Marshal(body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.endpoint, bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+g.key)
	req.Header.Set("Content-Type", "application/json")

	resp, err := g.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		msg := fmt.Sprintf("groq status %d: %s", resp.StatusCode, strings.TrimSpace(e.Error.Message))
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			cd := 30 * time.Second
			if s, err := strconv.ParseFloat(resp.Header.Get("Retry-After"), 64); err == nil && s > 0 {
				cd = time.Duration(s*float64(time.Second)) + time.Second
			}
			return "", &limitError{msg: msg, cooldown: cd}
		}
		return "", fmt.Errorf("%s", msg)
	}

	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			TotalTokens int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil || len(parsed.Choices) == 0 {
		return "", fmt.Errorf("groq: bad response: %s", strings.TrimSpace(string(data)))
	}
	ops.Add(g.Name(), "tokens", parsed.Usage.TotalTokens)
	out := strings.TrimSpace(parsed.Choices[0].Message.Content)
	if out == "" {
		return "", fmt.Errorf("groq returned an empty response")
	}
	return m.restore(out, false), nil
}
