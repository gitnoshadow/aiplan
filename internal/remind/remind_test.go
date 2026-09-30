package remind

import (
	"strings"
	"testing"
	"time"

	"voiceplan/internal/plan"
)

var now = time.Date(2026, 9, 30, 10, 0, 0, 0, plan.Taipei)

func TestScheduleNormal(t *testing.T) {
	start := now.Add(5 * time.Hour)
	due, late, ok := Schedule(start, 60, now)
	if !ok || late || !due.Equal(start.Add(-time.Hour)) {
		t.Fatalf("%v %v %v", due, late, ok)
	}
}

func TestScheduleLateWhenLeadAlreadyPassed(t *testing.T) {
	// Starts in 30 minutes, default lead is 60: send right now, flagged late.
	due, late, ok := Schedule(now.Add(30*time.Minute), 60, now)
	if !ok || !late || !due.Equal(now) {
		t.Fatalf("%v %v %v", due, late, ok)
	}
}

func TestScheduleSkipsStartedEvents(t *testing.T) {
	for _, start := range []time.Time{now, now.Add(-time.Minute), now.Add(-3 * time.Hour)} {
		if _, _, ok := Schedule(start, 60, now); ok {
			t.Errorf("event at %v already started, must not schedule", start)
		}
	}
}

func TestNextAttempt(t *testing.T) {
	want := []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute}
	for i, d := range want {
		got, ok := NextAttempt(i+1, now)
		if !ok || !got.Equal(now.Add(d)) {
			t.Errorf("attempt %d: %v %v", i+1, got, ok)
		}
	}
	if _, ok := NextAttempt(MaxAttempts, now); ok {
		t.Error("must give up after the final attempt")
	}
	if _, ok := NextAttempt(0, now); ok {
		t.Error("zero attempts is not a failure")
	}
}

func TestFormat(t *testing.T) {
	start := time.Date(2026, 10, 7, 14, 0, 0, 0, plan.Taipei) // Wednesday
	got := Format(Message{Title: "看牙醫", Location: "康美診所", Notes: "帶健保卡", Tips: "・提早 10 分鐘到", Start: start}, start.Add(-45*time.Minute))
	for _, want := range []string{"⏰ 提醒:看牙醫", "10/7(三) 14:00", "約 45 分鐘後開始", "📍 康美診所", "📝 帶健保卡", "行前注意事項", "・提早 10 分鐘到"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "提醒時間已過") {
		t.Error("late note must not appear on a normal reminder")
	}
}

func TestFormatMinimalAndLate(t *testing.T) {
	start := now.Add(20 * time.Minute)
	got := Format(Message{Title: "開會", Start: start, Late: true}, now)
	if strings.Contains(got, "📍") || strings.Contains(got, "行前注意事項") || !strings.Contains(got, "提醒時間已過") {
		t.Fatalf("%s", got)
	}
	if !strings.Contains(got, "約 20 分鐘後開始") {
		t.Fatalf("%s", got)
	}
	// A far-away event shows no countdown.
	far := Format(Message{Title: "x", Start: now.Add(72 * time.Hour)}, now)
	if strings.Contains(far, "後開始") {
		t.Fatalf("%s", far)
	}
}
