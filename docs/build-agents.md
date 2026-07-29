# Build agents

How a build runs on a machine that is not this server.

The built-in runner builds Docker images here, on the Linode. That is the wrong shape
for some builds: a Unity game needs the editor, a GPU, and steamcmd on a real Mac. A
**build agent** is a process on such a machine that claims those builds over HTTP and
runs them itself, reporting back into the same dashboard, live log and history as
everything else.

Audience: whoever is writing or operating an agent. For the server's own internals read
[../CLAUDE.md](../CLAUDE.md); for putting an ordinary Docker project on the server read
[adding-a-project.md](adding-a-project.md).

---

## The model

A project's **executor** decides who runs its builds:

| `executor` | Who builds it |
| --- | --- |
| `local` (default, and empty means this) | The in-process Docker runner. Unchanged behaviour. |
| anything else, e.g. `mac` | An agent that asks for that name. |

Triggers do not change. A manual button, a webhook push and a poll all create the same
`pending` build row. The only difference is what happens next: a local build is pushed
onto the runner's channel, and **a remote build is left in the table**. That pending row
*is* the queue — an agent claims it when it next polls.

This is why there is no separate queue, no broker and no message bus. There is a table
with a `status` column, and claiming is an `UPDATE ... WHERE status='pending'`.

```
   trigger ──► builds row (pending) ──┬── executor=local  ──► channel ──► Docker runner
                                      └── executor=mac    ──► waits ──► agent claims it
```

## Direction of travel

**The server never connects to an agent.** Every exchange is a POST the agent starts.
The Mac these builds run on is behind NAT with no inbound access, and even where that is
not true, an inbound design turns every build machine into a service someone has to
expose and keep certificates on. Buildkite, GitHub Actions and GitLab all landed here;
TeamCity started with server→agent connections and removed them.

The practical consequence is that **a cancel cannot be pushed**. It is a flag the agent
reads off the response to something it was going to send anyway.

---

## Protocol

All endpoints take `Authorization: Bearer <token>` and `X-Builds-Csrf: 1`, and speak
JSON. Paths are relative to `https://fandoster.com/builds`.

### `POST /api/agents/claim`

Long-poll for work. Held open for up to **30s**, then `204`.

```json
{"agent": "mac-m4max-dan", "executors": ["mac"]}
```

`204` has meant "nothing waiting" since the beginning, and since A2 it also means **"you
are paused"** — an operator has stopped this agent taking new work from the `/agents`
page. This is deliberately indistinguishable to the agent, and needs no agent change:
a paused claim is still held for the full 30s, so a paused agent keeps its normal poll
cadence and keeps proving it is alive. Answering anything else would be worse. A `403`
is fatal to the agent's claim loop, which exits so a wrong token is visible rather than
silent; and returning `204` immediately would drop the agent onto its one-second poll
floor and turn a pause into a request flood.

The agent name is limited to `models.MaxAgentNameLen` (64) characters, with no control
characters, no surrounding whitespace, no invalid UTF-8, and no `/` or `\` — it is a
database key and a URL path segment on the operator endpoints. `executors` is capped at
16 entries.

The claim may also carry a small block describing the agent itself: `version`,
`os_arch`, `started_at`, `disk_free_gb`, `disk_floor_gb`. All optional. An agent that
sends none of them is not penalised — each field is stored only when present, so an older
agent cannot blank what a newer one reported.

### `POST /api/agents/status`

The agent's own report on its health, sent when it starts and then every few minutes.
Answers `204`.

```json
{"agent": "mac-m4max-dan",
 "checks": [{"name": "unity", "detail": "no licence; open Unity Hub and sign in",
             "ok": false, "needs_operator": true}],
 "unity": ["2022.3.62f2"],
 "tools": {"steamcmd": "/Users/x/Steam/steamcmd.sh"},
 "workspaces": [{"name": "ship-main", "used": "2026-07-27T09:14:00Z"}],
 "timeouts": {"build": "90m", "silence": "20m"}}
```

Authenticated as an **agent**, not an operator — this is the machine describing itself,
and it is the only endpoint under `/api/agents` that is. It is deliberately not folded
into the heartbeat: the heartbeat exists only while a build runs, so it would report
nothing while the agent is idle, which is when an operator is asking.

`detail` strings are rendered on the agents page **verbatim**, because each carries the
remedy for the thing it is reporting. The whole report is bounded server-side
(`models.AgentStatus.Clamp`) and trimmed rather than rejected when it is too long.

A server that predates this endpoint answers **405**, not 404 — the path matches another
route's pattern under a different method. An agent should treat both as "there is nothing
here" and stop asking until it restarts.

`200` returns the claimed build and the **full project row, clone token included** — the
agent has to authenticate the same private clone the local runner would. This is the one
response in the API that does not strip secrets; treat it accordingly.

```json
{"build": {"id": 412, "commit_sha": "1bfe15b9", "status": "running", ...},
 "project": {"repo_url": "https://github.com/nmr/ship", "branch": "main",
             "clone_token": "ghp_...", "executor": "mac", ...},
 "log_offset": 0}
```

**Start logging at `log_offset`, not at 0.** It is almost always 0, but a build
can arrive carrying an earlier attempt's output — it was requeued while its project
still built locally, and the project moved to a remote executor before anyone claimed
it. An agent that assumes 0 gets a `409` and has to resync.

The claim is a compare-and-swap in one statement, so two agents polling at the same
instant cannot take the same build. Asking for `local` is rejected with `400`: that
queue belongs to the runner.

### `POST /api/builds/{id}/log`

```json
{"agent": "mac-m4max-dan", "offset": 4096, "data": "[12:00:03] ##[step:unity] ...\n"}
```

`offset` is a **byte** offset into the build's log. Responses:

| Status | Meaning |
| --- | --- |
| `200 {"len": N, "cancel": false}` | Appended (or recognised as an exact replay). Resume from `len`. |
| `409 {"error": ..., "len": N}` | Offsets disagree. Resync: resend from `len`. |
| `409 {"error": "build is not running"...}` | The build is no longer yours. Stop. |
| `413` | Chunk too large (4 MiB). Split it. |

Offsets are what make a retry safe. If the agent resends a chunk it already sent — the
usual case being a response lost on the way back — the server recognises it and appends
nothing; a partially-overlapping chunk is trimmed to just the new bytes. Overlapping
bytes are **compared, not assumed**: if they differ from what is stored, you get a `409`
with the true length rather than a silent guess about whose version is right. Keep the
agent's own log file as the source of truth and always send from its own offset.

This call doubles as a heartbeat, and is the fastest path for a cancel to reach a busy
agent.

### `POST /api/builds/{id}/heartbeat`

```json
{"agent": "mac-m4max-dan"}
```

`200 {"ok": true, "cancel": false}`, or `409` when the build is no longer yours.

Send it every **20s** from a goroutine of its own — not from the build loop. A Unity
IL2CPP link can go ten minutes without printing a line, and a build that goes quiet is
not a build that has died.

### `POST /api/builds/{id}/finish`

```json
{"agent": "mac-m4max-dan", "status": "success"}
```

`status` must be `success`, `failed` or `canceled`. Flush the log first: clients close
their live stream on a terminal status and drop anything after it.

A `200 {"already": true}` means the server had already recorded an outcome (the janitor
gave up on the agent, or an operator canceled). The recorded status is returned and not
overwritten — report it and move on.

### Cancelling

An operator cancelling a running agent build gets `202 {"status": "canceling"}`, and the
build stays `running`. It is not canceled until the agent says so: only the agent knows
whether a `steamcmd` upload is halfway through. The agent sees `"cancel": true` on its
next heartbeat or log response, stops, and POSTs `finish` with `canceled`.

---

## Timings

| | Value | Why |
| --- | --- | --- |
| Claim hold | 30s | Comfortably under the 60s nginx default; a hold that outlives `proxy_read_timeout` returns 504 and looks like a flapping agent. |
| Heartbeat | 20s | Three fit inside the TTL. |
| Heartbeat TTL | 90s | After this the server presumes the agent is gone and fails the build. |
| Log flush | 2s or 128 KB | One request in flight. Batching also keeps the O(n²) SQL log append cheap. |
| Cancel grace | SIGTERM, 60s, SIGKILL | Unity needs time to release `Temp/UnityLockfile`; 10s is not enough. |
| Error backoff | 1s doubling to 30s | Capped low so a server redeploy does not idle the agent for an hour. |

Keep building through a server outage. Only the server decides a build is lost, and it
gives every agent a full TTL from its own start time to check back in — see below.

---

## What the janitor does, and why it does not kill your build

The server sweeps every 30s for `running` rows that nobody is working on. For local
builds the rule is simply "any running row that isn't the current one is stale", which is
sound with a single worker — and would fail every agent build within 30 seconds of it
being claimed.

So an agent-owned row is stale only when its heartbeat has been quiet for the TTL,
measured from `max(last_heartbeat_at, process start)`. The floor is what makes a redeploy
survivable: after a restart every heartbeat in the table is old through no fault of the
agents, which had nowhere to send them. Measuring from process start gives each of them a
fresh TTL to reappear.

A build whose agent really is gone is **failed, not requeued**. Re-running a Docker build
is free; re-running a build that may already have pushed a player to Steam is not, and the
server cannot know how far it got. The log says which happened:

```
[ERROR] Build interrupted by server restart          ← this server went down under it
[ERROR] Build agent stopped responding — no heartbeat received
```

---

## Log grammar

Agents emit the runner's own log grammar (pinned in `internal/runner/runner.go`). The
step rail, error highlighting and live log in the UI are a function of these bytes and
nothing else, so an agent that speaks it gets the whole UI for free — no server or
front-end change per agent type.

```
[HH:MM:SS] ##[step:<id>] <detail>     step boundary; <id> is [a-z]+, so new steps are free
                                       (checkout, unity, stage, steam, …)
[ERROR] <msg>                          preceded by a blank line
[HH:MM:SS] BUILD SUCCESS               on success
```

Timestamps are UTC `15:04:05`. Note that the server does **not** scrub secrets out of
agent-supplied log bytes the way the local runner scrubs its own subprocess output — an
agent is responsible for not sending its tokens.

---

## Credentials

Agents authenticate with `BUILDS_AGENT_TOKEN`, a credential separate from
`BUILDS_PASSWORD` so a build machine never holds the password that signs into the UI. It
can be rotated on its own, and requests bearing it are refused by the project-management
endpoints outright.

Generate one and put it in the server's compose environment:

```bash
openssl rand -hex 32
```

With `BUILDS_PASSWORD` unset the whole server is unauthenticated — that is pre-existing
behaviour, and it means the claim endpoint (which hands out clone tokens) is open too.
Do not run it that way anywhere reachable.

---

## Setting a project up

```bash
ssh -i ~/.ssh/hermes-linode root@172.239.117.248 'docker exec builds sh -c "curl -s -X PUT \
  -H \"Authorization: Bearer \$BUILDS_PASSWORD\" -H \"X-Builds-Csrf: 1\" \
  -H \"Content-Type: application/json\" -d \"{\\\"executor\\\":\\\"mac\\\"}\" \
  http://127.0.0.1:8080/api/projects/7"'
```

Or set **Executor** on the project's settings page. Executor names are lowercase letters,
digits, `-` and `_`, up to 32 characters. Changing it back to `local` (or clearing it)
returns the project to the Docker runner.

## Checking it by hand

The protocol is plain JSON, so `curl` is a working agent:

```bash
curl -s -X POST -H "Authorization: Bearer $TOKEN" -H "X-Builds-Csrf: 1" \
  -d '{"agent":"laptop","executors":["mac"]}' \
  https://fandoster.com/builds/api/agents/claim
```

```bash
curl -s -X POST -H "Authorization: Bearer $TOKEN" -H "X-Builds-Csrf: 1" \
  -d '{"agent":"laptop","offset":0,"data":"[12:00:00] ##[step:checkout] hello\n"}' \
  https://fandoster.com/builds/api/builds/412/log
```

The line should appear on the build page immediately, with `checkout` lit on the step
rail. That is the whole integration surface.
