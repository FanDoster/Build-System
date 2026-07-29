package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/FanDoster/Build-System/internal/agents"
	"github.com/FanDoster/Build-System/internal/db"
	"github.com/FanDoster/Build-System/internal/models"
)

// Build-agent protocol.
//
// The server never connects to an agent — the machines that run Unity builds
// sit behind NAT, and even when they don't, an inbound-connection design turns
// every agent into a service someone has to expose. So every exchange is a
// POST the agent initiates:
//
//	claim     long-poll until a build for one of its executors is available
//	log       append output at a byte offset, learn whether a cancel is pending
//	heartbeat prove the agent is alive when the build is producing no output
//	finish    report the terminal status
//
// Two properties hold everything together. Claims are a compare-and-swap, so
// two agents polling at once cannot take the same build. And every response
// the agent already makes carries the cancel flag, so a cancel needs no
// channel of its own — which matters because a Unity compile can go minutes
// without printing anything, and cancel latency would otherwise be unbounded.
//
// Agents emit the runner's LOG GRAMMAR themselves (see runner.go): the step
// rail, error highlighting and live log in the web UI are entirely a function
// of the bytes in the log, so an agent that speaks the grammar gets all of it
// without a line of UI code.

// agentIdent is the identity every agent request carries. There is no
// registration handshake: the token in the Authorization header is the
// credential, and the name is for display and ownership checks.
type agentIdent struct {
	Agent string `json:"agent"`
}

// maxAgentLogBody caps one log upload. Agents flush on a timer and a size
// bound well under this, so hitting it means something has gone wrong on the
// other end — and an unbounded read would let one bad chunk pull the whole
// server's memory with it, given the body is buffered into a string and then
// concatenated onto the row. maxAgentBody covers the small control requests.
const (
	maxAgentLogBody = 4 << 20 // 4 MiB
	maxAgentBody    = 1 << 16 // 64 KiB
)

// decodeAgentBody reads a size-capped JSON body, answering the request itself
// on failure. An over-long body is 413, not 400: the agent should split it and
// retry, not treat its own payload as malformed.
func decodeAgentBody(w http.ResponseWriter, r *http.Request, limit int64, dst interface{}) bool {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("request body exceeds %d bytes — send the log in smaller chunks", limit))
			return false
		}
		writeError(w, 400, "invalid JSON: "+err.Error())
		return false
	}
	return true
}

func (s *Server) pollHold() time.Duration {
	if s.AgentPollHold > 0 {
		return s.AgentPollHold
	}
	return DefaultAgentPollHold
}

func (s *Server) pollInterval() time.Duration {
	if s.AgentPollInterval > 0 {
		return s.AgentPollInterval
	}
	return DefaultAgentPollInterval
}

// handleAgentClaim long-polls for work. 204 means "nothing for you, ask
// again"; 200 carries the build plus the FULL project row, clone token
// included, because the agent has to authenticate the same private clone the
// local runner would have.
func (s *Server) handleAgentClaim(w http.ResponseWriter, r *http.Request) {
	if !requireCsrf(w, r) {
		return
	}
	var req struct {
		agentIdent
		Executors []string `json:"executors"`
		// The self block. Optional in every sense: an agent too old to send it
		// is decoded without complaint (nothing here sets DisallowUnknownFields,
		// in either direction), and an absent or zero field leaves whatever is
		// already stored alone rather than blanking it.
		Version     string     `json:"version"`
		OSArch      string     `json:"os_arch"`
		StartedAt   *time.Time `json:"started_at"`
		DiskFreeGB  *int       `json:"disk_free_gb"`
		DiskFloorGB *int       `json:"disk_floor_gb"`
	}
	if !decodeAgentBody(w, r, maxAgentBody, &req) {
		return
	}
	if req.Agent == "" {
		writeError(w, 400, "agent is required")
		return
	}
	// Bounded before anything retains it. The name becomes a primary key and
	// the executor list is copied into the registry and kept for the life of
	// the process, so an unbounded body here is unbounded server memory — and
	// the caller chooses both. Deliberately permissive: this rejects the
	// absurd, not the unusual, because tightening naming under a deployed fleet
	// would turn a cosmetic choice into a claim that fails and CI that stops.
	if !models.ValidAgentName(req.Agent) {
		writeError(w, 400, "agent name must be 1-"+strconv.Itoa(models.MaxAgentNameLen)+
			" characters, with no control characters or surrounding space")
		return
	}
	if len(req.Executors) == 0 {
		writeError(w, 400, "executors is required")
		return
	}
	if len(req.Executors) > maxAgentExecutors {
		writeError(w, 400, "at most "+strconv.Itoa(maxAgentExecutors)+" executors")
		return
	}
	for _, e := range req.Executors {
		// "local" is the in-process Docker runner's queue. Letting an agent
		// claim from it would hand Dockerfile builds to a machine that has no
		// business running them, and silently starve the local worker.
		if !models.Remote(e) {
			writeError(w, 400, "executor "+strconv.Quote(e)+" is reserved for the local runner")
			return
		}
	}

	// Recorded before the wait, and closed however this returns. An empty poll
	// is the only trace an idle agent leaves anywhere.
	if s.Agents != nil {
		defer s.Agents.PollStarted(req.Agent, req.Executors, requestScheme(r))()
		s.rememberAgent(req.Agent, req.Executors, requestScheme(r), db.SelfReport{
			Version:     req.Version,
			OSArch:      req.OSArch,
			StartedAt:   req.StartedAt,
			DiskFreeGB:  req.DiskFreeGB,
			DiskFloorGB: req.DiskFloorGB,
		})
	}

	deadline := time.Now().Add(s.pollHold())
	ticker := time.NewTicker(s.pollInterval())
	defer ticker.Stop()

	for {
		// Checked every tick rather than once, so a pause takes effect at the
		// moment of claiming rather than up to a poll later. An operator who
		// pauses because they are about to unplug the machine gets what they
		// asked for, instead of one more build starting twenty seconds after.
		//
		// The cost is one indexed single-row read per tick — and the loop
		// already performs a write on every tick, so this adds no new order of
		// expense. Do not "optimise" it to once per poll without reading the
		// paragraph above.
		if s.agentIsPaused(req.Agent) {
			// Fall through to the wait rather than answering now. A paused
			// agent must keep its normal poll cadence: returning immediately
			// would turn it into a one-request-per-second loop against the
			// server, and would make its presence flicker on the very page
			// that is supposed to show it as calmly connected-but-paused.
			if time.Now().After(deadline) {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
			}
			continue
		}

		build, err := s.DB.ClaimBuildForAgent(req.Agent, req.Executors)
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		if build != nil {
			project, err := s.DB.GetProject(build.ProjectID)
			if err != nil {
				writeError(w, 500, err.Error())
				return
			}
			// Where the agent's first log append belongs. Usually 0, but a
			// build can arrive carrying an earlier attempt's output — it was
			// requeued while its project still built locally, and the project
			// moved to a remote executor before anyone claimed it. Telling the
			// agent the offset up front beats having it discover the mismatch.
			logOffset, err := s.DB.BuildLogLen(build.ID)
			if err != nil {
				writeError(w, 500, err.Error())
				return
			}
			// Opens the topic and moves the dashboard to "running" the moment
			// the build is claimed, not when its first log line lands.
			s.Bus.PublishStatus(build.ID, models.StatusRunning, build.StartedAt, nil)
			log.Printf("Agent %s claimed build %d (%s)", req.Agent, build.ID, project.Name)
			writeJSON(w, 200, map[string]interface{}{
				"build": build, "project": project, "log_offset": logOffset,
			})
			return
		}

		if time.Now().After(deadline) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		select {
		case <-r.Context().Done():
			// The agent hung up or the hold was cut short by a proxy. Nothing
			// was claimed, so there is nothing to unwind.
			return
		case <-ticker.C:
		}
	}
}

// handleListAgents serves the fleet: who is here, what they serve, what they
// are doing, and which executors nothing is serving.
//
// requireOperator, unlike the rest of the agent endpoints: an agent's own token
// authenticates against anything that does not check, and this discloses every
// machine and every queue rather than the one build the caller is working on.
func (s *Server) handleListAgents(w http.ResponseWriter, r *http.Request) {
	if !requireOperator(w, r) {
		return
	}
	if s.Agents == nil {
		writeError(w, 503, "agent registry not configured")
		return
	}
	fleet, err := agents.Build(s.DB, s.Agents, time.Now())
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, fleet)
}

// handleAgentStatus takes an agent's report on its own health.
//
// Deliberately NOT requireOperator, unlike every other endpoint on this page:
// this is the machine describing itself, and the credential it holds is the
// agent token. Deliberately not on the heartbeat either — the heartbeat exists
// only while a build runs, so it would deliver nothing while the agent is idle,
// which is exactly when somebody is asking what is wrong with it. And
// deliberately not on the claim poll, which fires twice a minute forever and
// has to stay small.
//
// Everything here is written by the machine being described. It is bounded on
// arrival by AgentStatus.Clamp and escaped on the way out; nothing in it is
// ever treated as a fact about the server.
func (s *Server) handleAgentStatus(w http.ResponseWriter, r *http.Request) {
	if !requireCsrf(w, r) {
		return
	}
	var req struct {
		agentIdent
		models.AgentStatus
	}
	if !decodeAgentBody(w, r, maxAgentBody, &req) {
		return
	}
	if !models.ValidAgentName(req.Agent) {
		writeError(w, 400, "invalid agent name")
		return
	}
	status := req.AgentStatus
	if err := s.DB.RecordAgentStatus(req.Agent, &status, time.Now()); err != nil {
		// Logged, not returned. A status report is information, and an agent
		// that treats a failure to file one as a reason to stop working would
		// have turned a reporting feature into an outage.
		log.Printf("agent %s: could not record status: %v", req.Agent, err)
	}
	w.WriteHeader(http.StatusNoContent)
}

// handlePauseAgent stops an agent being given new builds.
//
// requireOperator, like every endpoint on this page. Pause is an availability
// control — it is the one call in this API that can stop CI — and an agent
// token lives on a build machine. A machine that could pause the fleet, or
// pause its neighbours, would be a much more interesting thing to compromise
// than a machine that can only build.
//
// The running build, if there is one, is untouched. Pause means "take no more
// work", and killing a Unity build that may already be uploading to Steam is
// not something an operator asking for a pause has asked for.
func (s *Server) handlePauseAgent(w http.ResponseWriter, r *http.Request) {
	if !requireOperator(w, r) || !requireCsrf(w, r) {
		return
	}
	name := r.PathValue("name")
	if !models.ValidAgentName(name) {
		writeError(w, 400, "invalid agent name")
		return
	}
	var req struct {
		Minutes int    `json:"minutes"`
		Note    string `json:"note"`
	}
	if !decodeAgentBody(w, r, maxAgentBody, &req) {
		return
	}
	// The expiry is required, not defaulted. An open-ended pause is a dead CI
	// that looks healthy: the person who pauses to update Unity is the person
	// who will forget, and nobody investigates a pause they did not set. The
	// cap means even a bad value cannot outlive the week.
	// Bounded in minutes, BEFORE converting to a Duration. Checking the
	// converted value instead lets the multiply overflow int64 first: minutes
	// above about 1.5e8 wrap, a wrap into the negative passes both a
	// "greater than zero" test on the input and a "not more than a week" test
	// on the product, and the operator is told 200 for a pause that expired in
	// 1822. It fails open, so nothing gets stuck paused — but an endpoint that
	// answers 200 for work it did not do is its own bug.
	maxMinutes := int(models.MaxPauseDuration / time.Minute)
	if req.Minutes <= 0 {
		writeError(w, 400, "minutes must be greater than zero — a pause has to expire on its own")
		return
	}
	if req.Minutes > maxMinutes {
		writeError(w, 400, fmt.Sprintf("a pause may last at most %d hours",
			int(models.MaxPauseDuration.Hours())))
		return
	}
	now := time.Now()
	until := now.Add(time.Duration(req.Minutes) * time.Minute)
	if err := s.DB.PauseAgent(name, until, req.Note); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	log.Printf("Agent %s paused until %s", name, until.Format(time.RFC3339))
	writeJSON(w, 200, map[string]interface{}{"agent": name, "paused_until": until})
}

// handleResumeAgent lets an agent take work again.
func (s *Server) handleResumeAgent(w http.ResponseWriter, r *http.Request) {
	if !requireOperator(w, r) || !requireCsrf(w, r) {
		return
	}
	name := r.PathValue("name")
	if !models.ValidAgentName(name) {
		writeError(w, 400, "invalid agent name")
		return
	}
	if err := s.DB.ResumeAgent(name); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	log.Printf("Agent %s resumed", name)
	writeJSON(w, 200, map[string]interface{}{"agent": name, "paused_until": nil})
}

// handleForgetAgent removes an agent's record.
//
// Ships with the table because a name is self-asserted: one typo in a config
// file creates a row nothing else would ever remove. It clears the record, not
// the history — the machine reappears from its past builds until those age out,
// and reappears immediately if it is still polling, which is the correct
// outcome for a forget aimed at the wrong name.
func (s *Server) handleForgetAgent(w http.ResponseWriter, r *http.Request) {
	if !requireOperator(w, r) || !requireCsrf(w, r) {
		return
	}
	name := r.PathValue("name")
	if name == "" {
		writeError(w, 400, "invalid agent name")
		return
	}
	if err := s.DB.ForgetAgent(name); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if s.Agents != nil {
		s.Agents.Forget(name)
	}
	log.Printf("Agent %s forgotten", name)
	w.WriteHeader(http.StatusNoContent)
}

// maxAgentExecutors bounds the queue list one claim may advertise. The registry
// keeps this slice for the life of the process, keyed by a name the caller also
// chooses, so it is retained memory an unauthenticated-by-name caller controls.
const maxAgentExecutors = 16

// rememberAgent writes the sighting to the agents table, subject to the
// throttle, so last-seen and the agent's existence outlive this process.
//
// Every failure here is swallowed. This runs on the claim path, and nothing
// about remembering an agent is worth failing a build over: a full disk, a
// locked table or a migration that has not run must degrade the page, never
// stop CI. The throttle decision and the write are separate steps on purpose —
// the registry's lock is never held across a database call.
func (s *Server) rememberAgent(name string, executors []string, scheme string, self db.SelfReport) {
	if s.DB == nil || !s.Agents.ShouldPersist(name, db.AgentSightingInterval) {
		return
	}
	if err := s.DB.RecordAgentSighting(name, executors, scheme, self, time.Now()); err != nil {
		log.Printf("agent %s: could not record sighting: %v", name, err)
	}
}

// agentIsPaused reports whether an operator has stopped this agent taking work.
//
// Fails OPEN, and that is the whole design of it. An error reading the pause —
// a locked database, an unreadable value, a table that is not there yet —
// answers "not paused", so the worst case of a broken pause is an agent that
// keeps building. The opposite default would let one bad read stop the fleet
// silently, and nobody investigates a pause they did not set.
func (s *Server) agentIsPaused(name string) bool {
	if s.DB == nil {
		return false
	}
	until, err := s.DB.AgentPausedUntil(name)
	if err != nil {
		log.Printf("agent %s: could not read pause state, treating as not paused: %v", name, err)
		return false
	}
	return models.AgentPaused(until, time.Now())
}

// requestScheme reports how a request reached us.
//
// X-Forwarded-Proto because the server sits behind nginx and never sees TLS
// itself. The header is only trustworthy because that nginx overwrites it, so
// this is used to warn on the agents page and never to decide whether a
// request is allowed.
func requestScheme(r *http.Request) string {
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		return p
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

// handleAgentLog appends a chunk of build output at a byte offset.
//
// Offsets make the append idempotent, which is what lets an agent retry after
// a lost response without duplicating output: a chunk the server already has
// is recognised by its offset and dropped, and a partially-applied one is
// trimmed to just the bytes that are new. An offset past the end is the one
// unrecoverable case — the agent and server disagree about history — and gets
// a 409 carrying the true length so the agent can resync.
func (s *Server) handleAgentLog(w http.ResponseWriter, r *http.Request) {
	if !requireCsrf(w, r) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, 400, "invalid id")
		return
	}
	var req struct {
		agentIdent
		Offset int    `json:"offset"`
		Data   string `json:"data"`
	}
	if !decodeAgentBody(w, r, maxAgentLogBody, &req) {
		return
	}
	if req.Offset < 0 {
		writeError(w, 400, "offset must not be negative")
		return
	}

	s.agentLogMu.Lock()
	defer s.agentLogMu.Unlock()

	build, err := s.DB.GetBuildSummary(id)
	if err != nil {
		writeError(w, 404, "build not found")
		return
	}
	if !s.agentOwns(w, build, req.Agent) {
		return
	}

	total, err := s.DB.BuildLogLen(id)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if req.Offset > total {
		// Beyond the end: the agent believes it has sent bytes this server
		// never stored. Report the truth and let it resume from there.
		writeJSON(w, 409, map[string]interface{}{
			"error": "log offset ahead of server", "len": total,
		})
		return
	}

	// Bytes below the stored length are only skippable if they are genuinely
	// the same bytes — i.e. this really is a retry of a chunk that landed
	// before its response was lost. Assuming it would silently swallow real
	// output whenever the log already holds something the agent did not write
	// (a build requeued under the local runner before its project moved to a
	// remote executor arrives with a previous attempt's log). Verify, and on a
	// genuine disagreement make the agent resync rather than guessing.
	overlap := total - req.Offset
	if overlap > len(req.Data) {
		overlap = len(req.Data)
	}
	if overlap > 0 {
		stored, err := s.DB.BuildLogSlice(id, req.Offset, overlap)
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		if string(stored) != req.Data[:overlap] {
			writeJSON(w, 409, map[string]interface{}{
				"error": "log offset does not match what the server has stored", "len": total,
			})
			return
		}
	}

	tail := ""
	if end := req.Offset + len(req.Data); end > total {
		tail = req.Data[total-req.Offset:]
	}

	if tail != "" {
		s.seedBus(id, total)
		// Bus before DB, exactly as the runner's sink does it: a subscriber
		// that arrives between the two either sees the bytes in its snapshot
		// or reads them from the DB, never both.
		s.Bus.Publish(id, []byte(tail))
		if err := s.DB.AppendBuildLog(id, tail); err != nil {
			writeError(w, 500, err.Error())
			return
		}
		total += len(tail)
	}

	// The upload doubles as a heartbeat: a build that is talking is alive, and
	// this is also where a pending cancel reaches a busy agent soonest.
	_, cancel, err := s.DB.HeartbeatBuild(id, req.Agent)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]interface{}{"len": total, "cancel": cancel})
}

// seedBus brings a live build's topic buffer up to the stored log's length
// without publishing anything.
//
// It matters after a restart: the process that was streaming this build's log
// is gone along with its topic, but the build itself survived on the agent, so
// the next append arrives at an offset the empty new topic knows nothing
// about. Copying the stored bytes in restores the invariant that the buffer
// mirrors the DB. Silent by design — any subscriber attached in that window
// replayed the same bytes from the DB already, so publishing them would show
// the log twice.
func (s *Server) seedBus(id int64, storedLen int) {
	if storedLen == 0 {
		return
	}
	if _, cur, ok := s.Bus.LogTail(id, 0); ok && cur >= storedLen {
		return
	}
	build, err := s.DB.GetBuild(id)
	if err != nil {
		return
	}
	s.Bus.Seed(id, []byte(build.Log))
}

// handleAgentHeartbeat renews the lease on a claimed build and reports whether
// a cancel is pending. The agent sends it on a timer of its own, independent
// of build output, so a silent ten-minute IL2CPP link does not read as a dead
// machine.
func (s *Server) handleAgentHeartbeat(w http.ResponseWriter, r *http.Request) {
	if !requireCsrf(w, r) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, 400, "invalid id")
		return
	}
	var req agentIdent
	if !decodeAgentBody(w, r, maxAgentBody, &req) {
		return
	}
	ok, cancel, err := s.DB.HeartbeatBuild(id, req.Agent)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if !ok {
		// Not running, or owned by someone else. Either way this agent has
		// lost the build and must stop working on it.
		build, err := s.DB.GetBuildSummary(id)
		if err != nil {
			writeError(w, 404, "build not found")
			return
		}
		writeJSON(w, 409, map[string]interface{}{
			"error": "build is no longer yours", "status": build.Status,
		})
		return
	}
	writeJSON(w, 200, map[string]interface{}{"ok": true, "cancel": cancel})
}

// handleAgentFinish records an agent build's terminal status.
func (s *Server) handleAgentFinish(w http.ResponseWriter, r *http.Request) {
	if !requireCsrf(w, r) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, 400, "invalid id")
		return
	}
	var req struct {
		agentIdent
		Status models.BuildStatus `json:"status"`
	}
	if !decodeAgentBody(w, r, maxAgentBody, &req) {
		return
	}
	if !req.Status.Terminal() {
		writeError(w, 400, "status must be success, failed or canceled")
		return
	}

	// Held for the same reason the log handler holds it: the SSE contract says
	// every log byte is published before the terminal status, and an append
	// still in flight would land after it, where clients have already closed
	// their stream.
	s.agentLogMu.Lock()
	defer s.agentLogMu.Unlock()

	build, err := s.DB.GetBuildSummary(id)
	if err != nil {
		writeError(w, 404, "build not found")
		return
	}
	if build.Status.Terminal() {
		// Already decided — almost always the janitor having given up on an
		// agent that then came back. Report what is recorded rather than
		// rewriting history, and say so loudly enough to be found later.
		if build.Status != req.Status {
			log.Printf("Agent %s reported build %d as %s, but it was already %s",
				req.Agent, id, req.Status, build.Status)
		}
		writeJSON(w, 200, map[string]interface{}{
			"id": id, "status": build.Status, "already": true,
		})
		return
	}
	if !s.agentOwns(w, build, req.Agent) {
		return
	}

	if err := s.DB.FinishBuild(id, req.Status); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	finishedAt := time.Now().UTC()
	s.Bus.PublishStatus(id, req.Status, build.StartedAt, &finishedAt)

	if s.Notifier != nil && build.StartedAt != nil {
		s.Notifier.NotifyFinished(id, req.Status, *build.StartedAt, finishedAt)
	}
	log.Printf("Agent %s finished build %d: %s", req.Agent, id, req.Status)
	writeJSON(w, 200, map[string]interface{}{"id": id, "status": req.Status})
}

// agentOwns checks that the build is running and claimed by this agent,
// writing the error response itself when it is not. A mismatch is not an
// error condition to retry: the agent has lost the build (the janitor gave up
// on it, or an operator canceled it) and should stop.
func (s *Server) agentOwns(w http.ResponseWriter, build *models.Build, agent string) bool {
	if agent == "" {
		writeError(w, 400, "agent is required")
		return false
	}
	if build.Status != models.StatusRunning {
		writeJSON(w, 409, map[string]interface{}{
			"error": "build is not running", "status": build.Status,
		})
		return false
	}
	if build.Agent != agent {
		writeJSON(w, 409, map[string]interface{}{
			"error": "build is claimed by another agent", "agent": build.Agent,
		})
		return false
	}
	return true
}
