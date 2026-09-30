// Package api implements the JSON endpoints for parsing, confirming and
// listing plans. It depends only on interfaces so it can be tested offline.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"voiceplan/internal/gcal"
	"voiceplan/internal/line"
	"voiceplan/internal/plan"
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
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"` // pending | active | blocked
	IsSelf bool   `json:"is_self"`
}

type Contacts interface {
	List(ctx context.Context, userID int64) ([]Contact, error)
	EnsureSelf(ctx context.Context, userID int64) (Contact, error)
	CreateBindingCode(ctx context.Context, contactID int64, codeHash []byte, expires time.Time) error
}

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

const bindingCodeTTL = 10 * time.Minute

type Server struct {
	Contacts    Contacts
	Reminders   Reminders
	Tips        TipsGenerator
	Parser      Parser
	Transcriber Transcriber
	Calendar    Calendar
	Events      Events
	UserID      func(ctx context.Context) (int64, bool)
	Now         func() time.Time
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
	mux.Handle("GET /api/contacts", h(s.listContacts))
	mux.Handle("POST /api/line/bind", h(s.startBinding))
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
	resp.Reminder, resp.ReminderLate = s.scheduleReminder(r.Context(), uid, row.ID, ev, in.Reminder)
	writeJSON(w, http.StatusCreated, resp)
}

// scheduleReminder never fails the confirmation: the calendar event already
// exists, so a reminder problem is reported to the client instead.
func (s *Server) scheduleReminder(ctx context.Context, uid, eventID int64, ev plan.Event, in *ReminderInput) (string, bool) {
	if in == nil || in.LeadMinutes <= 0 || len(in.ContactIDs) == 0 || s.Reminders == nil {
		return "none", false
	}
	if in.LeadMinutes > 7*24*60 || len(in.ContactIDs) > 20 {
		return "failed", false
	}
	now := s.now()
	tips := ""
	if s.Tips != nil && now.Before(ev.Start) {
		tctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
		defer cancel()
		t, err := s.Tips.Tips(tctx, ev, now)
		if err != nil {
			log.Printf("tips failed (sending reminder without): %v", err)
		} else {
			tips = t
		}
	}
	res, err := s.Reminders.Schedule(context.WithoutCancel(ctx), ScheduleReq{
		UserID: uid, EventID: eventID, Start: ev.Start, LeadMinutes: in.LeadMinutes,
		ContactIDs: in.ContactIDs, Tips: tips, Now: now,
	})
	if err != nil {
		log.Printf("schedule reminder: %v", err)
		return "failed", false
	}
	return res.Status, res.Late
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
