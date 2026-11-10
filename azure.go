package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

// Azure is Microsoft's Azure AI Translator. Its free tier (F0) renews monthly,
// so it works well as the primary translator. Azure doesn't report remaining
// free quota through the API, so characters are counted locally.
type Azure struct {
	key, region  string
	endpoint     string
	monthlyLimit int64
	counter      *UsageCounter
	http         *http.Client
}

func NewAzure(key, region string, monthlyLimit int64, counter *UsageCounter) *Azure {
	return &Azure{
		key: key, region: region, monthlyLimit: monthlyLimit, counter: counter,
		endpoint: "https://api.cognitive.microsofttranslator.com/translate",
		http:     &http.Client{Timeout: 15 * time.Second},
	}
}

func (a *Azure) Name() string { return "azure" }

func (a *Azure) Translate(ctx context.Context, text, _ string, to Lang) (string, error) {
	m := mask(text)
	input := m.render(true) // textType=html keeps our <x i="N"/> placeholders intact
	raw, _ := json.Marshal([]map[string]string{{"Text": input}})

	q := url.Values{"api-version": {"3.0"}, "to": {to.ISO}, "textType": {"html"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.endpoint+"?"+q.Encode(), bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	req.Header.Set("Ocp-Apim-Subscription-Key", a.key)
	if a.region != "" && a.region != "global" {
		req.Header.Set("Ocp-Apim-Subscription-Region", a.region)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		msg := fmt.Sprintf("azure status %d: %s", resp.StatusCode, strings.TrimSpace(e.Error.Message))
		switch {
		case e.Error.Code == 403001: // free monthly quota used up
			return "", &limitError{msg: msg, cooldown: 6 * time.Hour}
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			return "", &limitError{msg: msg, cooldown: 30 * time.Second}
		}
		return "", fmt.Errorf("%s", msg)
	}

	var parsed []struct {
		Detected struct {
			Language string `json:"language"`
		} `json:"detectedLanguage"`
		Translations []struct {
			Text string `json:"text"`
		} `json:"translations"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil || len(parsed) == 0 || len(parsed[0].Translations) == 0 {
		return "", fmt.Errorf("azure: bad response: %s", strings.TrimSpace(string(data)))
	}
	if a.counter != nil {
		a.counter.Add(a.Name(), int64(utf8.RuneCountInString(input)))
	}
	if strings.HasPrefix(parsed[0].Detected.Language, to.ISO) {
		return text, nil // already in the target language
	}
	return m.restore(parsed[0].Translations[0].Text, true), nil
}

// Usage reports locally counted characters for the current month.
func (a *Azure) Usage(context.Context) (int64, int64, error) {
	return a.counter.ThisMonth(a.Name()), a.monthlyLimit, nil
}
