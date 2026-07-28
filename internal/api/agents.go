package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

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
	}
	if !decodeAgentBody(w, r, maxAgentBody, &req) {
		return
	}
	if req.Agent == "" {
		writeError(w, 400, "agent is required")
		return
	}
	if len(req.Executors) == 0 {
		writeError(w, 400, "executors is required")
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

	deadline := time.Now().Add(s.pollHold())
	ticker := time.NewTicker(s.pollInterval())
	defer ticker.Stop()

	for {
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
