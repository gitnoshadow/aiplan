package plan

import (
	"strings"
	"testing"
	"time"
)

// Fixed "now": Wednesday 2026-09-30 10:00 Taipei.
var now = time.Date(2026, 9, 30, 10, 0, 0, 0, Taipei)

func ptr[T any](v T) *T { return &v }

func TestNextWeekWeekday(t *testing.T) {
	cases := map[time.Weekday]string{
		time.Monday: "2026-10-05", time.Wednesday: "2026-10-07", time.Sunday: "2026-10-11",
	}
	for wd, want := range cases {
		if got := NextWeekWeekday(now, wd).Format("2006-01-02"); got != want {
			t.Errorf("下週 %v = %s want %s", wd, got, want)
		}
	}
	// On a Sunday, "next week" still starts the following Monday.
	sun := time.Date(2026, 10, 4, 9, 0, 0, 0, Taipei)
	if got := NextWeekWeekday(sun, time.Monday).Format("2006-01-02"); got != "2026-10-05" {
		t.Errorf("sunday anchor: %s", got)
	}
}

func TestSystemPromptAnchors(t *testing.T) {
	p := SystemPrompt(now)
	for _, want := range []string{"2026-09-30 10:00", "星期三", "2026-10-01 星期四 明天", "下週三 = 2026-10-07", "+08:00", "Asia/Taipei"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}

func TestNormalizeComplete(t *testing.T) {
	r := Normalize(Raw{Title: "看牙醫", StartTime: ptr("2026-10-07T14:00:00+08:00"), Category: "medical", Confidence: 0.9}, now)
	if r.StartTime != "2026-10-07T14:00:00+08:00" || len(r.Ambiguities) != 0 {
		t.Fatalf("unexpected: %+v", r)
	}
	if r.DurationMinutes != 90 || !r.DurationIsDefault {
		t.Fatalf("medical default should be 90 and flagged: %+v", r)
	}
}

func TestNormalizeExplicitDurationAndEndTime(t *testing.T) {
	r := Normalize(Raw{Title: "會議", StartTime: ptr("2026-10-07T14:00:00+08:00"), DurationMinutes: ptr(45), Category: "meeting"}, now)
	if r.DurationMinutes != 45 || r.DurationIsDefault {
		t.Fatalf("%+v", r)
	}
	r = Normalize(Raw{Title: "會議", StartTime: ptr("2026-10-07T14:00:00+08:00"), EndTime: ptr("2026-10-07T15:30:00+08:00"), Category: "meeting"}, now)
	if r.DurationMinutes != 90 || r.DurationIsDefault {
		t.Fatalf("%+v", r)
	}
	r = Normalize(Raw{Title: "會議", StartTime: ptr("2026-10-07T14:00:00+08:00"), EndTime: ptr("2026-10-07T13:00:00+08:00"), Category: "meeting"}, now)
	if len(r.Ambiguities) != 1 || !r.DurationIsDefault {
		t.Fatalf("end before start must be flagged and fall back: %+v", r)
	}
}

func TestNormalizeVagueDoesNotGuess(t *testing.T) {
	r := Normalize(Raw{Title: "繳費", StartTime: nil, Category: "errand", Ambiguities: []string{"「月底前」沒有明確日期"}}, now)
	if r.StartTime != "" {
		t.Fatalf("must not invent a start time: %+v", r)
	}
	if len(r.Ambiguities) != 2 {
		t.Fatalf("want model ambiguity + missing start, got %v", r.Ambiguities)
	}
}

func TestNormalizePastAndOffsetlessAndBadCategory(t *testing.T) {
	r := Normalize(Raw{Title: "x", StartTime: ptr("2026-09-01T09:00:00"), Category: "bogus"}, now)
	if r.Category != "other" || r.StartTime != "2026-09-01T09:00:00+08:00" {
		t.Fatalf("%+v", r)
	}
	if len(r.Ambiguities) != 1 || !strings.Contains(r.Ambiguities[0], "已經過") {
		t.Fatalf("past start not flagged: %v", r.Ambiguities)
	}
}

func TestNormalizeConvertsToTaipei(t *testing.T) {
	r := Normalize(Raw{Title: "x", StartTime: ptr("2026-10-07T06:00:00Z"), Category: "other"}, now)
	if r.StartTime != "2026-10-07T14:00:00+08:00" {
		t.Fatalf("%s", r.StartTime)
	}
}

func TestValidate(t *testing.T) {
	ok := ConfirmInput{Title: " 看牙醫 ", StartTime: "2026-10-07T14:00:00+08:00", DurationMinutes: 60, Category: "medical"}
	ev, err := ok.Validate()
	if err != nil || ev.Title != "看牙醫" || ev.End().Format(time.RFC3339) != "2026-10-07T15:00:00+08:00" {
		t.Fatalf("%+v %v", ev, err)
	}
	bad := []ConfirmInput{
		{Title: "", StartTime: ok.StartTime, DurationMinutes: 60},
		{Title: "x", StartTime: "", DurationMinutes: 60},
		{Title: "x", StartTime: "not a time", DurationMinutes: 60},
		{Title: "x", StartTime: ok.StartTime, DurationMinutes: 0},
		{Title: "x", StartTime: ok.StartTime, DurationMinutes: 1441},
		{Title: strings.Repeat("字", 201), StartTime: ok.StartTime, DurationMinutes: 60},
		{Title: "x", StartTime: ok.StartTime, DurationMinutes: 60, Notes: strings.Repeat("a", 2001)},
	}
	for i, b := range bad {
		if _, err := b.Validate(); err == nil {
			t.Errorf("case %d should fail", i)
		}
	}
}

func TestEveryCategoryHasDefault(t *testing.T) {
	for _, c := range Categories {
		if DefaultDuration[c] <= 0 {
			t.Errorf("category %s has no default", c)
		}
	}
	if len(Categories) != len(DefaultDuration) {
		t.Error("Categories and DefaultDuration are out of sync")
	}
}
