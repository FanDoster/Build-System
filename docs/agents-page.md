# Plan: an agents page

A page on the build server showing the machines that build things: whether each
one is online, what it is running, when it was last seen, and enough about it to
answer "why is nothing building" without opening a terminal.

This is a design document, not a description of something that exists. Nothing
in here is built yet. For the agent protocol that *does* exist, see
[build-agents.md](build-agents.md).

---

## 1. The questions the page must answer

In the order an operator actually asks them:

1. **Is the Mac going to build things?** Online, reachable, not paused, not out
   of disk.
2. **Why is nothing building?** Almost always one of: the agent is off, or a
   project's executor name matches no agent.
3. **What is it doing right now?** Which build, which step, how long.
4. **What has it been doing?** Recent builds, and whether failures cluster on
   this machine rather than in the code.
5. **What is wrong with it?** The things only a person at the machine can fix —
   an expired Unity licence, a Steam login wanting a code, a full disk.

Question 2 is the one with no answer today and the highest value. A project set
to executor `macos` when the agent serves `mac` produces a green agent, a build
that sits pending forever, and an error message in no log anywhere.

---

## 2. The one hard constraint

**An idle agent writes nothing to the database.** `db.ClaimBuildForAgent`
returns `(nil, nil)` on `sql.ErrNoRows` without performing an `UPDATE`, and
`handleAgentClaim` logs only on a successful claim. An agent that has been
long-polling healthily for an hour is byte-for-byte indistinguishable from one
that was unplugged an hour ago.

The corollary catches people: `max(builds.last_heartbeat_at)` is **not** "last
seen". `HeartbeatBuild` updates it only `WHERE status='running' AND agent=?`,
so it freezes the moment a build ends and is never cleared. Labelling that value
"last seen" on a page would be actively misleading — it means "the last moment
this agent was mid-build".

So liveness needs new state. That is the whole reason this feature is not a
template and an afternoon.

A second, subtler constraint: **an agent stops polling while it builds.** The
claim loop runs one build to completion before claiming again. Any liveness rule
based only on claim polls therefore reports every *busy* agent as offline —
which is precisely backwards.

---

## 3. What is already available

Worth knowing, because most of the page needs no new data at all:

| Available today | From |
| --- | --- |
| Every agent name ever seen | `SELECT DISTINCT agent FROM builds WHERE agent != ''` |
| The build an agent is running now | `builds WHERE agent=? AND status='running'` |
| Its build history, durations, success rate | `builds WHERE agent=?` |
| Which queues it has served | join `projects.executor` |
| Every configured remote executor | `SELECT DISTINCT executor FROM projects WHERE executor NOT IN ('','local')` — known even if no agent ever connected |
| Queue depth per executor | pending builds joined to projects |
| The step a running build is on | scan its stored log backwards for the last `##[step:<id>]` |
| Heartbeat age of a running build | `builds.last_heartbeat_at` |

That last one is worth calling out: **the current step needs no protocol
change**. The agent already emits the pinned log grammar, the server already
stores every byte, and it works retroactively on builds in flight right now.

`/api/live` also already carries `agent`, `last_heartbeat_at` and `executor` per
build, so a first cut of an agent strip could be built client-side with no
server change at all.

---

## 4. Design decisions

| Decision | Choice | Why |
| --- | --- | --- |
| Liveness source | In-memory registry in `api.Server`, written on every claim poll | An idle agent's only signal. A held long-poll is the strongest liveness proof there is, and it costs one map write per 30s per agent — no schema, no migration, no agent deploy. |
| Surviving a restart | Registry is deliberately volatile, with a process-start floor | Same reasoning as `models.AgentStale`: after a restart every agent looks stale through no fault of its own. Show "waiting for first poll since restart (12s)" rather than a red badge that is technically defensible and practically a lie. |
| Persistent agent identity | An `agents` table, added in A2 | The registry cannot answer "when did we last see it" across a restart, or "what did it look like yesterday". |
| Status model | Three orthogonal fields, never one enum | *Reachable*, *admitted* (paused), *busy* are independent. GitLab shipped these conflated into one `status` and had to break their API in 16.0 to separate them. Render one headline badge for glanceability, show all three underneath. |
| "Busy" | Derived from `builds`, never stored on the agent row | Two independently-written state fields with different TTLs will disagree, and the page will claim the agent is building #412 while the build page says #412 failed. One source of truth. |
| Online threshold | Reuse `models.AgentHeartbeatTTL` (90s) | It is already "three beats wide" and already documented. A second constant would drift from the first. |
| Live updates | A separate endpoint, not the existing `/api/live` snapshot | See §7 — this one is a trap. |
| Auth | `requireOperator` on every agents endpoint | An agent's own token authenticates against anything that does not check, and this page discloses the whole fleet rather than one project. |

---

## 5. The page

`GET /agents`, linked from the header beside Projects.

### 5.1 Coverage panel — first, and the most valuable thing here

Before any list of machines, one panel answering "why is nothing building". For
each distinct remote `projects.executor`:

```
mac        ● served by mac-m4max-dan          2 builds pending
windows    ✖ NO AGENT SERVING THIS            5 builds pending, oldest 3h ago
           projects: windows-fpl
```

An executor with pending builds and no live agent is the loudest thing on the
page. This is TeamCity's "Compatible Configurations" idea, and every input
already exists server-side today.

### 5.2 Per agent

In priority order — everything below (5) goes behind a disclosure triangle:

1. **State** — one badge (`online` / `busy` / `paused` / `offline`) plus the age
   of the freshest signal. Always show the timestamp, relative *and* absolute
   ("34 seconds ago · 14:22:07 UTC"). A badge the operator cannot verify is a
   badge they will not trust during an incident.
2. **Executors served**, and the projects routed to them.
3. **Current build** — project, build number, current step, elapsed, heartbeat
   age, linked to the build page.
4. **Problems** — the agent's failing `doctor` checks, needs-operator ones
   first, empty when healthy. Ship the agent's `Detail` strings verbatim; they
   were written for someone who has to fix the machine and each carries its
   remedy ("brew install git-lfs, and check the LaunchAgent's PATH includes
   /opt/homebrew/bin"). Re-wording them server-side loses the fix.
5. **Disk** — free against the agent's own floor, side by side, coloured on the
   ratio. The floor is the actual refusal threshold, so "38 GB free, floor 40"
   means the next build is already refused — which no absolute number conveys.
6. Last 10 builds with outcome and duration; consecutive-failure count (three
   reds on one machine is the fastest available signal for "the box is broken"
   rather than "the code is broken"); uptime and version; workspaces with
   last-used times, which is exactly the order the agent's sweep will delete
   them in.

### 5.3 Actions

Four, and no more: **pause/resume** (mandatory expiry, optional note), **cancel
the running build** (already implemented), **forget an agent row**, and **copy
the enrolment command**.

Pause needs care. Enforce it server-side by returning `204` from the claim
handler while paused — never client-side. A paused agent must **keep polling**,
and the UI must show it as connected-but-paused: if pause made the agent stop
polling, its last-seen would go stale and the page would flip it to offline,
destroying the distinction pause exists to express. The expiry is not a nicety
at one operator: the person who pauses to update Unity is the same person who
will forget, and an unexpiring pause is a dead CI that looks healthy.

---

## 6. Data model

### A1 — no schema change

```go
// in api.Server
type agentSighting struct {
    Name      string
    Executors []string
    FirstSeen time.Time
    LastPoll  time.Time
    Polling   bool   // a claim request is open right now
}
```

Written on entry to `handleAgentClaim`, `Polling` cleared with `defer`.

**The online predicate is a disjunction, and all three terms are required:**

```
polling now
  OR last claim poll within AgentHeartbeatTTL
  OR owns a running build whose last_heartbeat_at is within AgentHeartbeatTTL
```

Drop the third and every busy agent reads as offline, because a building agent
does not poll.

### A2 — `agents` table

Additive migration in the existing style, added to both the `CREATE TABLE` and
the `addColumnIfMissing` list:

```sql
CREATE TABLE IF NOT EXISTS agents (
    name              TEXT PRIMARY KEY,
    executors         TEXT NOT NULL DEFAULT '',   -- JSON array, as last advertised
    first_seen_at     DATETIME,
    last_seen_at      DATETIME,
    version           TEXT NOT NULL DEFAULT '',
    started_at        DATETIME,                   -- the agent's own start; uptime
    os_arch           TEXT NOT NULL DEFAULT '',
    disk_free_gb      INTEGER NOT NULL DEFAULT 0,
    disk_floor_gb     INTEGER NOT NULL DEFAULT 0,
    status_json       TEXT NOT NULL DEFAULT '',   -- last doctor result
    last_status_at    DATETIME,
    paused_until      DATETIME,
    pause_note        TEXT NOT NULL DEFAULT ''
);
```

Upserted on every claim poll — **including an empty one**, since that is the
only signal an idle agent produces. The upsert must fail open: an unknown name
creates a row and never rejects a claim. A migration or a wiped table must not
be able to stop CI.

Throttle the `last_seen_at` write to about once per 10s per agent so an agent in
a retry loop cannot hammer one hot SQLite row. Throttle in seconds — GitLab
throttled theirs to an hour and made their own UI up to an hour wrong.

### A3 — what the agent reports

Two additions, deliberately split:

- **On the claim poll**, a small always-cheap block: `version`, `started_at`,
  `disk_free_gb`, `disk_floor_gb`. Under a few hundred bytes; this fires twice a
  minute forever. The server ignores unknown fields today, so the agent can ship
  first and the page lights up when the server catches up.
- **`POST /api/agents/status`**, called at startup and on a 5–10 minute timer,
  carrying the full `doctor` result, installed Unity versions, tool paths,
  workspace list and effective timeouts. Keeping it off the hot path matters,
  and it must not go on the heartbeat: the heartbeat is per-build, so it would
  deliver nothing while idle — exactly when it is needed.

This also means the agent must run `Doctor` on a timer in the run path. Today it
is only reachable from the subcommand, so a cached-at-startup result would show
a week-old disk figure.

---

## 7. Live updates — the trap

`internal/live` fans out only when the serialized snapshot actually changed
(`bytes.Equal(snap, h.last)`). That is what makes N idle dashboards cost one
query per second and **zero bytes on the wire** — a documented, load-bearing
property.

An always-moving `last_seen_at` for idle agents in that same shared snapshot
would convert it into a permanent full-snapshot broadcast to every open
dashboard and project page, not just the agents page.

(Note in passing: a single `HeartbeatBuild` already changes the snapshot, so
agent heartbeats *already* defeat the dedupe for the duration of a build. Worth
fixing separately; do not make it worse.)

**Decision:** the agents page gets its own endpoint and its own hub, or simply
polls every few seconds. At one agent and one operator, a 3-second poll of a
small JSON document is the honest answer, and it keeps the invariant intact.

---

## 8. Milestones

**A1 — the page, from data that already exists.** In-memory registry, coverage
panel, per-agent state/executors/current build/recent history, current step
scraped from the log. No schema change, no agent change, no protocol change.
*Accept:* stopping the agent flips it to offline within 90s; starting it flips
back within 30s; renaming a project's executor to something no agent serves
shows the coverage warning with the pending count.

**A2 — persistence.** The `agents` table, last-seen across restarts, forget, and
pause/resume enforced in the claim handler. *Accept:* last-seen survives a
server redeploy; a paused agent keeps polling and reads as connected-but-paused;
its pause expires on its own.

**A3 — the agent reports on itself.** Claim-poll block plus
`/api/agents/status`; doctor on a timer; the page shows problems, disk against
floor, version, uptime and workspaces. *Accept:* pulling the Unity licence shows
the needs-operator check on the page with the agent's own remedy text.

**A4 — polish.** Clock-skew readout (the log grammar is agent-authored and UTC,
so a skewed Mac clock makes every step duration silently wrong); duplicate-name
and changed-hostname warnings; consecutive-failure highlighting.

---

## 9. Things that will bite

**`MAX(last_heartbeat_at)` will not scan into a `*time.Time`.** An aggregate
loses the column's declared type and the driver hands back a string. Selecting
the bare column works. Any per-agent rollup must scan a string and parse it.

**Never compare timestamps in SQL here.** `builds` already holds two formats
side by side — `created_at` from SQLite's `datetime('now')` and
`last_heartbeat_at` from the Go driver. `WHERE last_heartbeat_at > datetime('now','-90 seconds')`
returns the wrong rows silently. Decide staleness in Go, as `FailStaleRunning`
already does.

**Compute staleness against the process-start floor**, exactly as
`models.AgentStale` does, or every redeploy shows a fleet of dead agents.
Nothing outside the runner can currently reach `runner.startedAt` — it is
unexported with only a setter, and `api.RunnerControl` exposes only `Cancel` and
`Progress`. That needs a small widening.

**Agent names are unauthenticated free text.** Unlike executors, which are
validated to 32 characters of `[a-z0-9_-]`, `req.Agent` is only checked
non-empty, and anyone holding `BUILDS_AGENT_TOKEN` can assert any name —
including one that impersonates another agent, since ownership is name equality.
Render names through `html/template` and `textContent`, never `innerHTML`. Warn
when a name's reported hostname changes, and when two names appear at once: a
rename silently splits history, a duplicate silently merges two machines.

**An `agents` table keyed on a self-asserted name accumulates garbage.** A
typo'd name creates a row nothing ever removes. Ship forget in the same change
as the table.

**Templates and static assets are `go:embed`ed**, so the page cannot be
iterated on with a browser refresh — every change needs a rebuild and restart.

**List-row markup exists twice** the moment any agent row updates live: the
server-rendered template and the JS that builds rows added after page load. The
existing templates carry a comment saying so; follow it.

**Do not build the data path on `GetProject`.** It returns the clone token
unstripped — that is deliberate, for the claim response. `ListProjects`
sanitizes. A page that leaks tokens into HTML would be a bad way to find out.

---

## 10. What this deliberately will not have

- **No auto-reaping of unseen agents.** GitHub de-registers after 14 days; at
  one agent that is machinery to explain rather than value. Manual forget, and
  at most hide (never delete) a row unseen for 30 days.
- **No utilisation charts or queue-time graphs.** Two fields —
  "last seen" and "last build finished" — answer "is it alive" and "is it being
  used" without any of it.
- **No remote control of the machine.** The server never connects to an agent;
  that is the invariant the whole design rests on. Everything here is either
  something the agent reported or something the server already knew.
- **No workspace sizes.** Nothing computes one today, and doing so means walking
  a Unity `Library` of hundreds of thousands of files on the build disk. The LRU
  order is the useful part, and it is free.
