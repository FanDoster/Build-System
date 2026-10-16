// Package notifications delivers bounded, durable Telegram events through the
// notification relay. It never talks to Telegram or accepts a chat destination.
package notifications

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/FanDoster/Build-System/internal/agents"
	"github.com/FanDoster/Build-System/internal/db"
	"github.com/FanDoster/Build-System/internal/models"
)

const DefaultURL = "https://notify.fandoster.com"

// Config has no recipient controls. A dedicated group-only identity and its
// binding are administered at the relay, independently of project settings.
type Config struct {
	URL, Key, PublicURL string
	Client              *http.Client // TLS test transport; timeout/redirect policy still enforced.
	Secrets             []string
}

type Worker struct {
	DB                      *db.DB
	Registry                *agents.Registry
	key, baseURL, publicURL string
	client                  *http.Client
	secrets                 []string
	configured              atomic.Bool
	verifiedAt              atomic.Int64
	routeBackoff            routeBackoff
	relayNext               int64
}

// New rejects insecure URL configuration, even when notifications are disabled.
func New(database *db.DB, registry *agents.Registry, cfg Config) (*Worker, error) {
	if cfg.URL == "" {
		cfg.URL = DefaultURL
	}
	u, err := url.Parse(cfg.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || strings.Contains(cfg.URL, "#") || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("Telegram relay URL must be an HTTPS origin without credentials, path, query or fragment")
	}
	if len(cfg.Key) > 4096 || strings.IndexFunc(cfg.Key, func(r rune) bool { return r <= 0x20 || r >= 0x7f }) >= 0 {
		return nil, errors.New("invalid Telegram relay key")
	}
	client := &http.Client{Transport: &http.Transport{
		DialContext:         (&net.Dialer{Timeout: 2 * time.Second, KeepAlive: 20 * time.Second}).DialContext,
		TLSHandshakeTimeout: 2 * time.Second, ResponseHeaderTimeout: 3 * time.Second, IdleConnTimeout: 30 * time.Second,
		MaxIdleConns: 2, MaxConnsPerHost: 2,
	}}
	if cfg.Client != nil {
		copy := *cfg.Client
		client = &copy
	}
	client.Timeout = 5 * time.Second
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	w := &Worker{DB: database, Registry: registry, key: cfg.Key, baseURL: strings.TrimRight(cfg.URL, "/"), client: client, secrets: append([]string{}, cfg.Secrets...)}
	w.secrets = append(w.secrets, cfg.Key)
	if cfg.PublicURL != "" {
		p, e := url.Parse(cfg.PublicURL)
		if e != nil || p.Scheme != "https" || p.Host == "" || p.User != nil || p.RawQuery != "" || p.ForceQuery || strings.Contains(cfg.PublicURL, "#") || p.Fragment != "" {
			return nil, errors.New("public build URL must use HTTPS without credentials, query or fragment")
		}
		w.publicURL = strings.TrimRight(cfg.PublicURL, "/")
	}
	if saved, err := database.GetSetting("telegram_route_backoff"); err != nil {
		return nil, err
	} else if saved != "" {
		_ = json.Unmarshal([]byte(saved), &w.routeBackoff)
	}
	if saved, err := database.GetSetting("telegram_relay_retry_at"); err != nil {
		return nil, err
	} else {
		w.relayNext, _ = strconv.ParseInt(saved, 10, 64)
	}
	return w, nil
}

// FromEnv reads a server-only file, never a key-valued environment variable.
// Errors deliberately omit paths, file contents and HTTP bodies.
func FromEnv(database *db.DB, registry *agents.Registry) (*Worker, error) {
	key := ""
	if path := os.Getenv("BUILDS_TELEGRAM_KEY_FILE"); path != "" {
		f, err := os.Open(path)
		if err != nil {
			return nil, errors.New("cannot read Telegram relay key file")
		}
		b, err := io.ReadAll(io.LimitReader(f, 4097))
		f.Close()
		if err != nil || len(b) > 4096 {
			return nil, errors.New("invalid Telegram relay key file")
		}
		key = strings.TrimSpace(string(b))
		if key == "" {
			return nil, errors.New("empty Telegram relay key file")
		}
	}
	if key == "" {
		if err := database.SetTelegramCapture(false); err != nil {
			return nil, err
		}
	}
	return New(database, registry, Config{URL: os.Getenv("BUILDS_TELEGRAM_URL"), Key: key, PublicURL: os.Getenv("BUILDS_PUBLIC_URL"), Secrets: []string{os.Getenv("BUILDS_PASSWORD"), os.Getenv("BUILDS_PASSWORD_HASH"), os.Getenv("BUILDS_AGENT_TOKEN")}})
}

// Configured exposes only a short-lived verified readiness bit, never key,
// path, route or destination metadata. A key on disk alone is not readiness.
func (w *Worker) Configured() bool {
	return w != nil && w.configured.Load() && time.Since(time.Unix(w.verifiedAt.Load(), 0)) < 30*time.Second
}

func (w *Worker) Run(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		// Errors affect only notification delivery, not the build runner. Do not
		// log response bodies, metadata, keys or private project data here.
		_ = w.Sweep(ctx, time.Now())
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Sweep is a single bounded work unit. Call it serially; main runs one worker.
// Time is explicit so lifecycle/retry/retention tests never need to sleep.
func (w *Worker) Sweep(ctx context.Context, now time.Time) error {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	if err := w.DB.PruneNotifications(now); err != nil {
		return err
	}
	if w.key == "" {
		w.configured.Store(false)
		return w.DB.SetTelegramCapture(false)
	}
	agentErr := w.observeAgents(now)
	// A relay-wide 429 applies to every queued request, not only the first
	// event that encountered it. Persist the gate across process restarts.
	if now.Unix() < w.relayNext {
		return agentErr
	}
	// Refresh readiness even with an empty outbox. This is also checked again
	// immediately before each POST; a stale UI readiness bit never authorises it.
	_, _, _ = w.verifyRoute(ctx, now)
	due, err := w.DB.DueNotifications(now, 10)
	if err != nil {
		return err
	}
	for _, n := range due {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := w.process(ctx, now, n); err != nil {
			return err
		}
		if now.Unix() < w.relayNext {
			break
		}
	}
	return agentErr
}

type routeBackoff struct {
	Attempts int
	Next     int64
}

func (w *Worker) verifyRoute(ctx context.Context, now time.Time) (bool, time.Duration, error) {
	if now.Unix() < w.routeBackoff.Next {
		w.configured.Store(false)
		return false, time.Duration(w.routeBackoff.Next-now.Unix()) * time.Second, nil
	}
	// Pointers distinguish an explicit false from an absent field. A relay
	// version without this contract must never authorise personal fallback.
	var route struct {
		Configured    *bool `json:"configured"`
		Group         *bool `json:"group"`
		PreserveTitle *bool `json:"preserve_title"`
	}
	status, retry, err := w.request(ctx, http.MethodGet, "/v1/route", "", "", &route, now)
	okay := err == nil && status == http.StatusOK && route.Configured != nil && *route.Configured && route.Group != nil && *route.Group && route.PreserveTitle != nil && !*route.PreserveTitle
	w.configured.Store(okay)
	if okay {
		if e := w.DB.SetTelegramCapture(true); e != nil {
			return false, 0, e
		}
		w.verifiedAt.Store(now.Unix())
		if w.routeBackoff.Attempts > 0 {
			w.routeBackoff = routeBackoff{}
			if e := w.DB.SetSetting("telegram_route_backoff", ""); e != nil {
				return false, 0, e
			}
		}
	} else {
		if status == http.StatusOK && err == nil {
			if e := w.DB.SetTelegramCapture(false); e != nil {
				return false, 0, e
			}
		}
		w.routeBackoff.Attempts++
		delay := backoff(w.routeBackoff.Attempts, retry)
		w.routeBackoff.Next = now.Add(delay).Unix()
		encoded, _ := json.Marshal(w.routeBackoff)
		if e := w.DB.SetSetting("telegram_route_backoff", string(encoded)); e != nil {
			return false, delay, e
		}
		retry = delay
	}
	return okay, retry, err
}

var relayIDPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

func validReceiptStatus(s string) bool {
	switch s {
	case "pending", "sent", "failed", "discarded", "expired":
		return true
	}
	return false
}

type receipt struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

func (w *Worker) process(ctx context.Context, now time.Time, n db.Notification) error {
	if n.State == "submitted" {
		return w.delivery(ctx, now, n)
	}
	p, err := w.DB.GetProject(n.ProjectID)
	if err != nil {
		return err
	}
	if !p.TelegramNotifications {
		return w.DB.UpdateNotification(n.ID, "muted", "", n.Attempts, now)
	}
	if n.Payload == "" {
		var event buildEvent
		if err := json.Unmarshal([]byte(n.EventJSON), &event); err != nil {
			return w.DB.UpdateNotification(n.ID, "failed", "", n.Attempts, now)
		}
		event.Project = w.scrub(*p, event.Project)
		event.Branch = w.scrub(*p, event.Branch)
		event.Executor = w.scrub(*p, event.Executor)
		event.Agent = w.scrub(*p, event.Agent)
		event.Commit = w.scrub(*p, event.Commit)
		n.Payload = w.projectPayload(*p, w.buildText(event))
		if err := w.DB.SetNotificationPayload(n.ID, n.Payload); err != nil {
			return err
		}
	}
	ok, retry, routeErr := w.verifyRoute(ctx, now)
	if !ok || routeErr != nil {
		return w.retry(now, n, retry)
	}
	// A settings save while metadata was being read must mute this attempt too.
	p, err = w.DB.GetProject(n.ProjectID)
	if err != nil {
		return err
	}
	if !p.TelegramNotifications {
		return w.DB.UpdateNotification(n.ID, "muted", "", n.Attempts, now)
	}
	canSubmit, err := w.DB.NotificationCanSubmit(n.ID)
	if err != nil {
		return err
	}
	if !canSubmit {
		return nil
	}
	var result receipt
	status, retry, err := w.request(ctx, http.MethodPost, "/v1/notifications", n.Payload, n.EventKey, &result, now)
	if err != nil || (status != http.StatusOK && status != http.StatusAccepted) || (!relayIDPattern.MatchString(result.ID) || !validReceiptStatus(result.Status)) {
		if status == 400 || status == 413 || status == 422 {
			return w.DB.UpdateNotification(n.ID, "failed", "", n.Attempts+1, now)
		}
		return w.retry(now, n, retry)
	}
	state := "submitted"
	switch result.Status {
	case "sent":
		state = "delivered"
	case "failed", "discarded", "expired":
		state = "failed"
	}
	return w.DB.RecordNotificationAcceptance(n.ID, state, result.ID, n.Attempts+1, now.Add(30*time.Second))
}

func (w *Worker) delivery(ctx context.Context, now time.Time, n db.Notification) error {
	if !relayIDPattern.MatchString(n.RelayID) {
		return w.DB.UpdateNotification(n.ID, "failed", "", n.Attempts, now)
	}
	var result receipt
	status, retry, err := w.request(ctx, http.MethodGet, "/v1/notifications/"+n.RelayID, "", "", &result, now)
	if err != nil || status != 200 || result.ID != n.RelayID || !validReceiptStatus(result.Status) {
		return w.retry(now, n, retry)
	}
	state := "submitted"
	switch result.Status {
	case "sent":
		state = "delivered"
	case "failed", "discarded", "expired":
		state = "failed"
	}
	return w.DB.UpdateNotification(n.ID, state, n.RelayID, n.Attempts, now.Add(30*time.Second))
}

func (w *Worker) retry(now time.Time, n db.Notification, retry time.Duration) error {
	attempts := n.Attempts + 1
	return w.DB.UpdateNotification(n.ID, n.State, n.RelayID, attempts, now.Add(backoff(attempts, retry)))
}

func backoff(attempts int, retry time.Duration) time.Duration {
	delay := 5 * time.Second
	for i := 1; i < attempts && delay < 30*time.Minute; i++ {
		delay *= 2
	}
	if delay > 30*time.Minute {
		delay = 30 * time.Minute
	}
	if retry > delay {
		delay = retry
	}
	if delay > 24*time.Hour {
		delay = 24 * time.Hour
	}
	return delay
}

func (w *Worker) request(ctx context.Context, method, path, body, key string, out any, now time.Time) (int, time.Duration, error) {
	if now.Unix() < w.relayNext {
		return http.StatusTooManyRequests, time.Duration(w.relayNext-now.Unix()) * time.Second, nil
	}
	req, err := http.NewRequestWithContext(ctx, method, w.baseURL+path, strings.NewReader(body))
	if err != nil {
		return 0, 0, errors.New("invalid relay request")
	}
	req.Header.Set("Authorization", "Bearer "+w.key)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := w.client.Do(req)
	if err != nil {
		return 0, 0, errors.New("relay request failed")
	}
	defer resp.Body.Close()
	retry := time.Duration(0)
	if resp.StatusCode == 429 || resp.StatusCode == 503 {
		if seconds, e := strconv.ParseInt(resp.Header.Get("Retry-After"), 10, 32); e == nil && seconds > 0 {
			retry = time.Duration(seconds) * time.Second
		} else if at, e := http.ParseTime(resp.Header.Get("Retry-After")); e == nil && at.After(now) {
			retry = at.Sub(now)
		}
	}
	if resp.StatusCode == 429 {
		if retry < 5*time.Second {
			retry = 5 * time.Second
		}
		if retry > 24*time.Hour {
			retry = 24 * time.Hour
		}
		w.relayNext = now.Add(retry).Unix()
		if e := w.DB.SetSetting("telegram_relay_retry_at", strconv.FormatInt(w.relayNext, 10)); e != nil {
			return resp.StatusCode, retry, e
		}
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8193))
	if err != nil || len(b) > 8192 {
		return resp.StatusCode, retry, errors.New("invalid relay response")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, retry, nil
	}
	if err := json.Unmarshal(b, out); err != nil {
		return resp.StatusCode, retry, errors.New("invalid relay response")
	}
	return resp.StatusCode, retry, nil
}

// The relay's safe Markdown subset supports **bold**; it renders escaped
// Telegram HTML. Metadata cannot introduce formatting, links or extra lines.
var metadataURL = regexp.MustCompile(`(?i)(?:[a-z][a-z0-9+.-]*:)?//[^\s]+|git@[^\s]+:[^\s]+`)
var metadataToken = regexp.MustCompile(`(?i)(?:gh[pousr]_[a-z0-9_]{10,}|github_pat_[a-z0-9_]{10,}|(?:token|password|secret|authorization)\s*[=:]\s*[^\s]+)`)

func cleanPlain(s string, max int) string {
	s = metadataURL.ReplaceAllString(s, "[redacted URL]")
	s = metadataToken.ReplaceAllString(s, "[redacted]")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == unicode.ReplacementChar {
			return ' '
		}
		return r
	}, s)
	return bound(strings.Join(strings.Fields(s), " "), max)
}
func clean(s string, max int) string {
	s = cleanPlain(s, max)
	s = strings.NewReplacer("*", "＊", "_", "＿", "`", "ˋ", "[", "［", "]", "］", "<", "‹", ">", "›", "\\", "＼").Replace(s)
	return bound(s, max)
}

func bound(s string, max int) string {
	count := 0
	for i, r := range s {
		n := 1
		if r > 0xffff {
			n = 2
		}
		if count+n > max {
			return s[:i]
		}
		count += n
	}
	return s
}

// scrub runs on raw metadata before any display truncation. It also masks a
// secret prefix at the end of an SQL-bounded snapshot field, so capture limits
// cannot turn a credential into an unmatched partial string.
func (w *Worker) scrub(p models.Project, text string) string {
	secrets := append(append([]string{}, w.secrets...), p.CloneToken, p.WebhookSecret)
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		for _, variant := range []string{secret, url.QueryEscape(secret), url.PathEscape(secret), clean(secret, 4096), cleanPlain(secret, 4096)} {
			if variant == "" {
				continue
			}
			text = strings.ReplaceAll(text, variant, "[redacted]")
			// A full secret replaced above is no longer present. Only a trailing
			// prefix can have been clipped by the bounded SQL capture.
			max := len(variant) - 1
			if max > len(text) {
				max = len(text)
			}
			for n := max; n >= 4; n-- {
				if strings.HasSuffix(text, variant[:n]) {
					text = text[:len(text)-n] + "[redacted]"
					break
				}
			}
		}
	}
	return text
}

func (w *Worker) projectPayload(p models.Project, text string) string {
	// ListProjects sanitizes secrets. Fetch only this project, never full logs.
	if full, err := w.DB.GetProject(p.ID); err == nil {
		p = *full
	}
	text = w.scrub(p, text)
	// Truncate only the tail: the complete bounded first bold heading survives.
	text = bound(text, 3300)
	format := "markdown"
	if strings.HasPrefix(text, "🤖") {
		format = "plain"
	}
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(struct {
		Text   string `json:"text"`
		Format string `json:"format"`
	}{text, format})
	return strings.TrimSpace(buf.String())
}

type buildEvent struct {
	ID        int64              `json:"id"`
	Number    int64              `json:"number"`
	ProjectID int64              `json:"project_id"`
	Project   string             `json:"project"`
	Branch    string             `json:"branch"`
	Status    models.BuildStatus `json:"status"`
	Commit    string             `json:"commit"`
	Executor  string             `json:"executor"`
	Agent     string             `json:"agent"`
	Started   *string            `json:"started"`
	Finished  *string            `json:"finished"`
}

func (w *Worker) buildText(b buildEvent) string {
	icon, result := "❌", "FAILED"
	if b.Status == models.StatusSuccess {
		icon, result = "✅", "SUCCEEDED"
	} else if b.Status == models.StatusCanceled {
		result = "CANCELED"
	}
	owner := clean(b.Agent, 64)
	if owner == "" {
		owner = "Local runner"
		if models.Remote(b.Executor) {
			owner = "Not recorded"
		}
	}
	queue := b.Executor
	if queue == "" {
		queue = models.ExecutorLocal
	}
	commit := b.Commit
	if len(commit) > 12 {
		commit = commit[:12]
	}
	text := fmt.Sprintf("**%s %s #%d — %s**\nAgent: %s\nQueue: %s\nBranch: %s\nCommit: %s", icon, clean(b.Project, 120), b.Number, result, owner, clean(queue, 100), clean(b.Branch, 180), clean(commit, 32))
	if start, okay := eventTime(b.Started); okay {
		if finish, okay := eventTime(b.Finished); okay && !finish.Before(start) {
			text += "\nDuration: " + finish.Sub(start).Round(time.Second).String()
		}
	}
	if w.publicURL != "" {
		text += fmt.Sprintf("\n%s/builds/%d", w.publicURL, b.ID)
	}
	return text
}
func eventTime(raw *string) (time.Time, bool) {
	if raw == nil {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999 -0700 MST", "2006-01-02 15:04:05.999999999 -0700 -0700", "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, *raw); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
