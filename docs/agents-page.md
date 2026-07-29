# Plan: an agents page

A page on the build server showing the machines that build things: whether each
one is online, what it is running, when it was last seen, and enough about it to
answer "why is nothing building" without opening a terminal.

This is a design document that is now mostly built. **A1, A2 and A3 are done
and deployed** — the page at `/agents`, the coverage panel, per-agent state, the
persisted `agents` table behind pause/resume/forget, and the agent's own report
on what is wrong with the machine. A4 (polish) is still design only; §8 says
which is which. For the agent protocol, see [build-agents.md](build-agents.md).

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

**A2 changed this, deliberately and narrowly.** An idle agent now writes one row
per `db.AgentSightingInterval` — ten seconds, not per poll. The constraint above
is still what shapes everything, because the *reason* the write exists is that
nothing else records an idle agent at all; but "an idle agent writes nothing" is
no longer literally true, and A2's throttle is the price of a pause that
survives a redeploy and a last-seen that outlives the process. Read the rest of
this section as the problem statement, not as current behaviour.

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

## 4a. Transport security

Everything an agent sends and receives is sensitive. Every request carries the
agent token in an `Authorization` header, and **the claim response carries the
project's GitHub clone token** — the one endpoint in the API that deliberately
returns a secret unstripped. A page about agents has to account for how that
traffic is protected, and be able to say when it is not.

### Where it stands today (verified 2026-07-29)

| | State |
| --- | --- |
| Production transport | TLS 1.3, `TLS_AES_256_GCM_SHA384`, certificate verifies (`Verify return code: 0`) |
| Plain HTTP to the public host | `301` to `https://` |
| Certificate checking in the agent | Go's default. No `InsecureSkipVerify` anywhere in either repository |
| The server's own listener | `127.0.0.1:8082` — nginx terminates TLS, and the cleartext hop never leaves the machine |
| Transport visible to the server | Yes. nginx sets `X-Forwarded-Proto $scheme` on `location ^~ /builds/` |
| HSTS | Not set |

So production is encrypted. The gap was that **nothing enforced it**: the
agent's config accepted `http://` for any host, and a `POST` to a plaintext URL
puts the token on the wire before the redirect ever comes back. The 301 does not
protect the first request, and Go forwards the `Authorization` header across a
same-host redirect.

Closed in the agent (`internal/config`, `internal/client`): a non-loopback
`server_url` must be `https`, and a redirect that downgrades to `http` is
refused. Loopback stays exempt so a development server without a certificate
still works.

### What the page must do about it

1. **Record and show the transport per agent.** The server reads
   `X-Forwarded-Proto` (falling back to `r.TLS != nil` when nothing is in
   front). Store it on the sighting and render it. An agent whose requests are
   arriving in clear is a loud red warning, not a footnote — it means a token
   has already been exposed and needs rotating.
2. **Do not trust the header blindly.** `X-Forwarded-Proto` is client-supplied
   unless a proxy overwrites it, and nginx here does overwrite it. Treat it as
   evidence for a warning, never as an authorisation decision.
3. **Never render a token.** The agents page is the first UI that would be
   tempted to show "what the agent was given". It must not: the claim response
   is the only place a clone token legitimately appears, and it belongs nowhere
   near a web page. Build the page's data path on `ListProjects`, which
   sanitizes, not `GetProject`, which does not.
4. **Show the certificate expiry** for the public host, if it is cheap to get.
   An expired certificate breaks every agent at once, and the failure — a TLS
   error in a log on someone else's machine — is invisible from here.

### Related, not in scope for this page

- **HSTS is worth setting on the nginx site** so a browser that types `http://`
  never sends a session cookie in clear. It does nothing for the agent: Go's
  HTTP client does not implement HSTS, which is exactly why the agent enforces
  its own rule instead.
- **Mutual TLS or certificate pinning** would be the next step up and is not
  proposed. With one agent and a bearer token over a verified TLS 1.3 channel,
  the marginal gain is small and the operational cost — a private CA, renewals
  on both ends, an agent that stops building when a certificate rotates — is
  not.

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
   of the freshest signal, and the transport its last request arrived on. A
   plaintext agent is flagged here, loudly. Always show the timestamp, relative *and* absolute
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
    last_scheme       TEXT NOT NULL DEFAULT '',   -- https or http, from X-Forwarded-Proto
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

**A1 — the page, from data that already exists. Done, 2026-07-29.** In-memory
registry, coverage panel, per-agent state/executors/current build/recent
history, current step scraped from the log. No schema change, no agent change,
no protocol change.
*Accept:* stopping the agent flips it to offline within 90s; starting it flips
back within 30s; renaming a project's executor to something no agent serves
shows the coverage warning with the pending count.

*Verified* against a server on a temporary database, driven by a script that
speaks the real claim/log/heartbeat/finish protocol: offline at 91s against the
90s tolerance, back online at 2s, and a project pointed at `macos` while the
agent served `mac` showed the queue unserved with its pending count and
oldest-pending time while the agent itself stayed green — which is the whole
point of the panel. Consecutive failures counted correctly, the offline agent
stopped covering its queue, and the current step tracked the build through
checkout → unity → steam.

The first production deploy then found what a local harness could not. The
coverage panel called the `mac` queue **unserved while the Mac agent was
building from it** — the loudest warning on the page, firing at the one moment
it was wrong. The cause is the same fact the liveness rule is built around: an
agent does not poll while it builds, so after a redeploy a busy agent has no
registry sighting and advertises no executors, and coverage was reading only the
advertised list. A build in flight is now itself the proof — an agent running a
build from queue E serves E, which is stronger evidence than advertising it.
`p.executor` was already on the build summary, so this needed no new query.

Two more things surfaced in the local run and are worth recording. An orphaned
`running` row — an agent killed mid-build — makes an idle agent read as busy
until the janitor sweeps it, at most `AgentHeartbeatTTL`. This is left alone
deliberately: the janitor is the single authority on stale builds, and a second
staleness rule in the view is exactly the kind of duplicated state §6 avoids. It
self-heals, and it was seen to. Separately, the failure counter originally let a
build *in flight* reset the run, which blanked the warning at the moment
somebody was watching a retry; it now steps over non-terminal builds and stops
only at a success or a cancel.

**A2 — persistence. Done, 2026-07-29.** The `agents` table, last-seen across
restarts, forget, and pause/resume enforced in the claim handler. *Accept:*
last-seen survives a server redeploy; a paused agent keeps polling and reads as
connected-but-paused; its pause expires on its own.

*Verified* against a server on a temporary database, driven by a script speaking
the real protocol. A pause left the queued build pending while the agent stayed
`paused` with `polling: true`; the coverage panel said "paused — nothing will be
claimed" rather than "no agent serving this". Resume let the same build be
claimed. Killing and restarting the server preserved both the pause and
`first_seen`, and a remembered-but-absent agent decayed `online` → `waiting` →
`offline` over the tolerance, taking its queue coverage with it only at the end.
Pause, resume and forget were exercised through the page's own buttons.

Three decisions are worth keeping, because each has a wrong answer that looks
right:

- **Pause is checked on every tick of the claim loop, not once per poll.** An
  operator pausing because they are about to unplug the machine should not get
  one more build twenty seconds later. The loop already writes on every tick, so
  the extra indexed read is not a new order of cost.
- **A paused claim still holds the connection for the full poll before
  answering 204.** Returning immediately would turn a paused agent into a
  one-request-per-second loop — the agent's own floor is a second — and make its
  presence flicker on the page that is supposed to show it calmly connected.
- **A persisted row does not set `Known`.** That flag gates the post-restart
  grace, and a redeploy takes longer than the 90s tolerance; marking remembered
  agents as known would show the fleet offline after every deploy and, because
  offline agents do not cover queues, fire the coverage warning every time.

Two bugs were caught by running it rather than by tests. The first showed a
live, actively-polling agent as "last poll before restart": the stored timestamp
is written microseconds after the registry records the same poll, so taking
whichever was newer always picked the stored one. The persisted value is now
used only when there is no live sighting at all. The second was that
`ValidAgentName` accepted `/`; Go's own router handles a percent-encoded slash
correctly, but nginx normalises `%2F` in front of this server, so an agent named
`a/../claim` would have addressed a different endpoint than the operator
clicked.

A review pass over the finished code found six more, all fixed, none of which a
passing test suite would have shown:

| Defect | Why it mattered |
| --- | --- |
| `GET /agents` (the HTML page) had no operator check | Its JSON twin did. A build machine's own token could read the whole fleet — every machine, every queue, every pause note — by asking for the page instead of the API. |
| The pause cap was applied to the converted `Duration`, not the operator's input | `minutes * time.Minute` overflows int64 above ~1.5e8, and a wrap into the negative passes *both* "greater than zero" on the input and "no more than a week" on the product. The endpoint answered `200` for a pause that expired in 1822. It fails open, so nothing got stuck, but an endpoint reporting success for work it did not do is its own bug. |
| `PauseAgent` stamped `first_seen_at` when it created a row | Pausing a machine that has never connected recorded a contact that never happened, and nothing corrected it — the sighting path fills that column only while it is NULL. |
| The pause note was cut at a byte offset | Any note containing an accent had its last character split, and the invalid bytes survived into SQLite and back onto the page. |
| The five-second poll wiped the action-error banner | A refused pause or forget read as a silent no-op, which is the worst outcome for a control whose whole job is to be certain. The error now survives until the operator's next action. |
| `.` and `..` were valid agent names | A browser resolves them out of a URL path before sending, so such an agent could never be paused or forgotten from the page. |

Three further candidates were raised and refuted on inspection: that the seven
A3 columns needed `addColumnIfMissing` entries (the table is new, so no database
can exist with a narrower one), and two readings of `Forget` on a live agent
that rested on a state the claim handler cannot be in.

**A3 — the agent reports on itself. Done, 2026-07-29.** Claim-poll block plus
`/api/agents/status`; doctor on a timer; the page shows problems, disk against
floor, version, uptime and workspaces. *Accept:* pulling the Unity licence shows
the needs-operator check on the page with the agent's own remedy text.

*Verified* with the real agent binary against a real server. Pointing the agent
at a Unity Hub directory with no editors in it and a steamcmd that is not there
put three `needs operator` rows on the page within seconds, each carrying
Doctor's own sentence — "install the version the project's ProjectVersion.txt
asks for with Unity Hub" — because the `Detail` strings are shipped word for
word. A healthy run reported nine installed editors, 184 GB free against a 40 GB
floor, `darwin/arm64`, and a version of `2026-07-29-self-report+157ec682-dirty`.

The split matters more than it looks. The claim block is four scalars because it
rides a request that fires twice a minute forever; the fuller report has its own
endpoint on the agent's own timer. It is deliberately **not** on the heartbeat,
which exists only while a build runs and would therefore deliver nothing while
the agent is idle — exactly when somebody is asking what is wrong with it. Free
disk is measured on every claim rather than cached, because a Unity build moves
it by tens of gigabytes while it runs.

Two properties are what make this safe to ship in either order:

- **A server without the endpoint answers 405, not 404** — the path matches
  another route's pattern under a different method. Neither is in the agent
  client's fatal set, so a reporter that simply retried would ask a server that
  can never answer, every five minutes, forever. It asks once, says so once, and
  stops until the agent restarts. Verified against a build of the previously
  deployed server: one log line, no repeats, claiming unaffected.
- **An older agent sends none of this, and must not blank what a newer one
  stored.** Every column in the block is written `COALESCE(NULLIF(?, ...), col)`,
  on both of `RecordAgentSighting`'s write paths — the `UPDATE` and the
  fall-through `INSERT`. Miss the second and a brand-new agent's first claim
  stores nothing, with no visible reason.

Three defects were found after the code was written, all fixed:

- **The `<details>` panel snapped shut every five seconds.** The client renderer
  rebuilds each row from scratch, and a re-render destroys the element holding
  that state. Which machines have the panel open is now remembered by name on
  the container, not on the element.
- **A full disk was discarded.** `NULLIF(?, 0)` treated a reported zero as "not
  reported" — and 0 GB free is exactly what a disk that has filled reports. The
  page would have kept showing the last healthy figure at the moment it stopped
  being true. The reported fields are now nullable end to end, so "did not
  measure" and "measured zero" are different things.
- **Uptime kept counting for a machine that was switched off.** The stored start
  time does not decay, so a Mac shut down on Friday read "up 3d15h" on Monday
  beside an `offline` badge. Every other stale fact on the row was true when it
  was observed; that one was never true at any instant. Uptime is now withheld
  from an agent that cannot be reached.

Nine further candidates were raised by review and refuted on inspection —
several of them variations on the two write paths in `RecordAgentSighting`,
whose `ON CONFLICT` branch is only reachable in a race that cannot lose data.

**A4 — polish.** Transport warning and certificate expiry; clock-skew readout (the log grammar is agent-authored and UTC,
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

**`X-Forwarded-Proto` is only trustworthy because nginx overwrites it.** Behind
a different proxy, or with the container reachable directly, it is whatever the
client sent. Use it to raise a warning; never to decide whether a request is
allowed.

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
