// Package api implements the JSON endpoints for parsing, confirming and
// listing plans. It depends only on interfaces so it can be tested offline.
package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"voiceplan/internal/calsync"
	"voiceplan/internal/gcal"
	"voiceplan/internal/line"
	"voiceplan/internal/plan"
	"voiceplan/internal/remind"
	"voiceplan/internal/tips"
)

type Parser interface {
	ParsePlan(ctx context.Context, text string, now time.Time) (plan.Raw, error)
}
type Transcriber interface {
	Transcribe(ctx context.Context, audio []byte, mime string) (string, error)
}
type Calendar interface {
	InsertEvent(ctx context.Context, userID int64, ev plan.Event) (string, error)
	DeleteEvent(ctx context.Context, userID int64, eventID string) error
	UpdateEvent(ctx context.Context, userID int64, eventID string, ev plan.Event) error
	Status(ctx context.Context, userID int64) (connected, needsReauth bool, err error)
}

// EventRow is a stored event as returned to the client.
type EventRow struct {
	ID              int64     `json:"id"`
	Title           string    `json:"title"`
	StartTime       string    `json:"start_time"`
	DurationMinutes int       `json:"duration_minutes"`
	Location        string    `json:"location"`
	Notes           string    `json:"notes"`
	Category        string    `json:"category"`
	Status          string    `json:"status"`
	GoogleEventID   string    `json:"-"`
	StartUTC        time.Time `json:"-"`
	// Reminders says who will be (or was) notified and how long before. Only
	// filled in by List, for the "upcoming" screen.
	Reminders []ReminderGroup `json:"reminders,omitempty"`
}

// Recipient is one person of a reminder and how that delivery is going.
type Recipient struct {
	ContactID int64  `json:"contact_id"`
	Name      string `json:"name"`
	Status    string `json:"status"`           // pending | sent | failed | skipped
	Reason    string `json:"reason,omitempty"` // why it failed or was skipped
}

// ReminderGroup is all recipients that share one lead time.
type ReminderGroup struct {
	LeadMinutes int         `json:"lead_minutes"`
	Late        bool        `json:"late,omitempty"` // the lead time had already passed when it was scheduled
	Recipients  []Recipient `json:"recipients"`
}

// DeliveryInfo is one delivery row as read from the database.
type DeliveryInfo struct {
	EventID     int64
	LeadMinutes int
	Late        bool
	ContactID   int64
	Name        string
	Status      string
	Reason      string
}

// GroupDeliveries folds delivery rows into per-event reminder groups. Rows must
// already be ordered (event, lead time, owner first); that order is preserved.
func GroupDeliveries(ds []DeliveryInfo) map[int64][]ReminderGroup {
	out := map[int64][]ReminderGroup{}
	for _, d := range ds {
		gs := out[d.EventID]
		if n := len(gs); n == 0 || gs[n-1].LeadMinutes != d.LeadMinutes {
			gs = append(gs, ReminderGroup{LeadMinutes: d.LeadMinutes})
		}
		g := &gs[len(gs)-1]
		g.Late = g.Late || d.Late
		status := d.Status
		if status == "sending" { // in flight: to the user it is still waiting
			status = "pending"
		}
		r := Recipient{ContactID: d.ContactID, Name: d.Name, Status: status}
		if status == "failed" || status == "skipped" {
			r.Reason = d.Reason
		}
		g.Recipients = append(g.Recipients, r)
		out[d.EventID] = gs
	}
	return out
}

type Events interface {
	// Reserve inserts a pending row; created=false returns the existing row for the same request_id.
	Reserve(ctx context.Context, userID int64, requestID string, ev plan.Event) (row EventRow, created bool, err error)
	Complete(ctx context.Context, id int64, googleEventID string) error
	Abandon(ctx context.Context, id int64) error
	List(ctx context.Context, userID int64, from time.Time, limit int) ([]EventRow, error)
	Get(ctx context.Context, userID, id int64) (EventRow, error)
	Delete(ctx context.Context, userID, id int64) error
}

// Contact is a person who can receive LINE reminders.
type Contact struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Status  string `json:"status"` // pending | active | blocked ("blocked" is shown as invalid)
	IsSelf  bool   `json:"is_self"`
	Enabled bool   `json:"enabled"`
	// LineUserID is LINE's opaque id for this person (shown to the owner only,
	// and only once the contact is bound). Empty until then.
	LineUserID string `json:"line_user_id,omitempty"`
}

var ErrNotFound = errors.New("not found")

type Contacts interface {
	List(ctx context.Context, userID int64) ([]Contact, error)
	EnsureSelf(ctx context.Context, userID int64) (Contact, error)
	Get(ctx context.Context, userID, id int64) (Contact, error) // ErrNotFound if not the user's
	Create(ctx context.Context, userID int64, name string) (Contact, error)
	Update(ctx context.Context, userID, id int64, name *string, enabled *bool) error
	// Delete removes the contact together with its LINE ID, binding codes and delivery records.
	Delete(ctx context.Context, userID, id int64) error
	CreateBindingCode(ctx context.Context, contactID int64, codeHash []byte, expires time.Time) error
}

type Group struct {
	ID        int64   `json:"id"`
	Name      string  `json:"name"`
	MemberIDs []int64 `json:"member_ids"`
}

// Groups are only a shortcut for ticking several recipients; unrelated to LINE groups.
type Groups interface {
	List(ctx context.Context, userID int64) ([]Group, error)
	Save(ctx context.Context, userID int64, id int64, name string, memberIDs []int64) (Group, error) // id 0 = create
	Delete(ctx context.Context, userID, id int64) error
}

// Usage reports messages pushed in a Taipei month ("2026-10").
type Usage interface {
	Month(ctx context.Context, month string) (int, error)
}

const (
	maxContacts = 30
	maxGroups   = 20
)

type ScheduleReq struct {
	UserID      int64
	EventID     int64
	Start       time.Time
	LeadMinutes int
	ContactIDs  []int64
	Tips        string
	Now         time.Time
}

type ScheduleResult struct {
	Status     string // scheduled | none | event_started
	Recipients int
	Late       bool
}

type Reminders interface {
	Schedule(ctx context.Context, q ScheduleReq) (ScheduleResult, error)
}

type TipsGenerator interface {
	Tips(ctx context.Context, ev plan.Event, now time.Time) (string, error)
}

// Sync is the Google Calendar change sync (implemented by calsync.Syncer).
type Sync interface {
	Status(ctx context.Context, userID int64) (calsync.Status, error)
	SyncUser(ctx context.Context, userID int64) (calsync.Stats, error)
}

// ReminderPlan is the reminder the user wants after an edit.
type ReminderPlan struct {
	LeadMinutes int
	ContactIDs  []int64
}

// EditReq is a full edit: new event content and the complete desired reminder.
type EditReq struct {
	UserID, EventID int64
	Event           plan.Event
	Reminder        *ReminderPlan // nil = no reminders
	Tips            *string       // nil = keep the stored advice
	Now             time.Time
}

type EditResult struct {
	Status      string // scheduled | none | cleared | event_started
	Recipients  int
	Late        bool
	AlreadySent int // wanted recipients who already got this reminder (not resent)
}

// Editor applies an edit to the event row and its reminders atomically.
type Editor interface {
	Apply(ctx context.Context, req EditReq) (EditResult, error)
}

// LogEntry is one line of the delivery log: one message to one person.
type LogEntry struct {
	ID          int64     `json:"id"`
	EventID     int64     `json:"event_id"`
	EventTitle  string    `json:"event_title"`
	EventStart  string    `json:"event_start"` // RFC 3339, Asia/Taipei
	ContactName string    `json:"contact_name"`
	LeadMinutes int       `json:"lead_minutes"`
	Status      string    `json:"status"` // pending | sent | failed | skipped
	Reason      string    `json:"reason,omitempty"`
	Attempts    int       `json:"attempts"`
	Late        bool      `json:"late,omitempty"`
	DueAt       string    `json:"due_at"`
	SentAt      *string   `json:"sent_at"`
	DueUTC      time.Time `json:"-"` // for the paging cursor
}

// LogQuery asks for one page, newest first. Statuses empty = all.
type LogQuery struct {
	Statuses  []string
	Limit     int
	BeforeDue *time.Time // keyset paging: only rows strictly before (BeforeDue, BeforeID)
	BeforeID  int64
}

type DeliveryLog interface {
	List(ctx context.Context, userID int64, q LogQuery) ([]LogEntry, error)
}

// logFilters maps the filter names the UI uses to delivery statuses.
// "pending" includes deliveries currently being sent.
var logFilters = map[string][]string{
	"all":     nil,
	"sent":    {"sent"},
	"failed":  {"failed"},
	"skipped": {"skipped"},
	"pending": {"pending", "sending"},
}

const (
	logDefaultLimit = 30
	logMaxLimit     = 100
)

// EncodeCursor / DecodeCursor make an opaque "continue after this row" token.
func EncodeCursor(due time.Time, id int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("%d:%d", due.UTC().UnixMicro(), id)))
}

func DecodeCursor(s string) (time.Time, int64, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return time.Time{}, 0, errors.New("bad cursor")
	}
	var micro, id int64
	if n, err := fmt.Sscanf(string(raw), "%d:%d", &micro, &id); err != nil || n != 2 || id <= 0 {
		return time.Time{}, 0, errors.New("bad cursor")
	}
	return time.UnixMicro(micro).UTC(), id, nil
}

const bindingCodeTTL = 10 * time.Minute

type Server struct {
	Log          DeliveryLog
	Editor       Editor
	Sync         Sync
	Groups       Groups
	Usage        Usage
	MonthlyLimit int
	AddFriendURL string
	Contacts     Contacts
	Reminders    Reminders
	Tips         TipsGenerator
	Parser       Parser
	Transcriber  Transcriber
	Calendar     Calendar
	Events       Events
	UserID       func(ctx context.Context) (int64, bool)
	Now          func() time.Time
}

// Register mounts routes; wrap is the auth middleware.
func (s *Server) Register(mux *http.ServeMux, wrap func(http.Handler) http.Handler) {
	h := func(f http.HandlerFunc) http.Handler { return wrap(f) }
	mux.Handle("GET /api/google/status", h(s.googleStatus))
	mux.Handle("POST /api/plans/parse", h(s.parse))
	mux.Handle("POST /api/plans/confirm", h(s.confirm))
	mux.Handle("POST /api/transcribe", h(s.transcribe))
	mux.Handle("GET /api/events", h(s.listEvents))
	mux.Handle("DELETE /api/events/{id}", h(s.deleteEvent))
	mux.Handle("PATCH /api/events/{id}", h(s.updateEvent))
	mux.Handle("GET /api/deliveries", h(s.deliveryLog))
	mux.Handle("GET /api/contacts", h(s.listContacts))
	mux.Handle("POST /api/contacts", h(s.createContact))
	mux.Handle("PATCH /api/contacts/{id}", h(s.updateContact))
	mux.Handle("DELETE /api/contacts/{id}", h(s.deleteContact))
	mux.Handle("POST /api/contacts/{id}/invite", h(s.invite))
	mux.Handle("POST /api/line/bind", h(s.startBinding))
	mux.Handle("GET /api/groups", h(s.listGroups))
	mux.Handle("POST /api/groups", h(s.saveGroup))
	mux.Handle("PUT /api/groups/{id}", h(s.saveGroup))
	mux.Handle("DELETE /api/groups/{id}", h(s.deleteGroup))
	mux.Handle("GET /api/line/usage", h(s.usage))
	mux.Handle("GET /api/sync/status", h(s.syncStatus))
	mux.Handle("POST /api/sync/now", h(s.syncNow))
}

func (s *Server) syncStatus(w http.ResponseWriter, r *http.Request) {
	uid, ok := s.user(w, r)
	if !ok {
		return
	}
	st, err := s.Sync.Status(r.Context(), uid)
	if err != nil {
		log.Printf("sync status: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	writeJSON(w, 200, st)
}

// syncNow runs a sync immediately so the user does not have to wait for the timer.
func (s *Server) syncNow(w http.ResponseWriter, r *http.Request) {
	uid, ok := s.user(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	stats, err := s.Sync.SyncUser(ctx, uid)
	switch {
	case errors.Is(err, gcal.ErrReauthRequired):
		writeErr(w, http.StatusConflict, "reauth_required")
		return
	case errors.Is(err, gcal.ErrNoCredentials):
		writeErr(w, http.StatusConflict, "calendar_not_connected")
		return
	case err != nil:
		log.Printf("sync now: %v", err)
		writeErr(w, http.StatusBadGateway, "sync_failed")
		return
	}
	st, _ := s.Sync.Status(r.Context(), uid)
	writeJSON(w, 200, map[string]any{
		"updated": stats.Updated, "cancelled": stats.Cancelled + stats.Reconciled, "status": st,
	})
}

func pathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil && id > 0
}

func validName(s string, max int) (string, bool) {
	s = strings.TrimSpace(s)
	n := utf8.RuneCountInString(s)
	return s, n >= 1 && n <= max
}

func (s *Server) createContact(w http.ResponseWriter, r *http.Request) {
	uid, ok := s.user(w, r)
	if !ok {
		return
	}
	var in struct {
		Name string `json:"name"`
	}
	if !decode(w, r, 4<<10, &in) {
		return
	}
	name, ok := validName(in.Name, 30)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid_name")
		return
	}
	existing, err := s.Contacts.List(r.Context(), uid)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	if len(existing) >= maxContacts {
		writeErr(w, http.StatusConflict, "too_many_contacts")
		return
	}
	c, err := s.Contacts.Create(r.Context(), uid, name)
	if err != nil {
		log.Printf("create contact: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

func (s *Server) updateContact(w http.ResponseWriter, r *http.Request) {
	uid, ok := s.user(w, r)
	if !ok {
		return
	}
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid_id")
		return
	}
	var in struct {
		Name    *string `json:"name"`
		Enabled *bool   `json:"enabled"`
	}
	if !decode(w, r, 4<<10, &in) {
		return
	}
	if in.Name != nil {
		n, ok := validName(*in.Name, 30)
		if !ok {
			writeErr(w, http.StatusBadRequest, "invalid_name")
			return
		}
		in.Name = &n
	}
	if err := s.Contacts.Update(r.Context(), uid, id, in.Name, in.Enabled); err != nil {
		if errors.Is(err, ErrNotFound) {
			writeErr(w, http.StatusNotFound, "not_found")
			return
		}
		log.Printf("update contact: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteContact(w http.ResponseWriter, r *http.Request) {
	uid, ok := s.user(w, r)
	if !ok {
		return
	}
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid_id")
		return
	}
	c, err := s.Contacts.Get(r.Context(), uid, id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found")
		return
	}
	if c.IsSelf {
		writeErr(w, http.StatusConflict, "cannot_delete_self")
		return
	}
	if err := s.Contacts.Delete(r.Context(), uid, id); err != nil {
		log.Printf("delete contact: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// invite issues a one-time binding code for a contact and a ready-to-send message.
func (s *Server) invite(w http.ResponseWriter, r *http.Request) {
	uid, ok := s.user(w, r)
	if !ok {
		return
	}
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid_id")
		return
	}
	c, err := s.Contacts.Get(r.Context(), uid, id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found")
		return
	}
	code, hash, err := line.NewBindingCode()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	if err := s.Contacts.CreateBindingCode(r.Context(), c.ID, hash, s.now().Add(bindingCodeTTL)); err != nil {
		log.Printf("create binding code: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	mins := int(bindingCodeTTL.Minutes())
	writeJSON(w, 200, map[string]any{
		"code": code, "expires_in_minutes": mins, "text": InviteText(s.AddFriendURL, code, mins),
	})
}

// InviteText is what the owner forwards to a family member.
func InviteText(addFriendURL, code string, mins int) string {
	var b strings.Builder
	b.WriteString("嗨,我想用 LINE 在行程前提醒你。請照下面兩步完成設定:\n")
	if addFriendURL != "" {
		fmt.Fprintf(&b, "1. 點這個連結,把官方帳號加為好友:%s\n", addFriendURL)
	} else {
		b.WriteString("1. 把我的 LINE 官方帳號加為好友\n")
	}
	fmt.Fprintf(&b, "2. 對它傳送這組綁定碼:%s(%d 分鐘內有效,只能用一次)", code, mins)
	return b.String()
}

func (s *Server) listGroups(w http.ResponseWriter, r *http.Request) {
	uid, ok := s.user(w, r)
	if !ok {
		return
	}
	gs, err := s.Groups.List(r.Context(), uid)
	if err != nil {
		log.Printf("list groups: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	if gs == nil {
		gs = []Group{}
	}
	writeJSON(w, 200, gs)
}

func (s *Server) saveGroup(w http.ResponseWriter, r *http.Request) {
	uid, ok := s.user(w, r)
	if !ok {
		return
	}
	var id int64
	if r.Method == http.MethodPut {
		if id, ok = pathID(r); !ok {
			writeErr(w, http.StatusBadRequest, "invalid_id")
			return
		}
	}
	var in struct {
		Name      string  `json:"name"`
		MemberIDs []int64 `json:"member_ids"`
	}
	if !decode(w, r, 8<<10, &in) {
		return
	}
	name, ok := validName(in.Name, 30)
	if !ok || len(in.MemberIDs) > maxContacts {
		writeErr(w, http.StatusBadRequest, "invalid_group")
		return
	}
	if id == 0 {
		existing, err := s.Groups.List(r.Context(), uid)
		if err == nil && len(existing) >= maxGroups {
			writeErr(w, http.StatusConflict, "too_many_groups")
			return
		}
	}
	g, err := s.Groups.Save(r.Context(), uid, id, name, in.MemberIDs)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			writeErr(w, http.StatusNotFound, "not_found")
			return
		}
		log.Printf("save group: %v", err)
		writeErr(w, http.StatusConflict, "group_failed") // most likely a duplicate name
		return
	}
	writeJSON(w, 200, g)
}

func (s *Server) deleteGroup(w http.ResponseWriter, r *http.Request) {
	uid, ok := s.user(w, r)
	if !ok {
		return
	}
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid_id")
		return
	}
	if err := s.Groups.Delete(r.Context(), uid, id); err != nil {
		log.Printf("delete group: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) usage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.user(w, r); !ok {
		return
	}
	month := s.now().In(plan.Taipei).Format("2006-01")
	sent, err := s.Usage.Month(r.Context(), month)
	if err != nil {
		log.Printf("usage: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	writeJSON(w, 200, map[string]any{
		"month": month, "sent": sent, "limit": s.MonthlyLimit, "level": remind.QuotaLevel(sent, s.MonthlyLimit),
	})
}

func (s *Server) listContacts(w http.ResponseWriter, r *http.Request) {
	uid, ok := s.user(w, r)
	if !ok {
		return
	}
	cs, err := s.Contacts.List(r.Context(), uid)
	if err != nil {
		log.Printf("list contacts: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	if cs == nil {
		cs = []Contact{}
	}
	writeJSON(w, 200, cs)
}

// startBinding issues a one-time code for the user's own LINE account. Only the
// hash is stored; the code is shown once.
func (s *Server) startBinding(w http.ResponseWriter, r *http.Request) {
	uid, ok := s.user(w, r)
	if !ok {
		return
	}
	self, err := s.Contacts.EnsureSelf(r.Context(), uid)
	if err != nil {
		log.Printf("ensure self: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	code, hash, err := line.NewBindingCode()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	if err := s.Contacts.CreateBindingCode(r.Context(), self.ID, hash, s.now().Add(bindingCodeTTL)); err != nil {
		log.Printf("create binding code: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	writeJSON(w, 200, map[string]any{"code": code, "expires_in_minutes": int(bindingCodeTTL.Minutes())})
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func (s *Server) user(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, ok := s.UserID(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
	}
	return id, ok
}

func decode(w http.ResponseWriter, r *http.Request, max int64, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, max)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request")
		return false
	}
	return true
}

func (s *Server) googleStatus(w http.ResponseWriter, r *http.Request) {
	uid, ok := s.user(w, r)
	if !ok {
		return
	}
	connected, needs, err := s.Calendar.Status(r.Context(), uid)
	if err != nil {
		log.Printf("google status: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	writeJSON(w, 200, map[string]bool{"connected": connected, "needs_reauth": needs})
}

func (s *Server) parse(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.user(w, r); !ok {
		return
	}
	var in struct {
		Text string `json:"text"`
	}
	if !decode(w, r, 16<<10, &in) {
		return
	}
	text := strings.TrimSpace(in.Text)
	if n := utf8.RuneCountInString(text); n == 0 || n > 1000 {
		writeErr(w, http.StatusBadRequest, "text_length")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 55*time.Second)
	defer cancel()
	raw, err := s.Parser.ParsePlan(ctx, text, s.now())
	if err != nil {
		log.Printf("parse failed: %v", err) // error text carries no user content
		writeErr(w, llmStatus(err), llmCode(err, "parse_failed"))
		return
	}
	writeJSON(w, 200, plan.Normalize(raw, s.now()))
}

func (s *Server) transcribe(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.user(w, r); !ok {
		return
	}
	ct := strings.ToLower(strings.TrimSpace(strings.SplitN(r.Header.Get("Content-Type"), ";", 2)[0]))
	if ct != "audio/wav" && ct != "audio/x-wav" {
		writeErr(w, http.StatusUnsupportedMediaType, "unsupported_audio")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
	audio, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusRequestEntityTooLarge, "audio_too_large")
		return
	}
	if len(audio) < 1000 {
		writeErr(w, http.StatusBadRequest, "audio_too_short")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 55*time.Second)
	defer cancel()
	text, err := s.Transcriber.Transcribe(ctx, audio, "audio/wav")
	if err != nil {
		log.Printf("transcribe failed: %v", err)
		writeErr(w, llmStatus(err), llmCode(err, "transcribe_failed"))
		return
	}
	writeJSON(w, 200, map[string]string{"text": text})
}

// Busyer is implemented by errors that mean "temporarily overloaded".
type Busyer interface{ Busy() bool }

func isBusy(err error) bool {
	var b Busyer
	return errors.As(err, &b) && b.Busy()
}

func llmStatus(err error) int {
	if isBusy(err) {
		return http.StatusServiceUnavailable
	}
	return http.StatusBadGateway
}

func llmCode(err error, fallback string) string {
	if isBusy(err) {
		return "llm_busy"
	}
	return fallback
}

// ReminderInput is optional: no lead time or no recipients means no reminder.
type ReminderInput struct {
	LeadMinutes int     `json:"lead_minutes"`
	ContactIDs  []int64 `json:"contact_ids"`
}

type confirmReq struct {
	RequestID string `json:"request_id"`
	plan.ConfirmInput
	Reminder *ReminderInput `json:"reminder"`
}

type confirmResp struct {
	EventRow
	// scheduled | none | event_started | failed
	Reminder     string `json:"reminder,omitempty"`
	ReminderLate bool   `json:"reminder_late,omitempty"`
	// Recipients is how many LINE messages this reminder will consume.
	Recipients int `json:"reminder_recipients,omitempty"`
}

// confirm is the only path that writes to Google Calendar, and only for an
// explicit user confirmation.
func (s *Server) confirm(w http.ResponseWriter, r *http.Request) {
	uid, ok := s.user(w, r)
	if !ok {
		return
	}
	var in confirmReq
	if !decode(w, r, 32<<10, &in) {
		return
	}
	if n := len(in.RequestID); n < 8 || n > 64 {
		writeErr(w, http.StatusBadRequest, "request_id")
		return
	}
	ev, err := in.ConfirmInput.Validate()
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_plan")
		return
	}

	row, created, err := s.Events.Reserve(r.Context(), uid, in.RequestID, ev)
	if err != nil {
		log.Printf("reserve event: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	if !created { // double tap / retry: never create a second Google event
		if row.Status == "active" {
			writeJSON(w, http.StatusOK, row)
			return
		}
		writeErr(w, http.StatusConflict, "in_progress")
		return
	}

	gid, err := s.Calendar.InsertEvent(r.Context(), uid, ev)
	if err != nil {
		if aerr := s.Events.Abandon(context.WithoutCancel(r.Context()), row.ID); aerr != nil {
			log.Printf("abandon event %d: %v", row.ID, aerr)
		}
		switch {
		case errors.Is(err, gcal.ErrNoCredentials):
			writeErr(w, http.StatusConflict, "calendar_not_connected")
		case errors.Is(err, gcal.ErrReauthRequired):
			writeErr(w, http.StatusConflict, "reauth_required")
		default:
			log.Printf("insert event: %v", err)
			writeErr(w, http.StatusBadGateway, "calendar_failed")
		}
		return
	}
	if err := s.Events.Complete(context.WithoutCancel(r.Context()), row.ID, gid); err != nil {
		log.Printf("complete event %d: %v", row.ID, err)
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	row.Status = "active"

	resp := confirmResp{EventRow: row}
	resp.Reminder, resp.ReminderLate, resp.Recipients = s.scheduleReminder(r.Context(), uid, row.ID, ev, in.Reminder)
	writeJSON(w, http.StatusCreated, resp)
}

// scheduleReminder never fails the confirmation: the calendar event already
// exists, so a reminder problem is reported to the client instead.
func (s *Server) scheduleReminder(ctx context.Context, uid, eventID int64, ev plan.Event, in *ReminderInput) (string, bool, int) {
	if in == nil || in.LeadMinutes <= 0 || len(in.ContactIDs) == 0 || s.Reminders == nil {
		return "none", false, 0
	}
	if in.LeadMinutes > 7*24*60 || len(in.ContactIDs) > maxContacts {
		return "failed", false, 0
	}
	now := s.now()
	advice := ""
	if s.Tips != nil && now.Before(ev.Start) && !tips.Skip(ev.Category) {
		tctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
		defer cancel()
		t, err := s.Tips.Tips(tctx, ev, now)
		if err != nil {
			log.Printf("tips failed (sending reminder without): %v", err)
		} else {
			advice = t
		}
	}
	res, err := s.Reminders.Schedule(context.WithoutCancel(ctx), ScheduleReq{
		UserID: uid, EventID: eventID, Start: ev.Start, LeadMinutes: in.LeadMinutes,
		ContactIDs: in.ContactIDs, Tips: advice, Now: now,
	})
	if err != nil {
		log.Printf("schedule reminder: %v", err)
		return "failed", false, 0
	}
	return res.Status, res.Late, res.Recipients
}

func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) {
	uid, ok := s.user(w, r)
	if !ok {
		return
	}
	rows, err := s.Events.List(r.Context(), uid, s.now().Add(-2*time.Hour), 50)
	if err != nil {
		log.Printf("list events: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	if rows == nil {
		rows = []EventRow{}
	}
	writeJSON(w, 200, rows)
}

func (s *Server) deleteEvent(w http.ResponseWriter, r *http.Request) {
	uid, ok := s.user(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_id")
		return
	}
	row, err := s.Events.Get(r.Context(), uid, id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found")
		return
	}
	if row.GoogleEventID != "" {
		if err := s.Calendar.DeleteEvent(r.Context(), uid, row.GoogleEventID); err != nil {
			if errors.Is(err, gcal.ErrReauthRequired) {
				writeErr(w, http.StatusConflict, "reauth_required")
				return
			}
			log.Printf("delete google event: %v", err)
			writeErr(w, http.StatusBadGateway, "calendar_failed")
			return
		}
	}
	if err := s.Events.Delete(r.Context(), uid, id); err != nil {
		log.Printf("delete event row: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type updateReq struct {
	plan.ConfirmInput
	Reminder *ReminderInput `json:"reminder"`
}

// updateEvent edits an event and its reminders. Google is updated first: if it
// refuses, nothing local changes. If Google succeeds but the local write fails,
// the next sync copies Google's version back, so the two cannot stay apart.
func (s *Server) updateEvent(w http.ResponseWriter, r *http.Request) {
	uid, ok := s.user(w, r)
	if !ok {
		return
	}
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid_id")
		return
	}
	var in updateReq
	if !decode(w, r, 32<<10, &in) {
		return
	}
	ev, err := in.ConfirmInput.Validate()
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_plan")
		return
	}
	var want *ReminderPlan
	if in.Reminder != nil {
		if in.Reminder.LeadMinutes < 0 || in.Reminder.LeadMinutes > 7*24*60 || len(in.Reminder.ContactIDs) > maxContacts {
			writeErr(w, http.StatusBadRequest, "invalid_reminder")
			return
		}
		if in.Reminder.LeadMinutes > 0 && len(in.Reminder.ContactIDs) > 0 {
			want = &ReminderPlan{LeadMinutes: in.Reminder.LeadMinutes, ContactIDs: in.Reminder.ContactIDs}
		}
	}

	row, err := s.Events.Get(r.Context(), uid, id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found")
		return
	}
	if row.GoogleEventID != "" {
		if err := s.Calendar.UpdateEvent(r.Context(), uid, row.GoogleEventID, ev); err != nil {
			switch {
			case errors.Is(err, gcal.ErrReauthRequired):
				writeErr(w, http.StatusConflict, "reauth_required")
			case errors.Is(err, gcal.ErrNoCredentials):
				writeErr(w, http.StatusConflict, "calendar_not_connected")
			case errors.Is(err, gcal.ErrEventGone):
				writeErr(w, http.StatusConflict, "event_gone")
			default:
				log.Printf("update google event: %v", err)
				writeErr(w, http.StatusBadGateway, "calendar_failed")
			}
			return
		}
	}

	// Fresh advice for the edited event (it may have a new time, place or category).
	now := s.now()
	var advice *string
	if want != nil && s.Tips != nil && now.Before(ev.Start) && !tips.Skip(ev.Category) {
		tctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 20*time.Second)
		defer cancel()
		if t, err := s.Tips.Tips(tctx, ev, now); err != nil {
			log.Printf("tips failed (keeping the stored advice): %v", err)
		} else {
			advice = &t
		}
	}

	res, err := s.Editor.Apply(context.WithoutCancel(r.Context()), EditReq{
		UserID: uid, EventID: id, Event: ev, Reminder: want, Tips: advice, Now: now,
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			writeErr(w, http.StatusNotFound, "not_found")
			return
		}
		log.Printf("apply edit: %v", err)
		writeErr(w, http.StatusInternalServerError, "partial_update")
		return
	}
	writeJSON(w, 200, map[string]any{
		"reminder": res.Status, "reminder_late": res.Late,
		"reminder_recipients": res.Recipients, "reminder_already_sent": res.AlreadySent,
	})
}

// deliveryLog serves the "who was sent what, and did it work" history, newest
// first, a page at a time. Message text is never stored, only the outcome.
func (s *Server) deliveryLog(w http.ResponseWriter, r *http.Request) {
	uid, ok := s.user(w, r)
	if !ok {
		return
	}
	qs := r.URL.Query()
	filter := qs.Get("status")
	if filter == "" {
		filter = "all"
	}
	statuses, known := logFilters[filter]
	if !known {
		writeErr(w, http.StatusBadRequest, "invalid_status")
		return
	}
	limit := logDefaultLimit
	if v := qs.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeErr(w, http.StatusBadRequest, "invalid_limit")
			return
		}
		limit = min(n, logMaxLimit)
	}
	q := LogQuery{Statuses: statuses, Limit: limit + 1} // one extra row tells us whether another page exists
	if c := qs.Get("cursor"); c != "" {
		due, id, err := DecodeCursor(c)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_cursor")
			return
		}
		q.BeforeDue, q.BeforeID = &due, id
	}
	rows, err := s.Log.List(r.Context(), uid, q)
	if err != nil {
		log.Printf("delivery log: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	next := ""
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[len(rows)-1]
		next = EncodeCursor(last.DueUTC, last.ID)
	}
	if rows == nil {
		rows = []LogEntry{}
	}
	writeJSON(w, 200, map[string]any{"entries": rows, "next_cursor": next})
}
