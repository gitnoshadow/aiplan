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
	"net/http"
	"strings"
	"time"

	"voiceplan/internal/plan"
)

const DefaultBaseURL = "https://generativelanguage.googleapis.com"

type Client struct {
	APIKey  string
	Model   string // from LLM_MODEL, never hard-coded
	BaseURL string
	HTTP    *http.Client
}

func New(apiKey, model string) *Client {
	return &Client{APIKey: apiKey, Model: model, BaseURL: DefaultBaseURL,
		HTTP: &http.Client{Timeout: 45 * time.Second}}
}

type part map[string]any

func (c *Client) generate(ctx context.Context, body map[string]any) (string, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	url := fmt.Sprintf("%s/v1beta/models/%s:generateContent", strings.TrimRight(c.BaseURL, "/"), c.Model)
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
		return "", fmt.Errorf("gemini: unexpected status %d", resp.StatusCode)
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
			"temperature":      0,
			"maxOutputTokens":  2048,
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
		"generationConfig": map[string]any{"temperature": 0, "maxOutputTokens": 1024},
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
