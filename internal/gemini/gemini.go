// Package gemini is a minimal REST client for the Gemini API (plan parsing and
// audio transcription). It uses only the standard library.
package gemini

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"voiceplan/internal/plan"
	"voiceplan/internal/tips"
)

const DefaultBaseURL = "https://generativelanguage.googleapis.com"

type Client struct {
	APIKey        string
	Model         string // from LLM_MODEL, never hard-coded
	FallbackModel string // optional (LLM_FALLBACK_MODEL), tried if Model stays overloaded
	BaseURL       string
	HTTP          *http.Client
	Sleep         func(ctx context.Context, d time.Duration) // injectable for tests
}

// StatusError is returned for non-200 responses. It never carries the body,
// which may echo user content.
type StatusError struct{ Code int }

func (e *StatusError) Error() string { return fmt.Sprintf("gemini: unexpected status %d", e.Code) }

// Busy reports whether the failure is transient overload/rate limiting.
func (e *StatusError) Busy() bool { return retryable(e.Code) }

func retryable(code int) bool {
	switch code {
	case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

var retryDelays = []time.Duration{time.Second, 2 * time.Second}

func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

func New(apiKey, model string) *Client {
	return &Client{APIKey: apiKey, Model: model, BaseURL: DefaultBaseURL,
		HTTP: &http.Client{Timeout: 45 * time.Second}}
}

type part map[string]any

// generate calls the model, retrying transient overload (429/5xx) with a short
// backoff, then once more on the fallback model if one is configured.
func (c *Client) generate(ctx context.Context, body map[string]any) (string, error) {
	models := []string{c.Model}
	if c.FallbackModel != "" && c.FallbackModel != c.Model {
		models = append(models, c.FallbackModel)
	}
	sleep := c.Sleep
	if sleep == nil {
		sleep = sleepCtx
	}
	var lastErr error
	for _, m := range models {
		for attempt := 0; attempt <= len(retryDelays); attempt++ {
			if attempt > 0 {
				sleep(ctx, retryDelays[attempt-1])
			}
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			out, err := c.generateOnce(ctx, m, body)
			if err == nil {
				return out, nil
			}
			lastErr = err
			var se *StatusError
			if !errors.As(err, &se) || !se.Busy() {
				return "", err // permanent failure: retrying will not help
			}
			log.Printf("gemini: model %s busy (status %d), attempt %d", m, se.Code, attempt+1)
		}
	}
	return "", lastErr
}

func (c *Client) generateOnce(ctx context.Context, model string, body map[string]any) (string, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	url := fmt.Sprintf("%s/v1beta/models/%s:generateContent", strings.TrimRight(c.BaseURL, "/"), model)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", c.APIKey)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("gemini request: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		// Status only: the body may echo user content, which must not reach logs.
		return "", &StatusError{Code: resp.StatusCode}
	}
	var out struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
		PromptFeedback struct {
			BlockReason string `json:"blockReason"`
		} `json:"promptFeedback"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("gemini: bad response: %w", err)
	}
	if len(out.Candidates) == 0 {
		return "", fmt.Errorf("gemini: no candidates (block=%q)", out.PromptFeedback.BlockReason)
	}
	var sb strings.Builder
	for _, p := range out.Candidates[0].Content.Parts {
		sb.WriteString(p.Text)
	}
	return sb.String(), nil
}

func responseSchema() map[string]any {
	str := func(nullable bool) map[string]any {
		m := map[string]any{"type": "STRING"}
		if nullable {
			m["nullable"] = true
		}
		return m
	}
	return map[string]any{
		"type": "OBJECT",
		"properties": map[string]any{
			"title":            str(false),
			"start_time":       str(true),
			"end_time":         str(true),
			"duration_minutes": map[string]any{"type": "INTEGER", "nullable": true},
			"location":         str(false),
			"notes":            str(false),
			"category":         map[string]any{"type": "STRING", "enum": plan.Categories},
			"confidence":       map[string]any{"type": "NUMBER"},
			"ambiguities":      map[string]any{"type": "ARRAY", "items": map[string]any{"type": "STRING"}},
		},
		"required": []string{"title", "start_time", "end_time", "duration_minutes", "location", "notes", "category", "confidence", "ambiguities"},
	}
}

// ParsePlan asks the model for a structured plan. `now` anchors relative dates.
func (c *Client) ParsePlan(ctx context.Context, text string, now time.Time) (plan.Raw, error) {
	body := map[string]any{
		"systemInstruction": map[string]any{"parts": []part{{"text": plan.SystemPrompt(now)}}},
		"contents":          []map[string]any{{"role": "user", "parts": []part{{"text": text}}}},
		"generationConfig": map[string]any{
			"responseMimeType": "application/json",
			"responseSchema":   responseSchema(),
			// Newer Gemini models "think" by default and thinking tokens count toward
			// this limit, so leave generous headroom or the JSON gets cut off.
			"maxOutputTokens": 8192,
		},
	}
	out, err := c.generate(ctx, body)
	if err != nil {
		return plan.Raw{}, err
	}
	var raw plan.Raw
	dec := json.NewDecoder(strings.NewReader(stripFences(out)))
	if err := dec.Decode(&raw); err != nil {
		return plan.Raw{}, fmt.Errorf("gemini: model returned invalid JSON: %w", err)
	}
	return raw, nil
}

// Transcribe converts a short audio clip (e.g. audio/wav) to text.
func (c *Client) Transcribe(ctx context.Context, audio []byte, mime string) (string, error) {
	if len(audio) == 0 {
		return "", errors.New("empty audio")
	}
	body := map[string]any{
		"contents": []map[string]any{{"role": "user", "parts": []part{
			{"inlineData": map[string]any{"mimeType": mime, "data": base64.StdEncoding.EncodeToString(audio)}},
			{"text": "請逐字轉錄這段語音,使用繁體中文(台灣用語)。只輸出轉錄的文字,不要加任何說明。如果沒有聽到語音,輸出空白。"},
		}}},
		"generationConfig": map[string]any{"maxOutputTokens": 4096},
	}
	out, err := c.generate(ctx, body)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// stripFences removes ```json fences some models add despite the JSON mime type.
func stripFences(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	return strings.TrimSpace(s)
}

// Tips writes short, actionable pre-event advice for the event's category.
// The event fields are data, not instructions. Returns "" for categories that
// get no advice; callers fall back to no tips on error.
func (c *Client) Tips(ctx context.Context, ev plan.Event, now time.Time) (string, error) {
	if tips.Skip(ev.Category) {
		return "", nil
	}
	start := ev.Start.In(plan.Taipei)
	var facts strings.Builder
	fmt.Fprintf(&facts, "標題:%s\n類別:%s\n時間:%s(台北),預計 %d 分鐘\n",
		ev.Title, ev.Category, start.Format("2006-01-02 15:04"), ev.DurationMinutes)
	if ev.Location != "" {
		fmt.Fprintf(&facts, "地點:%s\n", ev.Location)
	}
	if ev.Notes != "" {
		fmt.Fprintf(&facts, "備註:%s\n", ev.Notes)
	}
	body := map[string]any{
		"systemInstruction": map[string]any{"parts": []part{{"text": tips.System(ev, now)}}},
		"contents":          []map[string]any{{"role": "user", "parts": []part{{"text": facts.String()}}}},
		"generationConfig":  map[string]any{"maxOutputTokens": 4096},
	}
	out, err := c.generate(ctx, body)
	if err != nil {
		return "", err
	}
	return tips.Clean(out, tips.MaxLines(ev.Category)), nil
}
