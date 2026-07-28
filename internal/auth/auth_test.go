package auth

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/FanDoster/Build-System/internal/db"
)

func testDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok:" + r.URL.Path))
	})
}

func newAuth(t *testing.T, d *db.DB, password, basePath string) *Auth {
	t.Helper()
	a, err := New(d, password, "", basePath)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestDisabledModePassesEverything(t *testing.T) {
	a := newAuth(t, testDB(t), "", "")
	if !a.Disabled() {
		t.Fatal("expected disabled")
	}
	h := a.Middleware(okHandler())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/projects/1", nil))
	if w.Code != 200 {
		t.Errorf("disabled auth blocked a request: %d", w.Code)
	}
}

func TestMiddlewareAllowlistAndBlocking(t *testing.T) {
	a := newAuth(t, testDB(t), "hunter2", "/builds")
	h := a.Middleware(okHandler())

	open := []string{"/api/health", "/api/webhook/github", "/login", "/api/login", "/static/css/style.css"}
	for _, p := range open {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", p, nil))
		if w.Code != 200 {
			t.Errorf("open path %s blocked: %d", p, w.Code)
		}
	}

	// Protected page → redirect to login with next.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/projects/1?x=1", nil))
	if w.Code != 302 {
		t.Fatalf("page: got %d, want 302", w.Code)
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "/builds/login?next=") || !strings.Contains(loc, "%2Fprojects%2F1") {
		t.Errorf("redirect location = %q", loc)
	}

	// Protected API → 401 JSON.
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/builds/1", nil))
	if w.Code != 401 || !strings.Contains(w.Body.String(), "authentication required") {
		t.Errorf("api: got %d %s", w.Code, w.Body.String())
	}
}

func login(t *testing.T, a *Auth, password string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	a.RegisterRoutes(mux)
	req := httptest.NewRequest("POST", "/api/login", strings.NewReader(`{"password":"`+password+`"}`))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

func TestLoginLogoutFlow(t *testing.T) {
	d := testDB(t)
	a := newAuth(t, d, "hunter2", "")
	h := a.Middleware(okHandler())

	if w := login(t, a, "wrong"); w.Code != 401 {
		t.Fatalf("wrong password: got %d", w.Code)
	}
	w := login(t, a, "hunter2")
	if w.Code != 200 {
		t.Fatalf("login: got %d %s", w.Code, w.Body.String())
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != cookieName || !cookies[0].HttpOnly {
		t.Fatalf("cookie wrong: %+v", cookies)
	}

	// Session cookie grants access.
	req := httptest.NewRequest("GET", "/projects/1", nil)
	req.AddCookie(cookies[0])
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("with cookie: got %d", rec.Code)
	}

	// Sessions survive a restart (secret persisted in DB).
	a2 := newAuth(t, d, "hunter2", "")
	req = httptest.NewRequest("GET", "/projects/1", nil)
	req.AddCookie(cookies[0])
	rec = httptest.NewRecorder()
	a2.Middleware(okHandler()).ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("after restart: got %d (secret not persisted?)", rec.Code)
	}

	// Logout expires the cookie.
	mux := http.NewServeMux()
	a.RegisterRoutes(mux)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/logout", nil))
	out := rec.Result().Cookies()
	if len(out) != 1 || out[0].MaxAge >= 0 || out[0].Value != "" {
		t.Errorf("logout cookie: %+v", out)
	}
}

func TestBearerPassword(t *testing.T) {
	a := newAuth(t, testDB(t), "hunter2", "")
	h := a.Middleware(okHandler())

	req := httptest.NewRequest("GET", "/api/builds/1", nil)
	req.Header.Set("Authorization", "Bearer hunter2")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Errorf("bearer correct: got %d", w.Code)
	}

	req = httptest.NewRequest("GET", "/api/builds/1", nil)
	req.Header.Set("Authorization", "Bearer nope")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Errorf("bearer wrong: got %d", w.Code)
	}
}

func TestTokenTamperAndExpiry(t *testing.T) {
	a := newAuth(t, testDB(t), "hunter2", "")

	tok := a.newToken(time.Now())
	if !a.validToken(tok) {
		t.Fatal("fresh token invalid")
	}
	if a.validToken(tok + "x") {
		t.Error("tampered signature accepted")
	}
	// Forge a later expiry with the old signature.
	exp, sig, _ := strings.Cut(tok, ".")
	if a.validToken("9999999999." + sig) {
		t.Error("expiry-extended token accepted")
	}
	_ = exp
	// Expired token.
	old := a.newToken(time.Now().Add(-sessionTTL - time.Hour))
	if a.validToken(old) {
		t.Error("expired token accepted")
	}
	// A different instance (different DB → different secret) must reject it.
	b := newAuth(t, testDB(t), "hunter2", "")
	if b.validToken(tok) {
		t.Error("token accepted across different secrets")
	}
}

func TestBcryptHashMode(t *testing.T) {
	d := testDB(t)
	hash, err := bcrypt.GenerateFromPassword([]byte("hunter2"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	a, err := New(d, "", string(hash), "")
	if err != nil {
		t.Fatal(err)
	}
	if a.Disabled() {
		t.Fatal("hash mode should enable auth")
	}
	if !a.verifyPassword("hunter2") {
		t.Error("correct password rejected in hash mode")
	}
	if a.verifyPassword("wrong") {
		t.Error("wrong password accepted in hash mode")
	}
}

func TestRateLimitLockout(t *testing.T) {
	a := newAuth(t, testDB(t), "hunter2", "")
	for i := 0; i < maxAttempts; i++ {
		if w := login(t, a, "wrong"); w.Code != 401 {
			t.Fatalf("attempt %d: got %d, want 401", i, w.Code)
		}
	}
	if w := login(t, a, "hunter2"); w.Code != 429 {
		t.Errorf("after lockout: got %d, want 429 even with correct password", w.Code)
	}
}

// A build agent authenticates with its own credential, so the machines running
// builds never hold the password that signs into the UI.
func TestAgentTokenAuthenticatesButIsMarkedAsAgent(t *testing.T) {
	d := testDB(t)
	a, err := New(d, "operator-pw", "", "")
	if err != nil {
		t.Fatal(err)
	}
	a.SetAgentToken("agent-tok")

	var sawAuthed, sawAgent bool
	h := a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuthed, sawAgent = IsAuthed(r.Context()), IsAgent(r.Context())
		w.WriteHeader(200)
	}))

	call := func(bearer string) int {
		sawAuthed, sawAgent = false, false
		req := httptest.NewRequest("GET", "/api/projects", nil)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code
	}

	if code := call("agent-tok"); code != 200 {
		t.Fatalf("agent token: status %d, want 200", code)
	}
	if !sawAuthed || !sawAgent {
		t.Errorf("agent request: authed=%v agent=%v, want both true", sawAuthed, sawAgent)
	}

	if code := call("operator-pw"); code != 200 {
		t.Fatalf("operator password: status %d, want 200", code)
	}
	if !sawAuthed || sawAgent {
		t.Errorf("operator request: authed=%v agent=%v, want authed only", sawAuthed, sawAgent)
	}

	if code := call("wrong"); code != 401 {
		t.Errorf("bad token: status %d, want 401", code)
	}
	// With no agent token configured, only the password works.
	b, _ := New(d, "operator-pw", "", "")
	req := httptest.NewRequest("GET", "/api/projects", nil)
	req.Header.Set("Authorization", "Bearer agent-tok")
	w := httptest.NewRecorder()
	b.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(w, req)
	if w.Code != 401 {
		t.Errorf("agent token with none configured: status %d, want 401", w.Code)
	}
}
