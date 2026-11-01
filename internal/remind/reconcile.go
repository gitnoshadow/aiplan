package remind

import "time"

// Existing is a delivery row that already exists for the event being edited.
type Existing struct {
	ID          int64
	ContactID   int64
	LeadMinutes int
	Status      string // pending | sending | sent | failed | skipped
}

// Desired is what the user wants after the edit: one lead time and a set of
// recipients. LeadMinutes == 0 or no ContactIDs means "no reminders".
type Desired struct {
	LeadMinutes int
	ContactIDs  []int64 // already filtered to people who can receive messages
}

// Move says where one existing delivery should go.
type Move struct {
	ID   int64
	Due  time.Time
	Late bool
}

// NewDelivery is a delivery that does not exist yet.
type NewDelivery struct {
	ContactID int64
	Due       time.Time
	Late      bool
}

// Plan is the set of changes that turns the existing deliveries into the desired ones.
type Plan struct {
	Delete      []int64       // pending deliveries nobody wants any more
	Reschedule  []Move        // pending deliveries kept, moved to the new time
	Skip        []int64       // pending deliveries kept but the event has already started
	Revive      []Move        // failed/skipped deliveries for a recipient who is wanted again
	Create      []NewDelivery // recipients with no delivery yet
	AlreadySent int           // wanted recipients who already got this reminder (never resent)
	Started     bool          // the (new) start time has passed: nothing can be scheduled
}

// Reconcile compares what exists with what the user now wants.
//
// Rules, in the order they matter:
//   - A delivery that was SENT is never touched or repeated.
//   - A pending delivery is deleted if its recipient was unticked or its lead
//     time differs from the new one (it is replaced, not duplicated).
//   - A pending delivery that stays is moved to the new reminder time.
//   - A failed/skipped delivery for a recipient who is wanted gets another
//     chance (e.g. the event was moved to the future, or quota came back).
//   - A wanted recipient with no delivery gets a new one.
//   - If the event has already started, nothing is scheduled and what is still
//     pending is skipped.
func Reconcile(existing []Existing, want Desired, newStart, now time.Time) Plan {
	var p Plan
	wanted := map[int64]bool{}
	if want.LeadMinutes > 0 {
		for _, id := range want.ContactIDs {
			wanted[id] = true
		}
	}
	due, late, canSchedule := Schedule(newStart, want.LeadMinutes, now)
	p.Started = !now.Before(newStart)

	have := map[int64]bool{} // wanted recipients that already have a row for the desired lead time
	for _, e := range existing {
		sameKey := e.LeadMinutes == want.LeadMinutes && wanted[e.ContactID]
		switch e.Status {
		case "sent":
			if sameKey {
				have[e.ContactID] = true
				p.AlreadySent++
			}
		case "pending":
			switch {
			case !sameKey:
				p.Delete = append(p.Delete, e.ID)
			case !canSchedule:
				have[e.ContactID] = true
				p.Skip = append(p.Skip, e.ID)
			default:
				have[e.ContactID] = true
				p.Reschedule = append(p.Reschedule, Move{ID: e.ID, Due: due, Late: late})
			}
		case "failed", "skipped":
			if sameKey {
				have[e.ContactID] = true
				if canSchedule {
					p.Revive = append(p.Revive, Move{ID: e.ID, Due: due, Late: late})
				}
			}
		}
		// "sending" is in flight: leave it alone.
	}
	if canSchedule && want.LeadMinutes > 0 {
		for _, id := range want.ContactIDs {
			if !have[id] {
				p.Create = append(p.Create, NewDelivery{ContactID: id, Due: due, Late: late})
				have[id] = true // guards against duplicate ids in the request
			}
		}
	}
	return p
}
