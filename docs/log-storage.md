# Durable append-only build logs

## Layout and reads

- `builds.log` stays as an immutable legacy prefix. No eager history migration runs.
- `build_log_state` stores `legacy_bytes` and authoritative `total_bytes` per build.
- `build_log_chunks` stores BLOBs up to 64 KiB at absolute byte offsets. Its composite primary key supports a predecessor seek plus bounded range scan for byte slices.
- New builds create zero-prefix state. Old rows gain state lazily on their first append. No historical log copy is stored in chunks.
- Full `GetBuild` and legacy list APIs assemble prefix plus chunks. Length, metadata, polling tails and SSE replay do not assemble the whole log unless the caller asks for all bytes.
- Offsets count bytes, not runes. NUL and slices inside UTF-8 runes remain exact in storage and raw HTTP tails.

## Write and replay rules

Transactions append bounded chunks and advance the byte count together. Cancel, janitor and restart notes commit with their status changes. The compatibility `UpdateBuildStatus` full replacement clears chunks/state and the old prefix in the same transaction; production queue-full paths append a note instead.

Commit durable data before `PublishAt`. Offset-aware publication drops duplicate/overlapping retries. A missing bus prefix disconnects subscribers rather than inventing offsets. SSE subscribes first, replays durable bytes, and trims queued bytes already replayed. It backfills the final stored tail before terminal status. A storage-read error closes SSE without terminal so the browser can reconnect.

Agent appends verify ownership, running status and expected length in the append transaction and renew the heartbeat. Agent finish uses a conditional running/owner update. Janitor stale checks re-read the heartbeat in its transaction. A failed sink commit retains buffered bytes and cannot publish them; Close reports failure and the runner does not publish a false terminal state.

The live bus retains at most 256 KiB per topic and uses absolute byte offsets. `Length` is metadata-only. `SeedAt` loads only the bounded durable suffix after restart/requeue. `LogTail` returns `ok=false` when the requested offset predates retention, so callers must read storage. `Subscribe` returns no snapshot for an evicted offset; callers replay storage through the returned absolute total before consuming offset-deduplicated events. SSE already follows this rule.

Subscriber queues hold at most 64 log events of at most 64 KiB each. Oversized publishes split events only at UTF-8 rune boundaries; slow clients disconnect and replay durable bytes. Event strings are shared between subscribers. Full initial SSE replay can still allocate the requested historical tail; retention bounds live topics, not explicit full downloads. Batching can delay local live output up to 500 ms and reflects committed output only.

## Deployment and rollback

Back up the SQLite database before the first schema-changing deployment. Use the exact validated checkout and version verification. Keep the single-server/single-local-worker invariant. No production migration or deployment was performed during implementation.

**An old binary cannot read appended chunks. Do not roll back only the binary after new writes.** Restoring the pre-deploy database also discards post-backup work and is not a routine rollback. Prefer a forward fix. An operator-approved offline export/materialization tool would be required for a lossless downgrade; no destructive eager materialization is included here. Raw `builds.log` SQL consumers must switch to the supported APIs.

Both new tables cascade on build/project deletion. Migration is additive and idempotent. Covering fleet-health indexes and the existing recent-summary index remain separate from log persistence.

## Validation

Hermetic Go tests cover old-schema reopen, lazy prefix preservation, Unicode/NUL byte slices, multi-chunk boundaries, reopen/GetBuild/list compatibility, replacement reset, foreign-key cascade, append/cancel/reset rollback, lease conflicts, committed-before-publication retries, bounded 20 MiB retention/absolute replay, UTF-8 event splitting, slow clients, concurrent ring access, SSE replay overlap and final-tail ordering. Synthetic benchmarks use temporary databases only.
