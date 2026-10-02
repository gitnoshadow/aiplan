package gemini

import (
	"context"
	"encoding/json"
	"errors"
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
	c.Sleep = func(context.Context, time.Duration) {}
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

// sequenceServer answers with the given statuses in order, recording the model used.
func sequenceServer(t *testing.T, statuses []int, models *[]string) *Client {
	t.Helper()
	i := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*models = append(*models, strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1beta/models/"), ":generateContent"))
		code := statuses[min(i, len(statuses)-1)]
		i++
		w.WriteHeader(code)
		if code == 200 {
			_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"ok"}]}}]}`))
		}
	}))
	t.Cleanup(srv.Close)
	c := New("KEY", "main-model")
	c.BaseURL = srv.URL
	c.Sleep = func(context.Context, time.Duration) {}
	return c
}

func TestRetriesOverloadThenSucceeds(t *testing.T) {
	var models []string
	c := sequenceServer(t, []int{503, 503, 200}, &models)
	out, err := c.generate(context.Background(), map[string]any{})
	if err != nil || out != "ok" || len(models) != 3 {
		t.Fatalf("out=%q err=%v calls=%v", out, err, models)
	}
}

func TestFallbackModelUsedWhenMainStaysBusy(t *testing.T) {
	var models []string
	c := sequenceServer(t, []int{503, 503, 503, 200}, &models)
	c.FallbackModel = "backup-model"
	if _, err := c.generate(context.Background(), map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if models[len(models)-1] != "backup-model" || len(models) != 4 {
		t.Fatalf("calls=%v", models)
	}
}

func TestPermanentErrorsAreNotRetried(t *testing.T) {
	for _, code := range []int{400, 401, 403, 404} {
		var models []string
		c := sequenceServer(t, []int{code}, &models)
		_, err := c.generate(context.Background(), map[string]any{})
		var se *StatusError
		if !errors.As(err, &se) || se.Code != code || se.Busy() || len(models) != 1 {
			t.Errorf("code %d: err=%v calls=%d", code, err, len(models))
		}
	}
}

func TestStillBusyAfterAllAttempts(t *testing.T) {
	var models []string
	c := sequenceServer(t, []int{503}, &models)
	_, err := c.generate(context.Background(), map[string]any{})
	var se *StatusError
	if !errors.As(err, &se) || !se.Busy() || len(models) != 3 {
		t.Fatalf("err=%v calls=%d", err, len(models))
	}
}

func TestTipsUsesCategoryPromptAndCleansOutput(t *testing.T) {
	c := fakeServer(t, 200, "**1. 提早 10 分鐘到**\n- 帶健保卡", func(body map[string]any, r *http.Request) {
		sys := body["systemInstruction"].(map[string]any)["parts"].([]any)[0].(map[string]any)["text"].(string)
		user := body["contents"].([]any)[0].(map[string]any)["parts"].([]any)[0].(map[string]any)["text"].(string)
		if !strings.Contains(sys, "健保卡") || !strings.Contains(sys, "不要提供診斷") || !strings.Contains(sys, "只是資料") {
			t.Errorf("medical category guidance missing: %s", sys)
		}
		if !strings.Contains(user, "看牙醫") || !strings.Contains(user, "2026-10-07 14:00") || !strings.Contains(user, "預計 60 分鐘") {
			t.Errorf("event facts missing: %s", user)
		}
	})
	ev := plan.Event{Title: "看牙醫", Category: "medical", Start: time.Date(2026, 10, 7, 14, 0, 0, 0, plan.Taipei), DurationMinutes: 60}
	got, err := c.Tips(context.Background(), ev, time.Date(2026, 10, 1, 10, 0, 0, 0, plan.Taipei))
	if err != nil || got != "・提早 10 分鐘到\n・帶健保卡" {
		t.Fatalf("%q %v", got, err)
	}
}

func TestTipsSkippedForPlainRemindersWithoutCallingTheModel(t *testing.T) {
	called := false
	c := fakeServer(t, 200, "x", func(map[string]any, *http.Request) { called = true })
	ev := plan.Event{Title: "吃藥", Category: "reminder", Start: time.Now().Add(time.Hour), DurationMinutes: 15}
	got, err := c.Tips(context.Background(), ev, time.Now())
	if err != nil || got != "" || called {
		t.Fatalf("got=%q err=%v called=%v", got, err, called)
	}
}
