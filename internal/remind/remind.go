// Package remind holds the pure reminder rules: when a reminder is due, what
// the message says and how failed sends are retried. No external dependencies.
package remind

import (
	"fmt"
	"strings"
	"time"

	"voiceplan/internal/plan"
)

// DefaultLeadMinutes is the default "remind me this long before" (1 hour).
const DefaultLeadMinutes = 60

// Schedule returns when a reminder should be sent. If the reminder time has
// already passed but the event has not started, it is due immediately and
// flagged late. ok=false means the event already started: nothing to send.
func Schedule(start time.Time, leadMinutes int, now time.Time) (due time.Time, late bool, ok bool) {
	if !now.Before(start) {
		return time.Time{}, false, false
	}
	due = start.Add(-time.Duration(leadMinutes) * time.Minute)
	if !due.After(now) {
		return now, true, true
	}
	return due, false, true
}

// Retry policy: 1 initial attempt + 3 retries with 1, 5 and 15 minute backoff.
var backoff = []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute}

const MaxAttempts = 4

// NextAttempt takes the number of attempts made so far (including the one that
// just failed) and returns when to try again; ok=false means give up.
func NextAttempt(attemptsMade int, now time.Time) (time.Time, bool) {
	if attemptsMade < 1 || attemptsMade >= MaxAttempts {
		return time.Time{}, false
	}
	return now.Add(backoff[attemptsMade-1]), true
}

// Message is the data behind one reminder.
type Message struct {
	Title    string
	Location string
	Notes    string
	Tips     string // pre-generated pre-event advice; may be empty
	Start    time.Time
	Late     bool // the reminder time had already passed when it was scheduled
}

var weekday = [...]string{"日", "一", "二", "三", "四", "五", "六"}

// Format builds the LINE text. One message per person keeps quota use minimal.
func Format(m Message, now time.Time) string {
	start := m.Start.In(plan.Taipei)
	var b strings.Builder
	fmt.Fprintf(&b, "⏰ 提醒:%s\n", m.Title)
	fmt.Fprintf(&b, "🕒 %d/%d(%s) %s", start.Month(), start.Day(), weekday[start.Weekday()], start.Format("15:04"))
	if left := m.Start.Sub(now); left > 0 {
		switch {
		case left < 90*time.Minute:
			fmt.Fprintf(&b, "(約 %d 分鐘後開始)", int((left+time.Minute-1)/time.Minute))
		case left < 36*time.Hour:
			fmt.Fprintf(&b, "(約 %d 小時後開始)", int((left+30*time.Minute)/time.Hour))
		}
	}
	b.WriteString("\n")
	if m.Location != "" {
		fmt.Fprintf(&b, "📍 %s\n", m.Location)
	}
	if m.Notes != "" {
		fmt.Fprintf(&b, "📝 %s\n", m.Notes)
	}
	if m.Tips != "" {
		fmt.Fprintf(&b, "\n行前注意事項\n%s\n", m.Tips)
	}
	if m.Late {
		b.WriteString("\n(建立行程時提醒時間已過,所以現在才通知)")
	}
	return strings.TrimRight(b.String(), "\n")
}
