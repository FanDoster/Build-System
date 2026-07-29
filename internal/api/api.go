package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/FanDoster/Build-System/internal/agents"
	"github.com/FanDoster/Build-System/internal/auth"
	"github.com/FanDoster/Build-System/internal/db"
	"github.com/FanDoster/Build-System/internal/live"
	"github.com/FanDoster/Build-System/internal/logbus"
	"github.com/FanDoster/Build-System/internal/models"
	"github.com/FanDoster/Build-System/internal/ws"
)

// maxWebhookBody caps webhook payload reads (GitHub push payloads are far smaller).
const maxWebhookBody = 1 << 20

// livePingInterval is how often an idle dashboard socket is pinged. It has to
// stay comfortably under the idle timeouts of intermediaries (nginx defaults
// to 60s of silence) or a quiet dashboard would be cut off every minute.
const livePingInterval = 25 * time.Second

// Version identifies the running build-server code. Bump it with any change
// that ships; /api/health returns it so a self-deploy can be confirmed live
// (the running container is only as new as the version it reports).
const Version = "2026-07-29-agent-pause"

// Agent long-poll defaults. The hold is deliberately under the 60s nginx
// defaults with room to spare: a claim request that outlives proxy_read_timeout
// comes back as a 504 and looks like a flapping agent. The interval is how
// often an idle hold re-checks the queue — one cheap indexed read per second
// per waiting agent.
const (
	DefaultAgentPollHold     = 30 * time.Second
	DefaultAgentPollInterval = time.Second
)

// RunnerControl is the runner surface the API needs (implemented by
// runner.Runner): canceling the in-flight build and reading its progress.
type RunnerControl interface {
	Cancel(buildID int64) bool
	Progress(buildID int64) (step string, ok bool)
}

// Notifier sends the build-completion mail for builds this server finishes on
// an agent's behalf. runner.Runner implements it; nil sends nothing.
type Notifier interface {
	NotifyFinished(buildID int64, status models.BuildStatus, startedAt, finishedAt time.Time)
}

type Server struct {
	DB      *db.DB
	BuildCh chan *models.Build
	Bus     *logbus.Bus
	Runner  RunnerControl
	// Live feeds the dashboard/project list pages. Optional: with a nil hub
	// /api/live 503s and the UI falls back to polling.
	Live     *live.Hub
	BasePath string // e.g. "/builds"
	// Notifier mails the outcome of agent-finished builds. Optional.
	Notifier Notifier
	// Agents remembers which agents have been in touch. An idle agent writes
	// nothing to the database, so this is the only record that one exists.
	// Optional; without it the agents page shows history only.
	Agents *agents.Registry

	// AgentPollHold/AgentPollInterval tune the claim long-poll; zero means the
	// defaults above. Tests set them small.
	AgentPollHold     time.Duration
	AgentPollInterval time.Duration

	// agentLogMu serialises the read-offset/publish/append sequence in
	// handleAgentLog. The bus buffer and the stored log have to stay
	// byte-identical, and that invariant spans three statements which two
	// concurrent uploads would interleave.
	agentLogMu sync.Mutex
}

type apiError struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, apiError{Error: msg})
}

func (s *Server) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/health", s.handleHealth)

	mux.HandleFunc("GET /api/projects", s.handleListProjects)
	mux.HandleFunc("POST /api/projects", s.handleCreateProject)
	mux.HandleFunc("GET /api/projects/{id}", s.handleGetProject)
	mux.HandleFunc("PUT /api/projects/{id}", s.handleUpdateProject)
	mux.HandleFunc("DELETE /api/projects/{id}", s.handleDeleteProject)

	mux.HandleFunc("POST /api/projects/{id}/build", s.handleTriggerBuild)
	mux.HandleFunc("GET /api/projects/{id}/builds", s.handleListProjectBuilds)
	mux.HandleFunc("GET /api/builds", s.handleListRecentBuilds)
	mux.HandleFunc("GET /api/builds/active", s.handleActiveBuilds)
	mux.HandleFunc("GET /api/live", s.handleLive)
	mux.HandleFunc("GET /api/builds/{id}", s.handleGetBuild)
	mux.HandleFunc("GET /api/builds/{id}/events", s.handleBuildEvents)
	mux.HandleFunc("GET /api/builds/{id}/log", s.handleBuildLog)
	mux.HandleFunc("POST /api/builds/{id}/cancel", s.handleCancelBuild)
	mux.HandleFunc("POST /api/builds/{id}/rerun", s.handleRerunBuild)

	// Build agents. Everything an agent does is a POST it initiates itself:
	// the Mac running these builds sits behind NAT, so nothing here ever
	// connects outwards to it.
	mux.HandleFunc("GET /api/agents", s.handleListAgents)
	mux.HandleFunc("POST /api/agents/claim", s.handleAgentClaim)
	// Operator controls. Registered after the literal /api/agents/claim above,
	// which the ServeMux prefers regardless of order — an agent named "claim"
	// cannot shadow the claim endpoint.
	mux.HandleFunc("POST /api/agents/{name}/pause", s.handlePauseAgent)
	mux.HandleFunc("POST /api/agents/{name}/resume", s.handleResumeAgent)
	mux.HandleFunc("DELETE /api/agents/{name}", s.handleForgetAgent)
	mux.HandleFunc("POST /api/builds/{id}/log", s.handleAgentLog)
	mux.HandleFunc("POST /api/builds/{id}/heartbeat", s.handleAgentHeartbeat)
	mux.HandleFunc("POST /api/builds/{id}/finish", s.handleAgentFinish)

	mux.HandleFunc("POST /api/webhook/github", s.handleGitHubWebhook)
}

// requireCsrf enforces the custom header on state-changing endpoints called
// from the UI. A custom header can't be sent by a plain cross-site form and
// forces a CORS preflight from scripts.
func requireCsrf(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("X-Builds-Csrf") != "1" {
		writeError(w, 403, "missing X-Builds-Csrf header")
		return false
	}
	return true
}

// requireOperator refuses agent credentials. An agent token lives in a file on
// a build machine so that machine can run builds; it must not also be able to
// change which repositories get built, or point a project at a different one.
func requireOperator(w http.ResponseWriter, r *http.Request) bool {
	if auth.IsAgent(r.Context()) {
		writeError(w, 403, "agent credentials cannot manage projects")
		return false
	}
	return true
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]string{"status": "ok", "version": Version})
}

// validExecutor accepts "" (meaning local), "local", or a queue name an agent
// can match on. The character restriction keeps executor names to things that
// read unambiguously in a config file and a URL — this is a routing key that
// has to agree exactly with a string typed into an agent's config.
func validExecutor(e string) error {
	if e == "" || e == models.ExecutorLocal {
		return nil
	}
	if len(e) > 32 {
		return fmt.Errorf("executor must be 32 characters or fewer")
	}
	for _, r := range e {
		if !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') && r != '-' && r != '_' {
			return fmt.Errorf("executor must be lowercase letters, digits, - or _ (got %q)", e)
		}
	}
	return nil
}

// --- Projects ---

func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	projects, err := s.DB.ListProjects()
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if projects == nil {
		projects = []models.Project{}
	}
	for i := range projects {
		projects[i].Sanitize()
	}
	writeJSON(w, 200, projects)
}

func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	if !requireCsrf(w, r) {
		return
	}
	if !requireOperator(w, r) {
		return
	}
	var p models.Project
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeError(w, 400, "invalid JSON: "+err.Error())
		return
	}
	if p.Name == "" || p.RepoURL == "" || p.ImageName == "" {
		writeError(w, 400, "name, repo_url, image_name are required")
		return
	}
	if p.Branch == "" {
		p.Branch = "main"
	}
	if p.DockerfilePath == "" {
		p.DockerfilePath = "Dockerfile"
	}
	if p.PollIntervalSecs == 0 {
		p.PollIntervalSecs = models.DefaultPollIntervalSecs
	}
	if p.PollIntervalSecs < models.MinPollIntervalSecs {
		writeError(w, 400, fmt.Sprintf("poll_interval_secs must be at least %d", models.MinPollIntervalSecs))
		return
	}
	if err := validExecutor(p.Executor); err != nil {
		writeError(w, 400, err.Error())
		return
	}

	if err := s.DB.CreateProject(&p); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			writeError(w, 409, "project name already exists")
			return
		}
		writeError(w, 500, err.Error())
		return
	}
	// Re-fetch to get secrets cleared from response
	created, _ := s.DB.GetProject(p.ID)
	created.Sanitize()
	writeJSON(w, 201, created)
}

func (s *Server) handleGetProject(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, 400, "invalid id")
		return
	}
	p, err := s.DB.GetProject(id)
	if err != nil {
		writeError(w, 404, "project not found")
		return
	}
	p.Sanitize()
	writeJSON(w, 200, p)
}

func (s *Server) handleUpdateProject(w http.ResponseWriter, r *http.Request) {
	if !requireCsrf(w, r) {
		return
	}
	if !requireOperator(w, r) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, 400, "invalid id")
		return
	}
	existing, err := s.DB.GetProject(id)
	if err != nil {
		writeError(w, 404, "project not found")
		return
	}

	// Pointer fields distinguish "not provided" (nil, keep current value)
	// from "provided as empty" (clear the value where that is allowed).
	var updates struct {
		Name              *string `json:"name"`
		RepoURL           *string `json:"repo_url"`
		Branch            *string `json:"branch"`
		DockerfilePath    *string `json:"dockerfile_path"`
		ImageName         *string `json:"image_name"`
		DeployComposePath *string `json:"deploy_compose_path"`
		DeployServiceName *string `json:"deploy_service_name"`
		WebhookSecret     *string `json:"webhook_secret"`
		CloneToken        *string `json:"clone_token"`
		NoCache           *bool   `json:"no_cache"`
		PollEnabled       *bool   `json:"poll_enabled"`
		PollIntervalSecs  *int    `json:"poll_interval_secs"`
		Executor          *string `json:"executor"`
	}
	if err := json.NewDecoder(r.Body).Decode(&updates); err != nil {
		writeError(w, 400, "invalid JSON: "+err.Error())
		return
	}

	// Required fields may be updated but not cleared.
	for field, v := range map[string]*string{
		"name": updates.Name, "repo_url": updates.RepoURL, "branch": updates.Branch,
		"dockerfile_path": updates.DockerfilePath, "image_name": updates.ImageName,
	} {
		if v != nil && *v == "" {
			writeError(w, 400, field+" cannot be empty")
			return
		}
	}

	if updates.PollIntervalSecs != nil && *updates.PollIntervalSecs < models.MinPollIntervalSecs {
		writeError(w, 400, fmt.Sprintf("poll_interval_secs must be at least %d", models.MinPollIntervalSecs))
		return
	}
	if updates.Executor != nil {
		if err := validExecutor(*updates.Executor); err != nil {
			writeError(w, 400, err.Error())
			return
		}
	}

	// Snapshot the executor before the updates are applied: moving a project
	// off a remote executor has to adopt the builds that were waiting for its
	// agent.
	before := *existing

	// The recorded poll baseline belongs to a specific ref; if either half of
	// that ref changes, the stored SHA is meaningless and must not be compared
	// against the new ref's tip (which would queue a build for a commit that
	// is simply "not the old one").
	refChanged := (updates.RepoURL != nil && *updates.RepoURL != existing.RepoURL) ||
		(updates.Branch != nil && *updates.Branch != existing.Branch)

	setIf := func(dst *string, src *string) {
		if src != nil {
			*dst = *src
		}
	}
	setIf(&existing.Name, updates.Name)
	setIf(&existing.RepoURL, updates.RepoURL)
	setIf(&existing.Branch, updates.Branch)
	setIf(&existing.DockerfilePath, updates.DockerfilePath)
	setIf(&existing.ImageName, updates.ImageName)
	setIf(&existing.DeployComposePath, updates.DeployComposePath)
	setIf(&existing.DeployServiceName, updates.DeployServiceName)
	setIf(&existing.WebhookSecret, updates.WebhookSecret)
	setIf(&existing.CloneToken, updates.CloneToken)
	if updates.NoCache != nil {
		existing.NoCache = *updates.NoCache
	}
	if updates.PollEnabled != nil {
		existing.PollEnabled = *updates.PollEnabled
	}
	if updates.PollIntervalSecs != nil {
		existing.PollIntervalSecs = *updates.PollIntervalSecs
	}
	if updates.Executor != nil {
		existing.Executor = *updates.Executor
		if existing.Executor == "" {
			existing.Executor = models.ExecutorLocal
		}
	}

	// Moving a project back to the local runner has to bring its waiting
	// builds with it. A remote project's pending rows were deliberately never
	// put on the channel — the agent was going to claim them — so after the
	// switch nobody owns them, and they would sit pending until the next
	// server restart happened to re-queue them.
	adopt := models.Remote(existing.Executor) == false && models.Remote(before.Executor)

	if err := s.DB.UpdateProject(existing); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if adopt {
		pending, err := s.DB.ListBuildsByStatus(models.StatusPending)
		if err == nil {
			for i := range pending {
				if pending[i].ProjectID == id {
					s.enqueue(&pending[i])
				}
			}
		}
	}
	if refChanged {
		if err := s.DB.ResetPollState(id); err != nil {
			writeError(w, 500, err.Error())
			return
		}
	}
	// Re-fetch without secrets
	updated, _ := s.DB.GetProject(id)
	updated.Sanitize()
	writeJSON(w, 200, updated)
}

func (s *Server) handleDeleteProject(w http.ResponseWriter, r *http.Request) {
	if !requireCsrf(w, r) {
		return
	}
	if !requireOperator(w, r) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, 400, "invalid id")
		return
	}
	if err := s.DB.DeleteProject(id); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"status": "deleted"})
}

// --- Builds ---

func (s *Server) handleTriggerBuild(w http.ResponseWriter, r *http.Request) {
	if !requireCsrf(w, r) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, 400, "invalid id")
		return
	}
	project, err := s.DB.GetProject(id)
	if err != nil {
		writeError(w, 404, "project not found")
		return
	}

	build := &models.Build{
		ProjectID:     project.ID,
		Status:        models.StatusPending,
		CommitSHA:     "manual",
		CommitMessage: "Manual trigger",
	}
	if err := s.DB.CreateBuild(build); err != nil {
		writeError(w, 500, err.Error())
		return
	}

	if !s.dispatch(project, build) {
		writeError(w, 503, "build queue is full, try again later")
		return
	}

	writeJSON(w, 201, build)
}

// dispatch hands a newly created build to whoever will run it.
//
// A build for a remote executor is deliberately NOT put on the local channel:
// its pending row IS the queue, and the agent serving that executor claims it
// over HTTP whenever it next polls. This is the one gate that keeps a Unity
// build from being handed to the Docker runner, so every path that creates a
// build goes through here.
func (s *Server) dispatch(project *models.Project, build *models.Build) bool {
	if models.Remote(project.Executor) {
		return true
	}
	return s.enqueue(build)
}

// enqueue attempts a non-blocking send to the build channel. On a full queue
// it marks the build failed rather than blocking the HTTP handler forever.
func (s *Server) enqueue(build *models.Build) bool {
	select {
	case s.BuildCh <- build:
		return true
	default:
		s.DB.UpdateBuildStatus(build.ID, models.StatusFailed, "Build not started: queue is full\n")
		return false
	}
}

func (s *Server) handleListProjectBuilds(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, 400, "invalid id")
		return
	}
	builds, err := s.DB.ListBuildsByProject(id, 0)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if builds == nil {
		builds = []models.Build{}
	}
	writeJSON(w, 200, builds)
}

func (s *Server) handleListRecentBuilds(w http.ResponseWriter, r *http.Request) {
	builds, err := s.DB.ListRecentBuilds(30)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if builds == nil {
		builds = []models.Build{}
	}
	writeJSON(w, 200, builds)
}

func (s *Server) handleGetBuild(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, 400, "invalid id")
		return
	}
	build, err := s.DB.GetBuild(id)
	if err != nil {
		writeError(w, 404, "build not found")
		return
	}
	if r.URL.Query().Get("meta") == "1" {
		build.LogLen = int64(len(build.Log))
		// The live bus buffer can be ahead of the batched DB writes.
		if _, cur, ok := s.Bus.LogTail(id, math.MaxInt); ok && int64(cur) > build.LogLen {
			build.LogLen = int64(cur)
		}
		build.Log = ""
		s.decorateProgress(build)
	}
	writeJSON(w, 200, build)
}

// decorateProgress fills the computed live-progress fields of an active build
// — queue position, current step, expected duration. The WebSocket feed pushes
// the same fields, so the logic lives in internal/live and both use it.
func (s *Server) decorateProgress(b *models.Build) {
	live.Decorate(s.DB, s.Runner, b)
}

// handleActiveBuilds returns all pending and running builds, log-free, with
// live progress — one cheap request for list pages to poll.
func (s *Server) handleActiveBuilds(w http.ResponseWriter, r *http.Request) {
	running, err := s.DB.ListBuildsByStatus(models.StatusRunning)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	pending, err := s.DB.ListBuildsByStatus(models.StatusPending)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}

	active := make([]models.Build, 0, len(running)+len(pending))
	for _, b := range append(running, pending...) {
		b.Log = ""
		s.decorateProgress(&b)
		active = append(active, b)
	}
	writeJSON(w, 200, active)
}

// handleLive serves the dashboard feed: the recent builds with live progress,
// pushed on change. A WebSocket upgrade gets a stream; a plain GET gets one
// snapshot of the identical payload, which is both the fallback for a proxy
// that will not forward Upgrade and the easiest way to inspect the feed by
// hand (curl /api/live).
//
// Reads only. Nothing the client sends is interpreted, which is why the
// endpoint needs no CSRF header — but it does need the origin check, since
// WebSocket has no same-origin policy of its own.
func (s *Server) handleLive(w http.ResponseWriter, r *http.Request) {
	if s.Live == nil {
		writeError(w, 503, "live feed not configured")
		return
	}
	if !ws.IsUpgrade(r) {
		snapshot, err := s.Live.Snapshot()
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(snapshot)
		return
	}
	if !ws.SameOrigin(r) {
		writeError(w, 403, "cross-origin websocket rejected")
		return
	}
	conn, err := ws.Upgrade(w, r)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	defer conn.Close()

	updates, unsub := s.Live.Subscribe()
	defer unsub()

	// The read loop enforces the idle timeout and answers pings. It also gives
	// this handler its disconnect signal: a client that closes or dies ends the
	// loop, which closes done, which returns from here.
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn.ReadLoop()
	}()

	ping := time.NewTicker(livePingInterval)
	defer ping.Stop()

	for {
		select {
		case <-done:
			return
		case <-ping.C:
			// Also the liveness probe for the write path: a peer that vanished
			// without a FIN (a dropped NAT mapping) surfaces here as an error.
			if err := conn.Ping(); err != nil {
				return
			}
		case snapshot, open := <-updates:
			if !open {
				return // dropped as a slow subscriber; the client reconnects
			}
			if err := conn.WriteText(snapshot); err != nil {
				return
			}
		}
	}
}

// handleBuildLog serves the raw scrubbed log: full text, ?download=1
// attachment, or ?offset=N incremental tail (the polling fallback for SSE).
func (s *Server) handleBuildLog(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, 400, "invalid id")
		return
	}
	build, err := s.DB.GetBuild(id)
	if err != nil {
		writeError(w, 404, "build not found")
		return
	}

	offset := 0
	if v := r.URL.Query().Get("offset"); v != "" {
		offset, err = strconv.Atoi(v)
		if err != nil || offset < 0 {
			writeError(w, 400, "invalid offset")
			return
		}
	}

	// Prefer the live bus buffer (ahead of batched DB writes); fall back to
	// the stored row for finished or pre-streaming builds.
	body, total, ok := s.Bus.LogTail(id, offset)
	if !ok || total == 0 {
		full := build.Log
		total = len(full)
		if offset > total {
			offset = total
		}
		body = []byte(full[offset:])
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Log-Offset", strconv.Itoa(total))
	w.Header().Set("X-Build-Status", string(build.Status))
	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Disposition",
			fmt.Sprintf(`attachment; filename="%s-build-%d.log"`, asciiFilename(build.ProjectName), build.ID))
	}
	w.WriteHeader(200)
	w.Write(body)
}

// handleBuildEvents streams a build's log and status transitions over SSE.
// Replay is race-free: Subscribe atomically returns the buffered bytes from
// the resume offset plus a live channel.
func (s *Server) handleBuildEvents(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, 400, "invalid id")
		return
	}
	build, err := s.DB.GetBuild(id)
	if err != nil {
		writeError(w, 404, "build not found")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, 500, "streaming unsupported")
		return
	}

	// Resume offset: browser-set Last-Event-ID (auto-reconnect) wins over ?offset.
	from := 0
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			from = n
		}
	} else if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			from = n
		}
	}

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	fmt.Fprint(w, "retry: 2000\n\n")

	writeStatus := func(status models.BuildStatus, startedAt, finishedAt *time.Time) {
		data, _ := json.Marshal(map[string]interface{}{
			"status":      status,
			"started_at":  startedAt,
			"finished_at": finishedAt,
		})
		fmt.Fprintf(w, "event: status\ndata: %s\n\n", data)
	}
	writeLog := func(chunk string, offset int) {
		// JSON-wrapped so \r and \n inside the chunk survive SSE framing.
		data, _ := json.Marshal(map[string]interface{}{"o": offset, "t": chunk})
		fmt.Fprintf(w, "event: log\nid: %d\ndata: %s\n\n", offset, data)
	}
	// dbTail slices the stored log at the resume offset.
	dbTail := func(b *models.Build) (string, int) {
		f := from
		if f > len(b.Log) {
			f = len(b.Log)
		}
		return b.Log[f:], len(b.Log)
	}
	// Terminal ordering contract: log bytes are always written BEFORE the
	// terminal status event — the client closes its EventSource on a
	// terminal status and would drop anything after it.
	writeTerminal := func(b *models.Build, chunk string, offset int) {
		if chunk != "" {
			writeLog(chunk, offset)
		}
		writeStatus(b.Status, b.StartedAt, b.FinishedAt)
		flusher.Flush()
	}

	// Already-terminal builds are served straight from storage without
	// subscribing — Subscribe would create a topic no terminal transition
	// will ever clean up.
	if build.Status.Terminal() {
		chunk, total := "", 0
		if tail, cur, ok := s.Bus.LogTail(id, from); ok && cur > 0 {
			chunk, total = string(tail), cur
		} else {
			chunk, total = dbTail(build)
		}
		writeTerminal(build, chunk, total)
		return
	}

	// Subscribe before re-reading state so no transition can slip between.
	snapshot, cur, ch, unsub := s.Bus.Subscribe(id, from)
	defer unsub()

	build, err = s.DB.GetBuild(id)
	if err != nil {
		return
	}

	// Replay: bus buffer when it has data, else the stored log (finished or
	// pre-streaming builds whose topic buffer is empty).
	replay, total := string(snapshot), cur
	if cur == 0 && len(build.Log) > 0 {
		replay, total = dbTail(build)
	}

	if build.Status.Terminal() {
		// The build finished between Subscribe and the re-read. Any final
		// log chunks are already queued on ch (delivered before close) —
		// drain them so the client doesn't lose the tail of the log.
	drain:
		for {
			select {
			case ev, open := <-ch:
				if !open {
					break drain
				}
				if ev.Kind == "log" {
					replay += ev.Chunk
					total = ev.Offset
				}
			default:
				break drain
			}
		}
		writeTerminal(build, replay, total)
		return
	}

	writeStatus(build.Status, build.StartedAt, build.FinishedAt)
	if len(replay) > 0 {
		writeLog(replay, total)
	}
	flusher.Flush()
	sent := total // last log offset delivered to this client

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			// A named event (not an SSE comment) so the client-side
			// stall watchdog can observe it.
			fmt.Fprint(w, "event: ping\ndata: {}\n\n")
			flusher.Flush()
		case ev, open := <-ch:
			if !open {
				// Dropped as a slow subscriber or topic closed. If the build
				// is terminal, backfill any log bytes this client never got
				// before the final status; otherwise the client reconnects
				// with Last-Event-ID and resumes.
				if b, err := s.DB.GetBuild(id); err == nil && b.Status.Terminal() {
					chunk, offset := "", sent
					if tail, cur, ok := s.Bus.LogTail(id, sent); ok && len(tail) > 0 {
						chunk, offset = string(tail), cur
					} else if len(b.Log) > sent {
						chunk, offset = b.Log[sent:], len(b.Log)
					}
					writeTerminal(b, chunk, offset)
				}
				return
			}
			switch ev.Kind {
			case "log":
				writeLog(ev.Chunk, ev.Offset)
				sent = ev.Offset
			case "status":
				// Per-subscriber FIFO: all log chunks published through the
				// bus before a terminal status are already delivered above.
				// Bytes appended straight to the row are not — a sweep writes
				// its reason in SQL, and can only mirror it onto a buffer that
				// still holds the build's log, which after a restart it does
				// not. Backfill from the row so the stream never ends on a
				// bare "failed" with the explanation sitting in the database.
				if ev.Status.Terminal() {
					if tail := s.storedTail(id, sent); tail != "" {
						writeLog(tail, sent+len(tail))
						sent += len(tail)
					}
				}
				writeStatus(ev.Status, ev.StartedAt, ev.FinishedAt)
			}
			flusher.Flush()
			if ev.Kind == "status" && ev.Status.Terminal() {
				return
			}
		}
	}
}

// storedTail returns the stored log past the byte offset a subscriber has
// already been sent, or "" when it is up to date. Reads the length and only
// the tail rather than the whole row: on an ordinary build this runs once per
// stream and finds nothing, and the row can be tens of megabytes.
func (s *Server) storedTail(id int64, sent int) string {
	n, err := s.DB.BuildLogLen(id)
	if err != nil || n <= sent {
		return ""
	}
	tail, err := s.DB.BuildLogSlice(id, sent, n-sent)
	if err != nil {
		return ""
	}
	return string(tail)
}

// handleCancelBuild cancels a queued or running build. Race-safe against the
// runner: the pending row is tombstoned atomically, and the runner registers
// its cancel func before claiming — so a cancel always lands on one or the other.
func (s *Server) handleCancelBuild(w http.ResponseWriter, r *http.Request) {
	if !requireCsrf(w, r) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, 400, "invalid id")
		return
	}

	if ok, err := s.DB.CancelPendingBuild(id); err != nil {
		writeError(w, 500, err.Error())
		return
	} else if ok {
		// Mirror the tombstone the SQL appended to the DB log so the bus
		// buffer and stored log stay byte-identical for live subscribers.
		s.Bus.Publish(id, []byte("[canceled while queued]\n"))
		now := time.Now().UTC()
		s.Bus.PublishStatus(id, models.StatusCanceled, nil, &now)
		writeJSON(w, 200, map[string]interface{}{"id": id, "status": models.StatusCanceled})
		return
	}

	if s.Runner != nil && s.Runner.Cancel(id) {
		// The runner writes the terminal row; clients observe it via SSE.
		// Guard the small window where the build finished but the runner
		// had not yet cleared its registry.
		if b, err := s.DB.GetBuild(id); err == nil && b.Status.Terminal() && b.Status != models.StatusCanceled {
			writeError(w, 409, "build already finished")
			return
		}
		writeJSON(w, 202, map[string]interface{}{"id": id, "status": "canceling"})
		return
	}

	// Summary, not GetBuild: only the status and owner are read, and an agent
	// build's log can be tens of megabytes.
	build, err := s.DB.GetBuildSummary(id)
	if err != nil {
		writeError(w, 404, "build not found")
		return
	}
	switch build.Status {
	case models.StatusCanceled:
		writeJSON(w, 200, map[string]interface{}{"id": id, "status": models.StatusCanceled, "already": true})
	case models.StatusSuccess, models.StatusFailed:
		writeError(w, 409, "build already finished")
	case models.StatusRunning:
		// Running on an agent: nothing here can reach into that machine, so
		// the cancel is left as a flag the agent reads off its next heartbeat
		// or log-append response. The build stays running until the agent
		// reports back — 202, not 200.
		if build.Agent != "" {
			if ok, err := s.DB.RequestAgentCancel(id); err != nil {
				writeError(w, 500, err.Error())
				return
			} else if ok {
				writeJSON(w, 202, map[string]interface{}{"id": id, "status": "canceling", "agent": build.Agent})
				return
			}
		}
		writeError(w, 409, "build could not be canceled, try again")
	default:
		writeError(w, 409, "build could not be canceled, try again")
	}
}

// handleRerunBuild creates a fresh build for the same project and ref.
func (s *Server) handleRerunBuild(w http.ResponseWriter, r *http.Request) {
	if !requireCsrf(w, r) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, 400, "invalid id")
		return
	}
	src, err := s.DB.GetBuildSummary(id)
	if err != nil {
		writeError(w, 404, "build not found")
		return
	}
	project, err := s.DB.GetProject(src.ProjectID)
	if err != nil {
		writeError(w, 404, "project no longer exists")
		return
	}

	build := &models.Build{
		ProjectID:     src.ProjectID,
		Status:        models.StatusPending,
		CommitSHA:     src.CommitSHA,
		CommitMessage: truncate(fmt.Sprintf("Re-run of #%d: %s", src.ID, src.CommitMessage), 100),
	}
	if err := s.DB.CreateBuild(build); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if !s.dispatch(project, build) {
		writeError(w, 503, "build queue is full, try again later")
		return
	}
	writeJSON(w, 201, build)
}

// --- GitHub Webhook ---

func (s *Server) handleGitHubWebhook(w http.ResponseWriter, r *http.Request) {
	// Parse the event
	event := r.Header.Get("X-GitHub-Event")
	if event != "push" {
		writeJSON(w, 200, map[string]string{"status": "ignored", "reason": fmt.Sprintf("event=%s, only push is handled", event)})
		return
	}

	// Keep the raw body: signature validation is HMAC over the exact bytes.
	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBody))
	if err != nil {
		writeError(w, 400, "failed to read payload")
		return
	}

	var payload struct {
		Ref        string `json:"ref"`
		Repository struct {
			CloneURL string `json:"clone_url"`
			FullName string `json:"full_name"`
		} `json:"repository"`
		HeadCommit *struct {
			ID      string `json:"id"`
			Message string `json:"message"`
		} `json:"head_commit"`
	}

	if err := json.Unmarshal(body, &payload); err != nil {
		writeError(w, 400, "invalid payload")
		return
	}

	// Branch deletions and some tag pushes have no head_commit.
	if payload.HeadCommit == nil || payload.HeadCommit.ID == "" {
		writeJSON(w, 200, map[string]string{"status": "ignored", "reason": "no head_commit in payload"})
		return
	}

	// Find matching project(s) by repo URL
	projects, err := s.DB.ListProjects()
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}

	signature := r.Header.Get("X-Hub-Signature-256")
	branch := strings.TrimPrefix(payload.Ref, "refs/heads/")
	var matched []models.Project
	rejected := 0
	for _, p := range projects {
		if p.Branch != branch || !repoURLMatch(p.RepoURL, payload.Repository.CloneURL) {
			continue
		}
		// ListProjects omits secrets; fetch the full row for validation.
		full, err := s.DB.GetProject(p.ID)
		if err != nil {
			continue
		}
		if full.WebhookSecret != "" && !validSignature(full.WebhookSecret, body, signature) {
			rejected++
			continue
		}
		matched = append(matched, *full)
	}

	if len(matched) == 0 {
		if rejected > 0 {
			writeError(w, 403, "invalid webhook signature")
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ignored", "reason": "no matching project"})
		return
	}

	sha := payload.HeadCommit.ID
	if len(sha) > 12 {
		sha = sha[:12]
	}

	var created []int64
	for _, project := range matched {
		build := &models.Build{
			ProjectID:     project.ID,
			Status:        models.StatusPending,
			CommitSHA:     sha,
			CommitMessage: truncate(payload.HeadCommit.Message, 100),
		}
		if err := s.DB.CreateBuild(build); err != nil {
			continue
		}
		if !s.dispatch(&project, build) {
			continue
		}
		created = append(created, build.ID)
	}

	writeJSON(w, 200, map[string]interface{}{
		"status":  "queued",
		"builds":  created,
		"project": matched[0].Name,
	})
}

// validSignature checks a GitHub X-Hub-Signature-256 header (constant-time).
func validSignature(secret string, body []byte, sigHeader string) bool {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(sigHeader))
}

// asciiFilename reduces a name to header-safe ASCII for Content-Disposition
// (raw non-ASCII or quote bytes would produce an invalid header).
func asciiFilename(name string) string {
	var b strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "build"
	}
	return b.String()
}

func repoURLMatch(configured, webhook string) bool {
	// Normalize: strip .git suffix, strip https:// prefix
	norm := func(s string) string {
		s = strings.TrimPrefix(s, "https://")
		s = strings.TrimPrefix(s, "http://")
		s = strings.TrimSuffix(s, ".git")
		return s
	}
	return norm(configured) == norm(webhook)
}

// truncate returns the first line of s, capped at n runes (not bytes, so
// multi-byte characters are never split).
func truncate(s string, n int) string {
	s = strings.SplitN(s, "\n", 2)[0]
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	if n <= 3 {
		return string(runes[:n])
	}
	return string(runes[:n-3]) + "..."
}
