package remind

import (
	"reflect"
	"testing"
	"time"

	"voiceplan/internal/plan"
)

var base = time.Date(2026, 10, 1, 10, 0, 0, 0, plan.Taipei)

func ex(id, contact int64, lead int, status string) Existing {
	return Existing{ID: id, ContactID: contact, LeadMinutes: lead, Status: status}
}

func ids(m []Move) []int64 {
	var out []int64
	for _, x := range m {
		out = append(out, x.ID)
	}
	return out
}

func TestUntickedRecipientsPendingDeliveryIsDeleted(t *testing.T) {
	existing := []Existing{ex(1, 10, 60, "pending"), ex(2, 11, 60, "pending")}
	p := Reconcile(existing, Desired{LeadMinutes: 60, ContactIDs: []int64{10}}, base.Add(5*time.Hour), base)
	if !reflect.DeepEqual(p.Delete, []int64{2}) || !reflect.DeepEqual(ids(p.Reschedule), []int64{1}) || len(p.Create) != 0 {
		t.Fatalf("%+v", p)
	}
}

func TestAddedRecipientGetsANewDelivery(t *testing.T) {
	existing := []Existing{ex(1, 10, 60, "pending")}
	start := base.Add(5 * time.Hour)
	p := Reconcile(existing, Desired{LeadMinutes: 60, ContactIDs: []int64{10, 11}}, start, base)
	if len(p.Create) != 1 || p.Create[0].ContactID != 11 || !p.Create[0].Due.Equal(start.Add(-time.Hour)) || p.Create[0].Late {
		t.Fatalf("%+v", p)
	}
}

func TestSentRemindersAreNeverRepeatedOrTouched(t *testing.T) {
	existing := []Existing{ex(1, 10, 60, "sent")}
	p := Reconcile(existing, Desired{LeadMinutes: 60, ContactIDs: []int64{10}}, base.Add(5*time.Hour), base)
	if len(p.Create)+len(p.Delete)+len(p.Reschedule)+len(p.Revive) != 0 || p.AlreadySent != 1 {
		t.Fatalf("a sent reminder must stay as it is: %+v", p)
	}
	// Even when the event moved and the person is still wanted, no second message.
	p = Reconcile(existing, Desired{LeadMinutes: 60, ContactIDs: []int64{10}}, base.Add(9*time.Hour), base)
	if len(p.Create) != 0 {
		t.Fatalf("%+v", p)
	}
	// Unticking someone who already got it deletes nothing (history stays).
	p = Reconcile(existing, Desired{LeadMinutes: 60, ContactIDs: nil}, base.Add(5*time.Hour), base)
	if len(p.Delete) != 0 {
		t.Fatalf("%+v", p)
	}
}

func TestChangingLeadTimeReplacesPendingInsteadOfDuplicating(t *testing.T) {
	existing := []Existing{ex(1, 10, 60, "pending")}
	start := base.Add(5 * time.Hour)
	p := Reconcile(existing, Desired{LeadMinutes: 1440, ContactIDs: []int64{10}}, start, base)
	if !reflect.DeepEqual(p.Delete, []int64{1}) || len(p.Create) != 1 {
		t.Fatalf("old 1-hour reminder must go and a 1-day one be created: %+v", p)
	}
	// 1 day before a start 5 hours away is already past: send now, flagged late.
	if !p.Create[0].Late || !p.Create[0].Due.Equal(base) {
		t.Fatalf("%+v", p.Create[0])
	}
}

func TestChangingLeadTimeAfterSendingStillRemindsAgainAtTheNewTime(t *testing.T) {
	// The 60-minute reminder was sent; the user now asks for a 30-minute one.
	existing := []Existing{ex(1, 10, 60, "sent")}
	p := Reconcile(existing, Desired{LeadMinutes: 30, ContactIDs: []int64{10}}, base.Add(5*time.Hour), base)
	if len(p.Create) != 1 || p.AlreadySent != 0 {
		t.Fatalf("a different lead time is a new reminder: %+v", p)
	}
}

func TestMovedEventReschedulesPendingToTheNewTime(t *testing.T) {
	existing := []Existing{ex(1, 10, 60, "pending")}
	newStart := base.Add(8 * time.Hour)
	p := Reconcile(existing, Desired{LeadMinutes: 60, ContactIDs: []int64{10}}, newStart, base)
	if len(p.Reschedule) != 1 || !p.Reschedule[0].Due.Equal(newStart.Add(-time.Hour)) {
		t.Fatalf("%+v", p)
	}
}

func TestMovedCloserTurnsReminderIntoSendNowAndLate(t *testing.T) {
	existing := []Existing{ex(1, 10, 60, "pending")}
	p := Reconcile(existing, Desired{LeadMinutes: 60, ContactIDs: []int64{10}}, base.Add(20*time.Minute), base)
	if len(p.Reschedule) != 1 || !p.Reschedule[0].Late || !p.Reschedule[0].Due.Equal(base) {
		t.Fatalf("%+v", p)
	}
}

func TestStartedEventSchedulesNothingAndSkipsWhatIsPending(t *testing.T) {
	existing := []Existing{ex(1, 10, 60, "pending")}
	p := Reconcile(existing, Desired{LeadMinutes: 60, ContactIDs: []int64{10, 11}}, base.Add(-time.Minute), base)
	if !p.Started || !reflect.DeepEqual(p.Skip, []int64{1}) || len(p.Create) != 0 || len(p.Reschedule) != 0 {
		t.Fatalf("%+v", p)
	}
}

func TestRemovingAllRemindersClearsPendingOnly(t *testing.T) {
	existing := []Existing{ex(1, 10, 60, "pending"), ex(2, 11, 60, "sent"), ex(3, 12, 60, "failed")}
	for _, want := range []Desired{{}, {LeadMinutes: 60}, {LeadMinutes: 0, ContactIDs: []int64{10}}} {
		p := Reconcile(existing, want, base.Add(5*time.Hour), base)
		if !reflect.DeepEqual(p.Delete, []int64{1}) || len(p.Create)+len(p.Reschedule)+len(p.Revive) != 0 {
			t.Fatalf("%+v -> %+v", want, p)
		}
	}
}

func TestFailedOrSkippedGetAnotherChanceWhenStillWanted(t *testing.T) {
	existing := []Existing{ex(1, 10, 60, "failed"), ex(2, 11, 60, "skipped"), ex(3, 12, 60, "failed")}
	p := Reconcile(existing, Desired{LeadMinutes: 60, ContactIDs: []int64{10, 11}}, base.Add(5*time.Hour), base)
	if !reflect.DeepEqual(ids(p.Revive), []int64{1, 2}) || len(p.Create) != 0 {
		t.Fatalf("%+v", p)
	}
	// If the event is already over, reviving would be pointless.
	p = Reconcile(existing, Desired{LeadMinutes: 60, ContactIDs: []int64{10}}, base.Add(-time.Hour), base)
	if len(p.Revive) != 0 {
		t.Fatalf("%+v", p)
	}
}

func TestInFlightAndDuplicateIDsAreHandled(t *testing.T) {
	existing := []Existing{ex(1, 10, 60, "sending")}
	p := Reconcile(existing, Desired{LeadMinutes: 60, ContactIDs: []int64{11, 11, 11}}, base.Add(5*time.Hour), base)
	if len(p.Delete) != 0 || len(p.Create) != 1 {
		t.Fatalf("a delivery being sent is left alone, and a repeated id creates one row: %+v", p)
	}
}

func TestFreshReminderOnAnEventThatHadNone(t *testing.T) {
	p := Reconcile(nil, Desired{LeadMinutes: 120, ContactIDs: []int64{10, 11}}, base.Add(6*time.Hour), base)
	if len(p.Create) != 2 || p.Started {
		t.Fatalf("%+v", p)
	}
}
