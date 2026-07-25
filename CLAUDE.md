# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

This file is for working **on** the build server. For putting a *new project* onto it
(create → trigger → deploy → expose), see [docs/adding-a-project.md](docs/adding-a-project.md).

## What this is

A self-hosted CI/CD server (Go + SQLite + Docker, no external dependencies beyond the
`git` and `docker` CLIs). It watches repositories, builds Docker images, pushes them to
`registry.fandoster.com`, and optionally runs `docker compose up` to deploy. It is
deployed as a container on the Hermes Linode and builds itself.

The Docker image/container/registry names are deliberately `builds`, not `Build-System`
— Watchtower and the compose files on the server track that name.

## Commands

```bash
go build ./...
go test ./...
go vet ./... && gofmt -l .    # gofmt -l must print nothing
```

Run a single test or package:

```bash
go test ./internal/poller/ -run TestNewCommitQueuesBuild -v
```

Run the server locally (see "Local demo harness" below for a usable setup):

```bash
BUILDS_DB=/tmp/builds.db BUILDS_ADDR=:8899 go run ./cmd/builds
```

### Environment

| Variable | Default | Notes |
| --- | --- | --- |
| `BUILDS_ADDR` | `:8080` | |
| `BUILDS_DB` | `/var/lib/builds/builds.db` | |
| `BUILDS_BASE_PATH` | `""` | e.g. `/builds` when behind a path-stripping proxy |
| `BUILDS_BUILD_TIMEOUT` | `30m` | Go duration |
| `BUILDS_PASSWORD` / `BUILDS_PASSWORD_HASH` | unset | Unset **disables auth entirely** (the server logs a loud warning). Hash (bcrypt) wins when both are set. |
| `BUILDS_NOTIFY_EMAIL` | unset | Recipient of build-completion mail. Unset disables it. |
| `BUILDS_PUBLIC_URL` | `""` | e.g. `https://fandoster.com/builds`. Only used for the link in that mail — the server never sees its own external URL. |
| `BUILDS_SMTP_ADDR` | `172.17.0.1:25` | The host's Postfix over the Docker bridge. Override is for tests. |

## Architecture

One process, three long-lived goroutine groups sharing a `chan *models.Build`:

- **API/web** (`internal/api`, `internal/web`) — writes a `pending` build row and sends
  it on the channel.
- **Runner** (`internal/runner`) — a **single** worker draining that channel, plus a
  janitor. Single-worker is an invariant, not a coincidence: the janitor assumes any
  `running` row that isn't the current build is stale, so **two server processes against
  one DB will fail each other's builds**.
- **Poller** (`internal/poller`) — sweeps every 10s, `git ls-remote`s each enabled
  project, and pushes builds onto the same channel.

`internal/logbus` is the in-memory pub/sub hub connecting the runner's output to SSE
clients. The DB row is the durable copy; the topic buffer mirrors it byte-for-byte.

### Two live transports, on purpose

| | build page | list pages (dashboard, project) |
| --- | --- | --- |
| transport | SSE, `GET /api/builds/{id}/events` | WebSocket, `GET /api/live` |
| payload | one build's log bytes + status | whole snapshot of the recent builds |
| source | `internal/logbus` (per-build topics) | `internal/live` (one sampler) |
| fallback | `GET .../log?offset=N` polling | `GET /api/live` polling (same payload) |

They are not interchangeable. A log is an append-only byte stream where every byte
matters and a reconnect must resume at an offset — SSE's `Last-Event-ID` does that for
free. The dashboard needs the opposite: idempotent whole-state snapshots that a late or
reconnecting client can apply blind, broadcast to every open page at once.

`internal/live` **samples** rather than being notified: a single goroutine re-reads the
recent builds every second while at least one client is subscribed, and fans out only
when the serialized snapshot actually changed. Builds are created in four places (API,
webhook, poller, startup recovery) and their statuses move in three more (runner,
janitor, DB layer); a sampler observes all of them without a hook in each, and a missed
hook would be silent. N idle dashboards therefore cost one query per second and zero
bytes on the wire; zero dashboards cost nothing at all.

`internal/ws` is a hand-rolled RFC 6455 server — ~200 lines covering exactly the
one-way text-frame case, keeping `go.mod` at one direct dependency. It masks nothing
(server frames must not be masked), rejects unmasked client frames, answers pings, and
caps inbound payloads at 4 KiB. Do not grow it into a general WebSocket library; reach
for a real one instead.

**The WebSocket needs proxy cooperation.** nginx must forward the upgrade for
`/api/live`, which is *not* the default:

```nginx
proxy_http_version 1.1;
proxy_set_header Upgrade $http_upgrade;
proxy_set_header Connection $connection_upgrade;
proxy_set_header Host $host;          # or the Origin check rejects every browser
proxy_read_timeout 300s;              # idle sockets are pinged every 25s
```

Without it the handshake fails and the UI falls back to polling `/api/live` every 4s —
degraded, not broken. The connection indicator next to "Recent Builds" says which mode
is live (`● live` / `◌ polling` / `↻ reconnecting`), and `curl /api/live` returns the
exact JSON the socket pushes.

**`Host` matters.** `SameOrigin` (in `internal/ws`) compares the browser's `Origin`
against `Host`/`X-Forwarded-Host`, because WebSocket has no CORS: without it any page
on the internet could open a socket carrying the operator's cookie. A `proxy_pass` that
leaves `Host` as the upstream address makes every browser fail that check.

### Build-completion email

`internal/runner/notify.go` mails the outcome of every build that reaches a terminal
state through `finish()`. It depends on server-side setup that is **not in this repo**:
Postfix on the host with the Docker bridge ranges in `mynetworks`, a UFW rule opening
port 25 on the bridge, and an SPF record authorising the host's IP. Change any of those
and mail stops with no change to this code.

Two constraints are load-bearing, both learned the hard way:

- **Never `smtp.SendMail`.** It negotiates STARTTLS whenever the server advertises it,
  and this Postfix presents a self-signed certificate that Go rejects. `notify.go`
  dials plain TCP and drives `smtp.NewClient` itself. `TestNotifySendsCompletionMail`
  fails loudly if a STARTTLS attempt ever reappears.
- **Send from `builds@fandoster.com`.** The SPF record covers that domain; anything
  else gets filed as unsolicited.

Mail is fire-and-forget from `finish()` — a dead relay must never turn a green build
red, and must never delay the next build. Two paths deliberately send nothing: a
restart requeue (not terminal) and the janitor's stale sweep (the server wasn't running
when that build died).

This feature was lost once already: it was written directly on the server, never
committed, and the next deploy's `git reset --hard origin/main` erased it silently.
Nothing warns you about that — if you fix something on the box, commit it.

### Things that will bite you

**SQLite pragmas are driver-specific.** The driver is `modernc.org/sqlite`, which reads
`_pragma=name(value)` and *silently ignores* unknown query params. The mattn spelling
(`_journal_mode=WAL&_foreign_keys=on`) parses fine and does nothing — this repo shipped
with foreign keys off for months and `ON DELETE CASCADE` never fired. `TestPragmasApplied`
guards the DSN; do not touch it without running that test.

**Log grammar is a pinned contract** between `internal/runner/runner.go` and the client
parser in `internal/web/static/js/app.js`. The sentinels (`##[step:<id>]`, `[ERROR]`,
`BUILD SUCCESS`) are documented in a comment at the top of `runner.go`. Changing one
means changing both sides. (That comment also points at
`internal/runner/testdata/log_fixture.txt`, which does not exist — the fixture was never
added, or was removed without updating the comment.)

**Do not pass `--progress` to `docker build`.** The production Docker is legacy
(non-BuildKit) and exits 125 on it.

**Templates and static assets are `go:embed`ed.** Editing HTML/CSS/JS requires a server
restart, not just a refresh.

**List row markup exists twice.** The dashboard cards (`index.html`) and project rows
(`project.html`) are also built in JS (`buildCard`/`buildRow` in `app.js`) for rows the
live feed adds after page load. Change one, change the other — the templates carry a
comment saying so.

**Ordering contract in SSE:** log bytes are always written *before* a terminal status
event — the client closes its `EventSource` on a terminal status and would drop anything
after it. `internal/api/api.go`'s `handleBuildEvents` is subtle around subscribe/re-read
races; read the comments before editing it.

**Interrupted builds keep `finished_at` NULL.** Stamping the restart time fabricates
durations and poisons the ETA estimates. Never "fix" this by filling it in.

**A restart re-queues the in-flight build rather than failing it.** The build server
pushes its own image, which makes Watchtower recreate the container, which SIGTERMs a
server that may be mid-build — so losing work on restart is a routine event here, not an
edge case. Two paths cover it: the runner requeues on a `context.Canceled` cause
(graceful SIGTERM), and `recoverOrphanedBuilds` requeues rows left `running` by a hard
kill. Both are bounded by `models.MaxBuildRequeues`, without which a build that crashes
the server would be retried on every boot forever. The runner writes the `[restart]`
seam **through the log sink**, never straight to the row — see the byte-mirror note
below.

**The DB log and the logbus buffer must stay byte-identical** for a live build. Anything
appending to `builds.log` via SQL while subscribers exist has to mirror the same bytes
onto the bus (`handleCancelBuild` does this for the cancel tombstone). This is why
`db.RequeueBuild` does *not* write the log seam itself and `db.RequeueNote` is exported
for callers to write at the right layer.

**State-changing API endpoints require `X-Builds-Csrf: 1`.** Any curl script must send it.

### Schema migrations

`db.migrate()` is idempotent and additive only: `CREATE TABLE IF NOT EXISTS` plus
`addColumnIfMissing` calls guarded by `pragma_table_info`. Add new columns to *both* the
`CREATE TABLE` (fresh DBs) and the migration list (existing DBs). There is no migration
framework and no down-migrations.

Secrets (`webhook_secret`, `clone_token`) are stored in the projects table.
`ListProjects` sanitizes them out, so any caller needing them must re-fetch the row with
`GetProject` — the webhook handler and the poller both do this.

### Build triggers

Three paths, all converging on a `pending` row plus a channel send:

1. **Manual** — `POST /api/projects/{id}/build`, commit SHA recorded as `"manual"`.
2. **Webhook** — `POST /api/webhook/github`, matched by normalized repo URL + branch,
   HMAC-validated when the project has a webhook secret. Always open (unauthenticated) —
   GitHub cannot log in.
3. **Polling** — per-project opt-in (`poll_enabled`, `poll_interval_secs`, floored at 30s).

Polling invariants, each of which has a test in `internal/poller/poller_test.go`:

- Enabling only **seeds** the baseline; it never builds. Otherwise turning it on would
  rebuild whatever the branch already pointed at.
- A commit that **already has a build** (any status, any trigger) is never queued again.
  This is what keeps webhook + polling from double-building every push — they race, and
  the loser must adopt the baseline rather than build. It also stops a failed commit
  being retried every sweep. This check must stay *before* the in-flight check, which
  cannot tell whether the running build is for this commit or an older one.
- A failed probe **keeps** the last known tip, so a remote coming back up doesn't build
  an old commit.
- A commit landing mid-build is **deferred, not dropped** — the in-flight build has its
  own SHA pinned, so the baseline is held and the next sweep builds once the queue clears.
- Changing `repo_url` or `branch` **resets** the baseline (`ResetPollState`).
- `UpdateProject` deliberately does not write the `last_polled_*` columns; the poller
  owns them via `UpdatePollState`, so a settings save can't clobber them.

### Secret scrubbing

`injectToken` puts the token in **both** halves of the userinfo
(`https://<token>:<token>@host/...`) because that is the only form valid for every
token type: GitHub accepts a *classic* PAT as the username, but a *fine-grained* one
must arrive as the password, and a username-only URL leaves git nothing to send —
which `GIT_TERMINAL_PROMPT=0` turns into `could not read Password`. The classic tokens
in use today also work username-only, so this is robustness, not a live fix.
`TestInjectToken` pins the format.

Clone tokens are injected into the clone URL and must never reach a stored log. The
scrubbing lives in `internal/runner/sink.go`, which scans complete `\n`/`\r`-terminated
segments (a secret contains neither) with a holdback window so a force-drain can't split
a token into two unmatched halves. Both the raw and percent-encoded forms are masked.
`runner.ScrubSecret` is the exported entry point for other packages.

## Testing conventions

Tests are hermetic — no network, no real Docker. The patterns to reuse:

- `internal/runner/runner_integration_test.go` — writes a stub `docker` script onto
  `PATH` (honouring `DOCKER_STUB_SLEEP`/`DOCKER_STUB_EXIT`) and creates a real local git
  repo, then drives a real `Runner` against a temp DB.
- `internal/poller/poller_test.go` — a fixture that swaps `Poller.LsRemote` for a stub
  and calls `Sweep()` directly, with `forceDue()` to skip past intervals instead of
  sleeping. One test (`TestGitLsRemote`) exercises the real git probe against a temp repo.

Both skip cleanly when `git` is absent.

## Local demo harness

Static assets are embedded, and the runner shells out to `docker`, so a useful local
setup needs: a stub `docker` script on `PATH`, a local git repo to point a project at
(a plain filesystem path works for both `clone` and `ls-remote`), and a
`.claude/launch.json` (gitignored) running `go run ./cmd/builds` on a spare port. Leave
`BUILDS_PASSWORD` unset locally so the API is reachable without auth.

## Deployment

Direct-to-`main`; no PR flow. Features are typically committed, pushed, and deployed in
one pass.

Server: Hermes Linode `172.239.117.248`, key `~/.ssh/hermes-linode`.

```bash
cd /root/projects/builds && git fetch && git reset --hard origin/main
docker build --no-cache -t registry.fandoster.com/builds:latest .
docker push registry.fandoster.com/builds:latest
cd /opt/docker/builds && docker compose up -d --force-recreate builds
```

`--no-cache` is not optional: a plain build once served a stale image because Docker
reused the `COPY` layer despite changed templates.

Back up the DB before any schema change:

```bash
cp $(docker volume inspect builds_builds-data --format '{{.Mountpoint}}')/builds.db /root/builds.db.bak-$(date +%Y%m%d-%H%M%S)
```

**Verify deploys against what is actually served, not "deploy succeeded".** Bump
`api.Version` with anything that ships — `/api/health` returns it, which is the cheapest
proof the running container is the code you just pushed.

The container listens on `127.0.0.1:8082`; a proxy strips the `/builds` base path, so
when curling the container directly use unprefixed routes (`/projects/4`, not
`/builds/projects/4`). The session cookie's path won't match on direct curls — use
`Authorization: Bearer <password>` instead. The simplest way to make an authenticated
prod API call without handling the secret is to let the container expand it:

```bash
docker exec builds sh -c 'curl -s -H "Authorization: Bearer $BUILDS_PASSWORD" -H "X-Builds-Csrf: 1" http://127.0.0.1:8080/api/projects'
```

## Known gaps

The build server is Watchtower-managed (`com.centurylinklabs.watchtower.enable=true` in
`/opt/docker/builds/docker-compose.yml`), so a Build-System build that pushes `:latest`
causes a restart that interrupts whatever runs next. Requeueing makes that survivable
rather than lossy; it does not stop the interruption. Removing the label and having the
server deploy itself deliberately would, at the cost of a self-restart mechanism.


Not bugs, just unbuilt: the registry host is hardcoded in `runner.go` (should be
`BUILDS_REGISTRY`); images are tagged `:latest` only, so there is no rollback target;
`/api/builds` and `/api/projects/{id}/builds` still serialize full logs per row
(`ListRecentBuildSummaries` is the log-free query the live feed uses — the list
endpoints could take the same treatment); and `AppendBuildLog`'s string concat in
SQLite is O(n²) for very large logs.
