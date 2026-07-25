# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

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

**Ordering contract in SSE:** log bytes are always written *before* a terminal status
event — the client closes its `EventSource` on a terminal status and would drop anything
after it. `internal/api/api.go`'s `handleBuildEvents` is subtle around subscribe/re-read
races; read the comments before editing it.

**Interrupted builds keep `finished_at` NULL.** Stamping the restart time fabricates
durations and poisons the ETA estimates. Never "fix" this by filling it in.

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
- A failed probe **keeps** the last known tip, so a remote coming back up doesn't build
  an old commit.
- A commit landing mid-build is **deferred, not dropped** — the in-flight build has its
  own SHA pinned, so the baseline is held and the next sweep builds once the queue clears.
- Changing `repo_url` or `branch` **resets** the baseline (`ResetPollState`).
- `UpdateProject` deliberately does not write the `last_polled_*` columns; the poller
  owns them via `UpdatePollState`, so a settings save can't clobber them.

### Secret scrubbing

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

Not bugs, just unbuilt: the registry host is hardcoded in `runner.go` (should be
`BUILDS_REGISTRY`); images are tagged `:latest` only, so there is no rollback target;
list endpoints serialize full logs per row; and `AppendBuildLog`'s string concat in
SQLite is O(n²) for very large logs.
