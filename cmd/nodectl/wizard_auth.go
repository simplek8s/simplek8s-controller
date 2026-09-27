package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// Wizard auth (PLAN.md §3.16, M9 D4): root-only login verified
// by the distro-owned setuid `su` (dynamic libcrypt ⇒ future
// hashes and a future PAM need no nodectl change), session
// cookie, per-IP backoff, Origin guard on mutating calls.

const (
	wizardSessionCookie = "sk8s_wizard"
	wizardSessionTTL    = 30 * time.Minute
)

var (
	// wizardAuthTimeout bounds one `su` validation (a var so
	// tests can shrink it against a stub helper).
	wizardAuthTimeout = 15 * time.Second
)

var (
	// wizardSuPath is the distro setuid validator (M9 D4
	// prerequisite). Overridden in tests with a stub.
	wizardSuPath = "/usr/bin/su"
	// nobodyCred drops the forked `su` caller to unprivileged:
	// as root `su` never prompts (measured bypass), as
	// non-root the setuid bit does the elevation. Tests run
	// non-root and override it with their own uid.
	nobodyCred   = &syscall.Credential{Uid: 65534, Gid: 65534, Groups: []uint32{}}
	wizardSuCred = nobodyCred
)

// wizardSessions is the in-memory session store.
type wizardSessions struct {
	mu sync.Mutex
	by map[string]time.Time
}

var sessions = &wizardSessions{by: map[string]time.Time{}}

// mintSession creates a 256-bit random session.
func (s *wizardSessions) mint() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(raw[:])
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purgeLocked(time.Now())
	s.by[tok] = time.Now().Add(wizardSessionTTL)
	return tok, nil
}

// valid reports whether tok is live, sliding its expiry.
func (s *wizardSessions) valid(tok string) bool {
	if tok == "" {
		return false
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.by[tok]
	if !ok || now.After(exp) {
		delete(s.by, tok)
		return false
	}
	s.by[tok] = now.Add(wizardSessionTTL)
	return true
}

func (s *wizardSessions) drop(tok string) {
	if tok == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.by, tok)
}

func (s *wizardSessions) purgeLocked(now time.Time) {
	for tok, exp := range s.by {
		if now.After(exp) {
			delete(s.by, tok)
		}
	}
}

// wizardLimiter backs off brute force per source IP: 5 free
// attempts per 10 minutes, then doubling delays capped at 5m.
type wizardLimiter struct {
	mu  sync.Mutex
	ips map[string]*wizardAttempt
}

type wizardAttempt struct {
	window  time.Time
	count   int
	blocked time.Time
}

var limiter = &wizardLimiter{ips: map[string]*wizardAttempt{}}

func (l *wizardLimiter) denied(ip string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	a, ok := l.ips[ip]
	if !ok {
		return false
	}
	if now.Before(a.blocked) {
		return true
	}
	if now.Sub(a.window) > 10*time.Minute {
		delete(l.ips, ip)
	}
	return false
}

func (l *wizardLimiter) fail(ip string) {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	a, ok := l.ips[ip]
	if !ok || now.Sub(a.window) > 10*time.Minute {
		a = &wizardAttempt{window: now}
		l.ips[ip] = a
	}
	a.count++
	if a.count > 5 {
		d := time.Minute << (a.count - 6)
		if d > 5*time.Minute || d <= 0 {
			d = 5 * time.Minute
		}
		a.blocked = now.Add(d)
	}
}

func (l *wizardLimiter) reset(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.ips, ip)
}

// clientIP is the direct peer (no proxies: TLS terminates here).
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// suCommand builds one validator invocation. Production
// drops the forked child to nobody (root callers are never
// prompted — measured bypass); the candidate travels by pipe
// only, stdio discarded. Tests swap the constructor for a
// stub: the nobody drop needs privilege the test sandbox
// lacks, and is one static line reviewed by inspection.
var suCommand = func(ctx context.Context, stdin io.Reader) *exec.Cmd {
	cmd := exec.CommandContext(ctx, wizardSuPath, "root", "-c", "true")
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: wizardSuCred}
	cmd.Stdin = stdin
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return cmd
}

// verifyRootPassword delegates to the setuid `su`: exit 0 is
// valid, any prompt-less failure is invalid, and a timeout
// or exec failure is a backend error (500, never 401).
func verifyRootPassword(password string) (ok bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), wizardAuthTimeout)
	defer cancel()
	runErr := suCommand(ctx, bytes.NewBufferString(password+"\n")).Run()
	if ctx.Err() == context.DeadlineExceeded {
		return false, context.DeadlineExceeded
	}
	return runErr == nil, nil
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func loginHandler(log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			apiError(w, http.StatusForbidden, "origin mismatch")
			return
		}
		var req loginRequest
		if err := readJSON(w, r, &req); err != nil {
			apiError(w, http.StatusBadRequest, "invalid request")
			return
		}
		ip := clientIP(r)
		if limiter.denied(ip) {
			apiError(w, http.StatusTooManyRequests, "too many attempts, retry later")
			return
		}
		// Root only, generic failure either way (no enumeration).
		if req.Username != "root" || req.Password == "" {
			limiter.fail(ip)
			apiError(w, http.StatusUnauthorized, "invalid credentials")
			return
		}
		ok, err := verifyRootPassword(req.Password)
		if err != nil {
			log.Error("auth backend failed", "err", err)
			apiError(w, http.StatusInternalServerError, "authentication unavailable")
			return
		}
		if !ok {
			limiter.fail(ip)
			apiError(w, http.StatusUnauthorized, "invalid credentials")
			return
		}
		limiter.reset(ip)
		tok, err := sessions.mint()
		if err != nil {
			log.Error("minting session failed", "err", err)
			apiError(w, http.StatusInternalServerError, "authentication unavailable")
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name:     wizardSessionCookie,
			Value:    tok,
			Path:     "/",
			HttpOnly: true,
			Secure:   true,
			SameSite: http.SameSiteStrictMode,
		})
		writeJSON(w, http.StatusOK, map[string]string{"state": string(detectWizardState())})
	}
}

func logoutHandler(log *slog.Logger) http.HandlerFunc {
	_ = log
	return func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(wizardSessionCookie); err == nil {
			sessions.drop(c.Value)
		}
		http.SetCookie(w, &http.Cookie{
			Name:     wizardSessionCookie,
			Value:    "",
			Path:     "/",
			MaxAge:   -1,
			HttpOnly: true,
			Secure:   true,
			SameSite: http.SameSiteStrictMode,
		})
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

// withSession enforces the session cookie plus the per-endpoint
// state gate (nil states = any state, e.g. jobs polling across
// a state flip, or logout).
func withSession(log *slog.Logger, states []wizardState, next http.HandlerFunc) http.HandlerFunc {
	_ = log
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(wizardSessionCookie)
		if err != nil || !sessions.valid(c.Value) {
			apiError(w, http.StatusUnauthorized, "login required")
			return
		}
		if len(states) > 0 {
			cur := detectWizardState()
			allowed := false
			for _, s := range states {
				if cur == s {
					allowed = true
					break
				}
			}
			if !allowed {
				apiError(w, http.StatusForbidden, "not available in state "+string(cur))
				return
			}
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !sameOrigin(r) {
			apiError(w, http.StatusForbidden, "origin mismatch")
			return
		}
		next(w, r)
	}
}

// sameOrigin is the stateless CSRF guard: browsers always send
// Origin (or Referer) on fetch POSTs; non-browser clients send
// neither and pass. A present-but-foreign value fails closed.
func sameOrigin(r *http.Request) bool {
	if o := r.Header.Get("Origin"); o != "" {
		u, err := url.Parse(o)
		if err != nil {
			return false
		}
		return u.Host == r.Host
	}
	if ref := r.Header.Get("Referer"); ref != "" {
		u, err := url.Parse(ref)
		if err != nil {
			return false
		}
		return u.Host == r.Host
	}
	return true
}
