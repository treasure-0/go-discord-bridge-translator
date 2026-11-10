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
)

// Azure is Microsoft's Azure AI Translator. Its free tier (F0) renews monthly,
// so it works well as the primary translator. Azure doesn't report remaining
// free quota through the API, so characters are counted locally (see Ops).
type Azure struct {
	key, region  string
	endpoint     string
	monthlyLimit int64
	http         *http.Client
}

func NewAzure(key, region string, monthlyLimit int64) *Azure {
	return &Azure{
		key: key, region: region, monthlyLimit: monthlyLimit,
		endpoint: "https://api.cognitive.microsofttranslator.com/translate",
		http:     &http.Client{Timeout: 15 * time.Second},
	}
}

func (a *Azure) Name() string { return "azure" }

func (a *Azure) Translate(ctx context.Context, text, _ string, to Lang) (Result, error) {
	m := mask(text)
	input := m.render(true) // textType=html keeps our <x i="N"/> placeholders intact
	raw, _ := json.Marshal([]map[string]string{{"Text": input}})

	q := url.Values{"api-version": {"3.0"}, "to": {to.ISO}, "textType": {"html"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.endpoint+"?"+q.Encode(), bytes.NewReader(raw))
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Ocp-Apim-Subscription-Key", a.key)
	if a.region != "" && a.region != "global" {
		req.Header.Set("Ocp-Apim-Subscription-Region", a.region)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.http.Do(req)
	if err != nil {
		return Result{}, err
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
			return Result{}, &limitError{msg: msg, cooldown: 6 * time.Hour}
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			return Result{}, &limitError{msg: msg, cooldown: 30 * time.Second}
		}
		return Result{}, fmt.Errorf("%s", msg)
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
		return Result{}, fmt.Errorf("azure: bad response: %s", strings.TrimSpace(string(data)))
	}
	lang := normLang(parsed[0].Detected.Language)
	if lang == to.ISO {
		return Result{Text: text, Lang: lang}, nil // already in the target language
	}
	return Result{Text: m.restore(parsed[0].Translations[0].Text, true), Lang: lang}, nil
}

// Usage reports locally counted characters for the current month (Azure's API
// doesn't expose remaining free quota).
func (a *Azure) Usage(context.Context) (int64, int64, error) {
	return ops.MonthSum(a.Name(), "chars"), a.monthlyLimit, nil
}
