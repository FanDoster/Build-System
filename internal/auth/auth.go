// Package auth password-protects the Build-System UI and API.
//
// Model: one operator, one password, supplied via environment (plaintext
// BUILDS_PASSWORD or bcrypt BUILDS_PASSWORD_HASH — hash wins when both are
// set). Successful login issues a stateless session token — an expiry
// timestamp HMAC-signed with a secret that is generated once and persisted
// in the DB, so sessions survive server restarts and redeploys. The token
// travels in an HttpOnly cookie; non-browser clients may instead send
// "Authorization: Bearer <password>".
//
// Always open: /api/health (healthchecks), /api/webhook/github (GitHub
// cannot log in; the webhook is HMAC-authenticated on its own), /static/
// (the login page needs CSS), /login and /api/login.
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/FanDoster/Build-System/internal/db"
)

const (
	cookieName     = "builds_session"
	sessionTTL     = 30 * 24 * time.Hour
	secretKey      = "session_secret" // app_settings key
	maxAttempts    = 5                // login rate limit bucket size
	refillInterval = 15 * time.Second // one attempt regained per interval
)

type ctxKey struct{}
type agentKey struct{}

// Auth is the middleware + login handlers. Zero-value is unusable; construct
// with New.
type Auth struct {
	password string // plaintext mode ("" when disabled or hash mode)
	hash     []byte // bcrypt mode (nil otherwise)
	basePath string
	secret   []byte

	// agentToken is a second credential (BUILDS_AGENT_TOKEN) for build
	// agents. It exists so the machines running builds never hold the
	// operator's password: it can be rotated on its own, and requests bearing
	// it are marked as agent requests so they can be kept away from the
	// endpoints that reconfigure what gets built.
	agentToken string

	mu       sync.Mutex
	attempts float64
	lastRef  time.Time
}

// New configures auth. password/passwordHash may both be empty, which
// DISABLES authentication entirely — the caller should log a loud warning.
// The session-signing secret is loaded from (or created in) the DB.
func New(database *db.DB, password, passwordHash, basePath string) (*Auth, error) {
	a := &Auth{
		password: password,
		basePath: basePath,
		attempts: maxAttempts,
		lastRef:  time.Now(),
	}
	if passwordHash != "" {
		a.hash = []byte(passwordHash)
		a.password = "" // hash takes precedence; never keep both
	}
	if a.Disabled() {
		return a, nil
	}

	sec, err := database.GetSetting(secretKey)
	if err != nil {
		return nil, err
	}
	if sec == "" {
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return nil, err
		}
		sec = hex.EncodeToString(raw)
		if err := database.SetSetting(secretKey, sec); err != nil {
			return nil, err
		}
	}
	a.secret = []byte(sec)
	return a, nil
}

// Disabled reports whether no password is configured.
func (a *Auth) Disabled() bool {
	return a.password == "" && len(a.hash) == 0
}

// IsAuthed reports whether the request context carries a valid session
// (set by Middleware). Used by templates for the sign-out link.
func IsAuthed(ctx context.Context) bool {
	v, _ := ctx.Value(ctxKey{}).(bool)
	return v
}

// IsAgent reports whether the request authenticated with the agent token
// rather than the operator password. Handlers use it to refuse requests that
// go beyond running builds — an agent's credential lives on a build machine,
// and a build machine has no business creating or deleting projects.
func IsAgent(ctx context.Context) bool {
	v, _ := ctx.Value(agentKey{}).(bool)
	return v
}

// SetAgentToken installs the agent credential. Empty (the default) means no
// agent may authenticate and only the operator password works.
func (a *Auth) SetAgentToken(tok string) { a.agentToken = tok }

func (a *Auth) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/login", a.handleLogin)
	mux.HandleFunc("POST /api/logout", a.handleLogout)
}

// Middleware enforces authentication on everything except the allowlist.
// Browser page requests are redirected to the login page; API requests get
// 401 JSON.
func (a *Auth) Middleware(next http.Handler) http.Handler {
	if a.Disabled() {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authed, agent := a.requestAuthed(r)
		ctx := context.WithValue(r.Context(), ctxKey{}, authed)
		ctx = context.WithValue(ctx, agentKey{}, agent)
		r = r.WithContext(ctx)

		if openPath(r.URL.Path) || authed {
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"error": "authentication required"})
			return
		}
		next_ := r.URL.Path
		if r.URL.RawQuery != "" {
			next_ += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, a.basePath+"/login?next="+url.QueryEscape(next_), http.StatusFound)
	})
}

func openPath(p string) bool {
	if p == "/api/health" || p == "/api/webhook/github" || p == "/login" || p == "/api/login" {
		return true
	}
	return strings.HasPrefix(p, "/static/")
}

// requestAuthed reports whether the request carries a valid credential, and
// whether that credential was an agent's rather than the operator's.
func (a *Auth) requestAuthed(r *http.Request) (authed, agent bool) {
	if c, err := r.Cookie(cookieName); err == nil && a.validToken(c.Value) {
		return true, false
	}
	// Script convenience: the password itself as a bearer token. A build
	// agent presents its own token in the same header instead.
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		tok := strings.TrimPrefix(h, "Bearer ")
		if a.verifyPassword(tok) {
			return true, false
		}
		if a.agentToken != "" && subtle.ConstantTimeCompare([]byte(a.agentToken), []byte(tok)) == 1 {
			return true, true
		}
	}
	return false, false
}

func (a *Auth) verifyPassword(candidate string) bool {
	if len(a.hash) > 0 {
		return bcrypt.CompareHashAndPassword(a.hash, []byte(candidate)) == nil
	}
	return subtle.ConstantTimeCompare([]byte(a.password), []byte(candidate)) == 1
}

// --- session tokens: "<unix-expiry>.<hex hmac-sha256(secret, expiry)>" ---

func (a *Auth) newToken(now time.Time) string {
	exp := strconv.FormatInt(now.Add(sessionTTL).Unix(), 10)
	return exp + "." + a.sign(exp)
}

func (a *Auth) validToken(tok string) bool {
	exp, sig, ok := strings.Cut(tok, ".")
	if !ok {
		return false
	}
	n, err := strconv.ParseInt(exp, 10, 64)
	if err != nil || time.Now().Unix() > n {
		return false
	}
	return hmac.Equal([]byte(sig), []byte(a.sign(exp)))
}

func (a *Auth) sign(msg string) string {
	mac := hmac.New(sha256.New, a.secret)
	mac.Write([]byte(msg))
	return hex.EncodeToString(mac.Sum(nil))
}

// --- login / logout ---

func (a *Auth) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !a.allowAttempt() {
		writeJSONError(w, http.StatusTooManyRequests, "too many attempts, wait a moment")
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if !a.verifyPassword(body.Password) {
		writeJSONError(w, http.StatusUnauthorized, "wrong password")
		return
	}
	a.resetAttempts()
	http.SetCookie(w, a.sessionCookie(a.newToken(time.Now()), sessionTTL, r))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (a *Auth) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, a.sessionCookie("", -time.Hour, r))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "signed out"})
}

func (a *Auth) sessionCookie(value string, ttl time.Duration, r *http.Request) *http.Cookie {
	path := a.basePath
	if path == "" {
		path = "/"
	}
	return &http.Cookie{
		Name:     cookieName,
		Value:    value,
		Path:     path,
		MaxAge:   int(ttl.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		// Secure when the client reached us over TLS (directly or via the
		// reverse proxy); plain-HTTP LAN/dev setups still get a cookie.
		Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
	}
}

// --- global login rate limit (token bucket) ---

func (a *Auth) allowAttempt() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	a.attempts += now.Sub(a.lastRef).Seconds() / refillInterval.Seconds()
	if a.attempts > maxAttempts {
		a.attempts = maxAttempts
	}
	a.lastRef = now
	if a.attempts < 1 {
		return false
	}
	a.attempts--
	return true
}

func (a *Auth) resetAttempts() {
	a.mu.Lock()
	a.attempts = maxAttempts
	a.mu.Unlock()
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
