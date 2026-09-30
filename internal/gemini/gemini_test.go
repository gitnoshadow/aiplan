package gemini

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"voiceplan/internal/plan"
)

func fakeServer(t *testing.T, status int, text string, check func(body map[string]any, r *http.Request)) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		if check != nil {
			check(m, r)
		}
		w.WriteHeader(status)
		resp := map[string]any{"candidates": []any{map[string]any{"content": map[string]any{"parts": []any{map[string]any{"text": text}}}}}}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	c := New("KEY", "test-model")
	c.BaseURL = srv.URL
	return c
}

func TestParsePlanRequestAndResponse(t *testing.T) {
	now := time.Date(2026, 9, 30, 10, 0, 0, 0, plan.Taipei)
	c := fakeServer(t, 200, "```json\n"+`{"title":"看牙醫","start_time":"2026-10-07T14:00:00+08:00","end_time":null,"duration_minutes":null,"location":"","notes":"","category":"medical","confidence":0.9,"ambiguities":[]}`+"\n```",
		func(body map[string]any, r *http.Request) {
			if r.URL.Path != "/v1beta/models/test-model:generateContent" {
				t.Errorf("path %s", r.URL.Path)
			}
			if r.Header.Get("x-goog-api-key") != "KEY" {
				t.Error("api key header missing")
			}
			if strings.Contains(r.URL.RawQuery, "KEY") {
				t.Error("api key must not be in the URL")
			}
			sys := body["systemInstruction"].(map[string]any)["parts"].([]any)[0].(map[string]any)["text"].(string)
			if !strings.Contains(sys, "2026-09-30 10:00") || !strings.Contains(sys, "下週三 = 2026-10-07") {
				t.Error("current Taipei time / calendar anchors missing from prompt")
			}
			cfg := body["generationConfig"].(map[string]any)
			if cfg["responseMimeType"] != "application/json" || cfg["responseSchema"] == nil {
				t.Error("structured output not requested")
			}
		})
	raw, err := c.ParsePlan(context.Background(), "下週三下午兩點看牙醫", now)
	if err != nil {
		t.Fatal(err)
	}
	if raw.Title != "看牙醫" || raw.StartTime == nil || *raw.StartTime != "2026-10-07T14:00:00+08:00" || raw.DurationMinutes != nil {
		t.Fatalf("%+v", raw)
	}
}

func TestParsePlanErrors(t *testing.T) {
	c := fakeServer(t, 500, "secret user text", nil)
	_, err := c.ParsePlan(context.Background(), "x", time.Now())
	if err == nil || strings.Contains(err.Error(), "secret user text") {
		t.Fatalf("error must exist and not leak the body: %v", err)
	}
	c = fakeServer(t, 200, "not json", nil)
	if _, err := c.ParsePlan(context.Background(), "x", time.Now()); err == nil {
		t.Fatal("invalid JSON must error")
	}
}

func TestTranscribe(t *testing.T) {
	c := fakeServer(t, 200, "  明天早上九點開會 \n", func(body map[string]any, r *http.Request) {
		p := body["contents"].([]any)[0].(map[string]any)["parts"].([]any)[0].(map[string]any)["inlineData"].(map[string]any)
		if p["mimeType"] != "audio/wav" || p["data"] == "" {
			t.Errorf("bad inlineData: %v", p)
		}
	})
	got, err := c.Transcribe(context.Background(), []byte("RIFFxxxx"), "audio/wav")
	if err != nil || got != "明天早上九點開會" {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := c.Transcribe(context.Background(), nil, "audio/wav"); err == nil {
		t.Fatal("empty audio must error")
	}
}
