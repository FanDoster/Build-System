# Notification fleet health queries

`agents.Build` remains the full `/agents` UI path. It still reads the running
step from the log and shows recent build history and queue coverage.

`agents.BuildHealth` is the notification path. It shares the same state assembly
and failure rules. Its database interface has no log, full-summary, pending-queue
or history-metadata methods. It reads:

- Names from the registry, durable agent records and historical build owners.
- The newest running build ID, project ID/name, current queue and heartbeat.
- Statuses from exactly the newest ten owned builds with an existing project.
- Durable pause, poll, disk and doctor/self-report state.

## Semantics to preserve

- Ownership is the recorded agent name. The running build is selected by largest
  global build ID, not by project-local number or heartbeat time.
- Advertised profiles remain capabilities. A running build proves eligibility
  only for its exact project and queue. It must not cover sibling projects or
  bypass a nonempty profile allowlist.
- Liveness is an open poll **or** a recent poll **or** a recent owned-build
  heartbeat. Busy outranks paused. Durable rows never set `Known`.
- After restart, an unknown agent is `waiting` for one `AgentHeartbeatTTL`.
  Notification observation also waits for that grace period.
- Failure streaks inspect the same ten-row window as the UI. Pending/running
  rows consume window slots but neither count nor break the streak. Failed rows
  count. Success, cancel and other outcomes stop it.
- Timestamp freshness is evaluated in Go. SQLite and Go-driver timestamp text
  formats must never be compared or aggregated in SQL.
- Database read errors must not manufacture health transitions. Agent-record
  errors keep the UI's degraded/fail-open behavior; notifications reject the
  degraded observation.

## Covering indexes

Keep the existing `idx_builds_recent_summary`; this path does not replace it.
Add these indexes after additive build-column migrations:

```sql
CREATE INDEX IF NOT EXISTS idx_builds_agent_recent
  ON builds(agent, id DESC, status, project_id);
CREATE INDEX IF NOT EXISTS idx_builds_agent_running_health
  ON builds(agent, status, id DESC, project_id, last_heartbeat_at);
```

A log-free `SELECT` alone is not enough. It can still visit large `builds`
records. Covering indexes keep names, status windows and running ownership out
of the build-table payload/overflow pages. Project metadata still comes from
`projects`. The project join intentionally excludes legacy orphan build rows,
as the UI history already does.

Agent-name discovery scans the narrow index and sorts the small grouped name
list. Running/status lookups seek by agent and do not sort build rows. This is
not a claim that historical name discovery is constant-time.

## Local verification

All fixtures are synthetic. No server, production database, credentials or
network is needed.

```sh
go test ./internal/agents ./internal/db ./internal/notifications -run 'FleetHealth|NotificationAgentMatches|ObserveAgents' -v
go test ./internal/db ./internal/agents -run '^$' -bench FleetHealth -benchtime=300ms -benchmem
```

The query-plan test uses 256 large inline legacy log records and checks for
`USING COVERING INDEX` before and after `ANALYZE`. This ensures the health path
does not rely on log migration first removing those large records. The database
benchmark compares the narrow path with the UI's existing running/recent summary
queries for 16 agents. It does not read UI log tails, so it is a conservative
comparison of query cost, not an end-to-end notification sweep benchmark.
