package auth

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"voiceplan/internal/authcore"
	"voiceplan/internal/config"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

const calendarScope = "https://www.googleapis.com/auth/calendar.events"

// CalendarConnector stores the refresh token from the incremental calendar
// authorisation (implemented by gcal.Service).
type CalendarConnector interface {
	Connect(ctx context.Context, userID int64, refreshToken string) error
}

type Handler struct {
	cfg      *config.Config
	store    *Store
	oauth    *oauth2.Config
	calOAuth *oauth2.Config
	verifier *oidc.IDTokenVerifier
	cal      CalendarConnector
}

func NewHandler(ctx context.Context, cfg *config.Config, store *Store, cal CalendarConnector) (*Handler, error) {
	provider, err := oidc.NewProvider(ctx, "https://accounts.google.com")
	if err != nil {
		return nil, err
	}
	login := &oauth2.Config{
		ClientID:     cfg.GoogleClientID,
		ClientSecret: cfg.GoogleClientSecret,
		RedirectURL:  cfg.OAuthRedirectURL,
		Endpoint:     google.Endpoint,
		// Login asks for identity only; calendar access is requested separately.
		Scopes: []string{oidc.ScopeOpenID, "email"},
	}
	calCfg := *login
	calCfg.Scopes = []string{oidc.ScopeOpenID, "email", calendarScope}
	return &Handler{
		cfg: cfg, store: store, oauth: login, calOAuth: &calCfg, cal: cal,
		verifier: provider.Verifier(&oidc.Config{ClientID: cfg.GoogleClientID}),
	}, nil
}

func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/login", h.login)
	mux.HandleFunc("GET /auth/callback", h.callback)
	mux.HandleFunc("POST /auth/logout", h.logout)
	mux.Handle("GET /auth/google/calendar", h.Require(http.HandlerFunc(h.calendarStart)))
	mux.Handle("GET /api/me", h.Require(http.HandlerFunc(h.me)))
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	h.start(w, r, h.oauth, "", oauth2.SetAuthURLParam("prompt", "select_account"))
}

// calendarStart begins the incremental authorisation for calendar.events.
// offline access + consent are required to receive a refresh token.
func (h *Handler) calendarStart(w http.ResponseWriter, r *http.Request) {
	u, _ := UserFrom(r.Context())
	h.start(w, r, h.calOAuth, "calendar",
		oauth2.AccessTypeOffline,
		oauth2.SetAuthURLParam("prompt", "consent"),
		oauth2.SetAuthURLParam("include_granted_scopes", "true"),
		oauth2.SetAuthURLParam("login_hint", u.Email))
}

func (h *Handler) start(w http.ResponseWriter, r *http.Request, cfg *oauth2.Config, purpose string, opts ...oauth2.AuthCodeOption) {
	state, _, err1 := authcore.NewToken()
	nonce, _, err2 := authcore.NewToken()
	verifier := oauth2.GenerateVerifier()
	if err1 != nil || err2 != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	tx := authcore.Tx{State: state, Nonce: nonce, Verifier: verifier, Purpose: purpose,
		Expires: time.Now().Add(authcore.TxTTL).Unix()}
	signed, err := authcore.SignTx(h.cfg.SessionSecret, tx)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: authcore.TxCookieName, Value: signed, Path: "/auth",
		MaxAge: int(authcore.TxTTL.Seconds()), HttpOnly: true,
		Secure: h.cfg.SecureCookies(), SameSite: http.SameSiteLaxMode,
	})
	opts = append(opts,
		oauth2.S256ChallengeOption(verifier),
		oauth2.SetAuthURLParam("nonce", nonce))
	http.Redirect(w, r, cfg.AuthCodeURL(state, opts...), http.StatusFound)
}

func (h *Handler) callback(w http.ResponseWriter, r *http.Request) {
	secure := h.cfg.SecureCookies()
	// The transaction cookie is single-use.
	http.SetCookie(w, authcore.ClearCookie(authcore.TxCookieName, "/auth", secure))

	fail := func(code string, err error) {
		if err != nil {
			log.Printf("auth callback: %s: %v", code, err) // never log tokens or claims
		}
		http.Redirect(w, r, "/?error="+code, http.StatusFound)
	}

	c, err := r.Cookie(authcore.TxCookieName)
	if err != nil {
		fail("failed", err)
		return
	}
	tx, err := authcore.VerifyTx(h.cfg.SessionSecret, c.Value, time.Now())
	if err != nil {
		fail("failed", err)
		return
	}
	if r.URL.Query().Get("state") != tx.State || r.URL.Query().Get("code") == "" {
		fail("failed", nil)
		return
	}

	tok, err := h.oauth.Exchange(r.Context(), r.URL.Query().Get("code"), oauth2.VerifierOption(tx.Verifier))
	if err != nil {
		fail("failed", err)
		return
	}
	rawID, _ := tok.Extra("id_token").(string)
	idToken, err := h.verifier.Verify(r.Context(), rawID)
	if err != nil || idToken.Nonce != tx.Nonce {
		fail("failed", err)
		return
	}
	var claims struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
	}
	if err := idToken.Claims(&claims); err != nil {
		fail("failed", err)
		return
	}

	// Allowlist: only the configured account may sign in. No user row is created otherwise.
	if !claims.EmailVerified || !authcore.EmailAllowed(claims.Email, h.cfg.AllowedEmail) {
		log.Printf("auth callback: rejected non-allowlisted login")
		fail("forbidden", nil)
		return
	}

	if tx.Purpose == "calendar" {
		h.finishCalendar(w, r, tok, claims.Email)
		return
	}

	user, err := h.store.UpsertUser(r.Context(), idToken.Subject, claims.Email)
	if err != nil {
		fail("failed", err)
		return
	}
	token, expires, err := h.store.CreateSession(r.Context(), user.ID)
	if err != nil {
		fail("failed", err)
		return
	}
	http.SetCookie(w, authcore.SessionCookie(token, expires, secure))
	http.Redirect(w, r, "/", http.StatusFound)
}

// finishCalendar completes the incremental authorisation for the signed-in user.
func (h *Handler) finishCalendar(w http.ResponseWriter, r *http.Request, tok *oauth2.Token, email string) {
	redirect := func(q string) { http.Redirect(w, r, "/?"+q, http.StatusFound) }

	c, err := r.Cookie(authcore.SessionCookieName)
	if err != nil {
		redirect("error=failed")
		return
	}
	user, err := h.store.Lookup(r.Context(), c.Value)
	if err != nil || !authcore.EmailAllowed(email, user.Email) {
		redirect("error=failed") // the Google account must be the one signed in here
		return
	}
	// Google lets people untick individual scopes on the consent screen.
	if scope, _ := tok.Extra("scope").(string); !strings.Contains(scope, calendarScope) {
		redirect("error=calendar_scope")
		return
	}
	if tok.RefreshToken == "" {
		redirect("error=calendar_failed")
		return
	}
	if h.cal == nil {
		redirect("error=calendar_failed")
		return
	}
	if err := h.cal.Connect(r.Context(), user.ID, tok.RefreshToken); err != nil {
		log.Printf("calendar connect: %v", err)
		redirect("error=calendar_failed")
		return
	}
	redirect("calendar=connected")
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(authcore.SessionCookieName); err == nil {
		if err := h.store.DeleteSession(r.Context(), c.Value); err != nil {
			log.Printf("logout: delete session: %v", err)
		}
	}
	http.SetCookie(w, authcore.ClearCookie(authcore.SessionCookieName, "/", h.cfg.SecureCookies()))
	w.WriteHeader(http.StatusNoContent)
}

type ctxKey struct{}

// Require rejects requests without a valid session with 401 JSON.
func (h *Handler) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(authcore.SessionCookieName)
		if err != nil {
			unauthorized(w)
			return
		}
		u, err := h.store.Lookup(r.Context(), c.Value)
		if err != nil {
			if err != ErrNoSession {
				log.Printf("session lookup: %v", err)
			}
			unauthorized(w)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, u)))
	})
}

func UserFrom(ctx context.Context) (User, bool) {
	u, ok := ctx.Value(ctxKey{}).(User)
	return u, ok
}

func (h *Handler) me(w http.ResponseWriter, r *http.Request) {
	u, _ := UserFrom(r.Context())
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"email": u.Email})
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
}
