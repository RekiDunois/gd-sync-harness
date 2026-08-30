# knowledge-sync implementation plan: worker live status, real-time throughput, and single data-plane ownership

## 0. Status

This is an **incremental implementation plan** for the existing `knowledge-sync` codebase.

It replaces the narrow idea of fixing the displayed `Speed` field by writing a different value into SQLite. The implementation inspection showed that a correct live status design requires a broader architectural change:

1. rclone's current top-level `stats.speed` is an average over the stats group/run, while the CLI presents it as if it were a current transfer rate;
2. rclone telemetry currently reaches the CLI only after being written into SQLite;
3. `profile status --watch` and `profile wait` use SQLite polling as cross-process communication;
4. `reconcile-now` / `reconcile-scheduled` can still execute reconciliation in the invoking CLI process;
5. the file watcher can still execute fast rclone upserts in the watcher process;
6. therefore the global worker is not currently the unique source of live transfer telemetry.

The target architecture is:

```text
filesystem / CLI intent
        |
        v
SQLite durable intent + state
        |
        v
      WORKER  <---------------- rclone progress frames
        |
        +-- durable state cache
        +-- live activity / throughput state
        +-- status snapshot aggregator
        |
        v
Unix domain socket
        |
        +--> profile status
        +--> profile status --watch
        +--> profile wait
        +--> profile add --wait

socket unavailable / incompatible
        |
        v
SQLite coarse fallback
```

The worker becomes the **single rclone data-plane owner**. SQLite remains the durable source of truth for reconciliation intent and lifecycle state, but it is no longer the normal live telemetry transport.

---

## 0.1 Locked design decisions

The following decisions are settled by the design review and should not be reopened unless implementation reveals a concrete incompatibility.

### Throughput semantics

- `Speed` means **recent whole-activity throughput**, not rclone's cumulative average.
- Compute it from the latest two cumulative-byte samples:

```text
speed = (bytes_now - bytes_previous) / actual_elapsed_time
```

- Use the actual elapsed duration between samples; do not assume the nominal stats interval.
- The first sample has `speed_known=false`.
- A measured `0 B/s` is distinct from an unknown speed.
- No additional moving average or EWMA is required for the first implementation.
- Live rclone progress should be emitted at approximately **1 second** cadence.
- `Speed` is rendered only while an activity is in a transfer/uploading phase. It must not remain visible in `finalizing` or other non-transfer phases.

### Live status transport

- Add one global Unix domain socket served by the worker.
- Default socket path:

```text
filepath.Join(os.TempDir(), "knowledge-sync", "worker.sock")
```

- Create the parent runtime directory with user-only permissions (`0700`).
- Add a persisted `socket-path` setting. An explicit configured value overrides the default.
- Expose it through formal CLI commands:

```text
knowledge-sync config get socket-path
knowledge-sync config set socket-path <path>
knowledge-sync config unset socket-path
```

- The protocol is versioned from V1.
- Use a simple streaming NDJSON protocol over `net.UnixConn` / `net.Listen("unix", ...)` or an equivalent standard-library implementation.
- One worker socket supports multiple simultaneous subscribers.
- Slow subscribers must never block reconciliation. Telemetry is latest-value state, not an event log; intermediate snapshots may be dropped.

### Status authority

- The **worker is the normal status provider**.
- SQLite remains the durable source of truth used by the worker and by fallback readers.
- `profile status`, `profile status --watch`, `profile wait`, and `profile add --wait` all use the same worker status client in the normal path.
- A new subscription performs one durable refresh for that profile before returning the initial snapshot.
- After the initial refresh, 1 Hz telemetry snapshots are generated from worker memory without 1 Hz SQL reads.
- If the socket is unavailable, incompatible, or disconnects, clients fall back to SQLite and keep retrying the socket approximately every existing observer poll interval.
- SQLite fallback does **not** display `Speed` and does not claim a live stall diagnosis.

### Durable mutation invalidation

- External CLI mutations continue to commit SQLite first.
- After a successful durable mutation, the CLI sends a best-effort worker invalidation/wake message for the affected profile.
- Failure to notify the worker does not invalidate the durable command.
- Existing periodic worker rescans remain the correctness fallback.

### SQLite telemetry persistence

- Remove the current per-progress-frame SQL write behavior.
- Keep live telemetry in worker memory.
- Opportunistically attach the latest telemetry snapshot to DB writes that are already required.
- Force a coarse telemetry flush at lifecycle boundaries such as phase transitions and run success/failure.
- If no other DB write occurs during a very long activity, emit at most one coarse telemetry checkpoint per **1 hour**.
- The existing `speed_bytes_per_second` column may remain for schema compatibility but is not a live-speed authority.
- Do not add a DB `speed_known` column for live status.

### Stall semantics

- Live stall detection belongs to the worker's live telemetry state.
- The existing `30m` no-progress concept may be preserved for the live channel using monotonic/measurable progress timestamps.
- When only SQLite fallback is available, report that live telemetry is unavailable and show the durable checkpoint age; do not infer `possible stall` from a deliberately sparse DB checkpoint.

### Single execution ownership

- All rclone data-plane operations used for synchronization must run in the worker.
- `reconcile-now`, `reconcile-scheduled`, `sync`, migration-triggered reconciliation, and equivalent full reconciliation entrypoints must persist intent and wake the worker instead of directly calling the transfer path.
- Watcher-triggered fast upserts must also move into the worker.
- The watcher becomes an event/intent producer only.

### Reconciliation intent authority

- Preserve `profile_sync_state.desired_generation` as the sole durable reconciliation-intent authority.
- Do **not** add a second reconciliation request queue whose rows independently mean "work must run".
- Manual and scheduled requests advance/coalesce the durable generation and may attach pending execution metadata to that intent.
- Non-manual intent may advance the target generation but must not overwrite an unconsumed manual one-shot option.
- A later manual request replaces earlier unconsumed manual options.

### `--allow-deletes` authorization

- `reconcile-now --allow-deletes N` is a one-attempt manual safety authorization.
- The worker atomically consumes it when claiming the attempt.
- A failed attempt does not automatically grant the same override to a retry.
- Persist the effective delete limit and whether it came from a manual override into `sync_runs` for auditability.

### Wait semantics

- Manual `reconcile-now` keeps its current synchronous user experience by default, but execution occurs in the worker.
- A waiter is bound to the **submitted generation**, not to a specific `run_id`.
- Success condition:

```text
last_success_generation >= submitted_generation
```

- This allows several coalesced intents to be satisfied by one run with a later target generation.
- `reconcile-scheduled` persists intent, wakes the worker, and exits without waiting for transfer completion.

### Worker availability

- The global worker is required infrastructure for enabled profiles.
- Profile add/enable/install flows must ensure the worker launchd job is installed/running.
- If a durable request has been committed but the worker cannot be started, report the request as persisted and the worker as unavailable; do not fall back to executing rclone inside the CLI.

---

## 1. Goals

This implementation must achieve all of the following:

1. show an actually recent transfer rate in live status;
2. avoid high-frequency SQLite writes for telemetry;
3. avoid using SQLite polling as the normal cross-process live status channel;
4. make worker live status complete enough that all observer commands share one implementation;
5. make the worker the unique synchronization data-plane owner;
6. preserve durable reconciliation semantics across process crashes and worker restarts;
7. preserve existing generation-based coalescing and stale-work protection;
8. retain safe fallback behavior when the socket is unavailable;
9. preserve destructive-operation safety and make one-shot delete overrides auditable;
10. keep the implementation macOS-oriented and based on Go standard Unix-socket facilities.

---

## 1.1 Non-goals

Do not expand this change into unrelated daemon/RPC infrastructure.

Specifically:

- do not introduce gRPC, HTTP, protobuf, or a general service framework;
- do not add network TCP listeners;
- do not make SQLite optional;
- do not turn the status socket into the authoritative persistence layer;
- do not persist every live telemetry frame;
- do not create per-profile worker processes;
- do not add arbitrary remote-control commands over the socket beyond the small status/wake/invalidate/request surface required by this plan;
- do not make `sync_runs` a general job queue;
- do not change the existing deletion budget model beyond persisting the effective one-attempt authorization;
- do not change profile IDs, remote ownership rules, or sidecar semantics.

---

## 2. Current implementation facts that drive the change

### 2.1 rclone progress parsing

`internal/exec/rclone.go` currently:

- runs rclone with JSON log stats;
- uses `--stats 10s`;
- parses the top-level `stats.speed` into `ProgressStats.Speed`;
- exposes cumulative counters such as bytes, transfers, checks, listed items, and current transfer metadata.

The top-level rclone speed is unsuitable as the CLI's current-speed value. Keep the cumulative byte counter as the source for the new estimator.

### 2.2 Worker progress persistence

`internal/cli/worker.go` currently calls `app.DB.UpdateRunStats(...)` for each progress callback. `progressSnapshot()` copies `ProgressStats.Speed` into `SpeedBytesPerSecond`.

This is the high-frequency DB path that must be removed from the live update loop.

### 2.3 Status and wait polling

`internal/cli/profilestatus.go` reads current run telemetry from SQLite and prints `Speed` when `speed_bytes_per_second > 0`. `--watch` repeatedly renders by polling the DB.

`internal/cli/wait.go` similarly polls durable state every two seconds.

These observers should share the new status-stream client.

### 2.4 Reconcile execution ownership mismatch

`internal/cli/reconcile.go` documents worker-owned execution but can directly call `executeReconcileAttempt()` after claiming a run in the invoking CLI process.

That contradicts the target worker-owned architecture and prevents the worker from being a complete live-telemetry source.

### 2.5 Watcher fast-upsert ownership mismatch

`internal/cli/watch.go` / `internal/watch/watcher.go` currently allow watcher batches to call `app.upsertForProfile()`, which performs rclone work outside the worker.

The durable `pending_events` table already contains the information needed for the worker to own the actual fast-upsert execution.

### 2.6 Existing intent model

`profile_sync_state.desired_generation` is explicitly the sole durable reconciliation-intent authority.

`pending_events` is a durable set of file-level event facts, not a competing full-reconcile request queue.

`sync_runs` is attempt/history state, not work authority.

The implementation must preserve this layering.

---

## 3. Target module layout

The exact package names may change during implementation, but keep the responsibilities separated.

Suggested additions:

```text
internal/live/
  protocol.go          # versioned request/response structs
  socket_path.go       # default/configured path resolution
  server.go            # worker Unix socket server
  client.go            # observer/request client
  hub.go               # subscriber registry + latest-value fan-out
  snapshot.go          # public worker status snapshot model
  throughput.go        # two-sample throughput estimator

internal/cli/
  config.go            # config get/set/unset socket-path
```

It is also acceptable to keep worker-only aggregation code under `internal/cli` if moving it into a new package would create circular dependencies. The important separation is conceptual:

```text
transport != durable state != telemetry estimation != rendering
```

Do not put socket transport logic into `state.DB`.

---

## 4. Socket path and configuration

### 4.1 Setting key

Add a persisted settings key, for example:

```go
const SettingWorkerSocketPath = "worker_socket_path"
```

The user-facing configuration name remains:

```text
socket-path
```

The internal DB key does not need to match the CLI spelling.

### 4.2 Resolution order

Use one resolver shared by worker and clients:

```text
1. persisted socket-path setting, if non-empty
2. filepath.Join(os.TempDir(), "knowledge-sync", "worker.sock")
```

Do not allow worker and client code to duplicate the resolution logic.

### 4.3 Runtime directory

For the default path:

- `MkdirAll(<temp>/knowledge-sync, 0700)`;
- verify the resolved runtime directory is a directory;
- best-effort `Chmod(..., 0700)` after creation;
- create the socket with user-only access, best-effort `Chmod(socket, 0600)` after `Listen`.

For an explicitly configured socket path:

- create a missing parent directory only when the configured path is clearly inside a path the tool is expected to manage, or require the parent to already exist;
- never recursively chmod arbitrary user-provided parent directories;
- fail with an actionable error if the parent is unusable.

Prefer the safer rule: **configured parent must already exist**. This avoids surprising filesystem mutations outside the default runtime directory.

### 4.4 Stale socket handling

Socket cleanup occurs only after acquiring the existing global worker singleton lock.

Startup algorithm:

```text
acquire worker singleton lock
resolve socket path
inspect existing path

missing:
    listen

existing Unix socket:
    remove stale socket
    listen

existing regular file / directory / symlink / other:
    fail closed; do not delete
```

Because the worker singleton lock is already held, an existing socket at the configured path cannot belong to another legitimate `knowledge-sync` worker using the same state database.

On normal worker shutdown:

- close listener;
- remove the socket path only if it still refers to the socket instance owned by this process, where practical;
- ignore cleanup failure after logging it.

Socket setup failure must not corrupt durable state. The worker may continue reconciliation without live IPC if appropriate, but must log the socket failure prominently so clients naturally use DB fallback.

### 4.5 Config command behavior

Add:

```text
knowledge-sync config get socket-path
knowledge-sync config set socket-path <path>
knowledge-sync config unset socket-path
```

Behavior:

- `get`: print explicit value and/or resolved effective value in a stable, documented format;
- `set`: validate non-empty path, persist it, then restart the launchd worker when the managed worker job is installed;
- `unset`: remove/clear the persisted override, then restart the managed worker;
- if no managed worker exists, report that the new value takes effect on the next worker start.

Do not rely on shell environment variables as the primary configuration surface.

---

## 5. Protocol V1

Use newline-delimited JSON with one JSON object per line.

Every message includes:

```json
{
  "protocol_version": 1,
  "type": "..."
}
```

Unknown fields are ignored for forward compatibility. Unknown message types should result in a protocol error response or clean connection close; they must not crash the worker.

### 5.1 Subscribe request

Illustrative request:

```json
{
  "protocol_version": 1,
  "type": "subscribe",
  "profile_id": "example-profile"
}
```

After receiving a valid subscription, the worker must immediately:

1. refresh durable state for that profile from SQLite once;
2. merge it with the latest live activity if one exists;
3. send a full `status` snapshot.

The client must not wait for the next rclone progress frame before receiving an initial response.

### 5.2 Status snapshot

Prefer one full, replacement-style snapshot type instead of a delta protocol.

Suggested shape:

```json
{
  "protocol_version": 1,
  "type": "status",
  "profile_id": "example-profile",
  "snapshot_seq": 42,
  "sampled_at": "2026-01-01T00:00:00Z",

  "profile": {
    "enabled": true,
    "tombstoned": false,
    "deletion_requested": false
  },

  "sync": {
    "initialized": true,
    "state": "syncing",
    "phase": "uploading",
    "desired_generation": 12,
    "last_success_generation": 11,
    "current_run_id": "<run_id>",
    "last_success_at": null,
    "last_error": null,
    "retry_classification": null,
    "next_retry_at": null
  },

  "activity": {
    "kind": "full_reconcile",
    "run_id": "<run_id>",
    "phase": "uploading",
    "files_completed": 2,
    "files_total": 10,
    "bytes_completed": 1048576,
    "bytes_total": 8388608,
    "checks_completed": 1,
    "checks_total": 2,
    "items_listed": 12,
    "errors_count": 0,
    "current_item": "example.md",
    "current_item_bytes": 524288,
    "current_item_size": 1048576,
    "speed_known": true,
    "speed_bytes_per_second": 7340032,
    "last_measurable_progress_at": "2026-01-01T00:00:00Z",
    "possible_stall": false
  }
}
```

The exact JSON nesting may be simplified, but preserve these semantic distinctions:

- durable profile lifecycle;
- durable synchronization lifecycle;
- ephemeral live activity;
- `speed_known` separate from numeric speed;
- activity kind separate from run kind.

### 5.3 Activity kinds

At minimum support:

```text
full_reconcile
fast_upsert
```

A fast upsert does not need a durable `sync_runs` row.

### 5.4 Invalidate / wake request

External mutation commands may send a best-effort message such as:

```json
{
  "protocol_version": 1,
  "type": "invalidate",
  "profile_id": "example-profile"
}
```

Worker behavior:

- mark the profile durable cache dirty;
- refresh it promptly;
- reevaluate schedulable work;
- publish an updated full status snapshot if state changed.

The request is an optimization only. Durable SQLite state remains authoritative.

### 5.5 Protocol mismatch

If client and worker protocol versions are incompatible:

- do not fail the entire `profile status` command solely because live IPC is incompatible;
- record/report live telemetry as unavailable;
- use SQLite fallback;
- `--watch` / wait loops continue periodically attempting to reconnect.

### 5.6 Backpressure

Each subscriber gets a bounded/latest-value delivery slot.

Recommended implementation:

- per-client channel capacity `1`;
- publishing is non-blocking;
- when full, replace/drop the stale pending snapshot and retain the newest state;
- connection writer failure disconnects that client only.

Invariant:

> No socket client can block the worker reconciliation path or rclone progress callback.

---

## 6. Worker status aggregator

### 6.1 Durable cache

Maintain worker-side cached durable status per profile.

Refresh it when:

- worker starts/recoveries complete;
- a client newly subscribes to that profile;
- worker itself performs a durable state mutation that changes public status;
- an external invalidate message arrives;
- the normal worker scheduling rescan observes changed durable state.

Do not refresh SQLite for each telemetry frame.

### 6.2 Live activity state

Maintain ephemeral state for the currently executing activity.

Because the current worker executes synchronously and is globally serialized by its worker loop/remote lease behavior, the first implementation can use a small map keyed by profile ID rather than overengineering a distributed activity registry.

Each activity stores at least:

```text
profile_id
kind
run_id (nullable for fast_upsert)
phase
started_at
latest cumulative counters
latest current item
throughput estimator state
last measurable progress timestamp
latest public activity snapshot
```

### 6.3 Full snapshot generation

Every publish generates a complete replacement snapshot from:

```text
durable cache + latest activity state
```

No SQL query is performed merely because a 1 Hz telemetry frame arrived.

### 6.4 Subscriber initial state

On subscribe:

- validate profile existence;
- perform a fresh durable read for that profile;
- merge any live activity already in memory;
- immediately emit a status snapshot.

This prevents one-shot `profile status` from returning a stale cache after a lost invalidate message.

---

## 7. Real-time throughput estimator

Add a small unit-testable estimator independent of rclone parsing.

Suggested interface:

```go
type ThroughputEstimator struct {
    // previous sample internals
}

type RateSample struct {
    Known          bool
    BytesPerSecond float64
}

func (e *ThroughputEstimator) Observe(totalBytes int64, now time.Time) RateSample
func (e *ThroughputEstimator) Reset()
```

Implementation rules:

1. first observation returns `Known=false`;
2. later observations use `deltaBytes / deltaTime`;
3. negative byte deltas reset the baseline and return unknown rather than emitting a negative rate;
4. zero positive-time delta bytes returns `Known=true, BytesPerSecond=0`;
5. non-positive elapsed time returns unknown and refreshes/reset baseline as appropriate;
6. use the supplied clock/sample timestamp so tests are deterministic;
7. reset estimator between activities/runs;
8. do not carry a prior run's last sample into a new run.

The estimator is whole-activity throughput because it is based on aggregate cumulative bytes, not a sum of per-file rates.

---

## 8. rclone progress cadence and parsing

### 8.1 Stats interval

Change the progress-enabled rclone invocation from `--stats 10s` to approximately:

```text
--stats 1s
```

Keep structured JSON log parsing.

### 8.2 Raw speed field

`ProgressStats.Speed` may remain temporarily for compatibility with parser tests or other callers, but live public status must stop treating it as current throughput.

Prefer one of these cleanup paths:

- rename/document it as `RcloneAverageSpeed` if retaining it is useful;
- or stop propagating it into public worker telemetry once no consumer needs it.

Do not silently keep two fields both named/understood as `Speed`.

### 8.3 Current-item parsing

The existing current-item fields may continue to use the current rclone transfer element behavior for this implementation. Improving multi-transfer current-item presentation is out of scope.

---

## 9. Sparse durable telemetry checkpoints

### 9.1 Separate live update from persistence

Replace this conceptual flow:

```text
rclone frame
  -> progressSnapshot
  -> UpdateRunStats SQL
```

with:

```text
rclone frame
  -> activity memory update
  -> throughput estimate
  -> live status publish
  -> maybeCheckpointTelemetry()
```

### 9.2 Checkpoint triggers

Persist coarse progress when any of these is true:

1. a required durable write is already happening and latest telemetry can be included cheaply;
2. phase changes;
3. run/activity succeeds;
4. run/activity fails;
5. one hour has elapsed since the last telemetry checkpoint during a still-running long activity.

The implementation may use an in-memory `lastCheckpointAt` per active durable run.

### 9.3 What to persist

For durable reconciliation runs, coarse checkpoints may continue populating existing columns:

```text
files_completed
bytes_total
bytes_completed
last_progress_at
last_heartbeat_at
checks_completed
checks_total
items_listed
errors_count
current_item
current_item_bytes
current_item_size
```

Do not rely on `speed_bytes_per_second` for live display. Either:

- set it to `0` on new checkpoints; or
- keep a coarse last-known value only for backward schema compatibility while ensuring no new UI treats it as live.

Prefer setting/clearing it so stale live-looking speed does not survive a restart.

### 9.4 Fallback rendering

When live socket telemetry is unavailable:

- durable transfer counters may be shown with a label/line indicating the snapshot is coarse;
- show checkpoint/heartbeat age;
- omit `Speed`;
- omit live `possible stall` diagnosis.

---

## 10. Durable manual/scheduled reconciliation intent

### 10.1 Preserve one authority

Do not add `reconcile_requests` as a second work queue.

Extend `profile_sync_state` with pending execution metadata attached to the existing generation intent.

Suggested migration fields:

```text
pending_manual_generation INTEGER NULL
pending_manual_allow_deletes INTEGER NULL
pending_manual_bypass_debounce INTEGER NOT NULL DEFAULT 0
pending_request_origin TEXT NULL
```

The exact set may be smaller. Avoid storing derivable or redundant fields.

A cleaner model is to make the manual generation itself the marker that options apply to the next claim satisfying/covering that generation.

### 10.2 Submission helper

Add one transactional state API for manual reconcile intent, for example:

```go
type ManualReconcileIntent struct {
    AllowDeletes   int
    BypassDebounce bool
}

func (d *DB) SubmitManualReconcile(profileID string, intent ManualReconcileIntent) (submittedGeneration int64, err error)
```

Transaction requirements:

- ensure sync state exists;
- advance `desired_generation` by at least one opportunity even if source generation did not change;
- reopen the manual gate using the existing explicit-manual semantics as required;
- record the new manual metadata;
- replace any previously unconsumed manual metadata;
- return the exact submitted generation used by the CLI waiter.

### 10.3 Scheduled intent helper

Scheduled safety reconciliation also needs a durable generation even when no source debt exists.

Add a helper that advances/coalesces the generation without overriding pending manual metadata.

Scheduled behavior must preserve destructive debounce semantics that manual reconcile may bypass.

### 10.4 Merge rules

When multiple events occur before claim:

```text
manual request
filesystem event
scheduled request
```

apply these rules:

- `desired_generation` advances to cover all known intent;
- filesystem/scheduled changes never clear pending manual options;
- a newer manual request replaces older pending manual options;
- the eventual run captures the latest `desired_generation` as today;
- one run may therefore satisfy several waiters.

---

## 11. Claim-time one-attempt options and audit

### 11.1 Schema

Extend `sync_runs` with audit fields such as:

```text
effective_max_delete INTEGER NOT NULL DEFAULT 0
manual_delete_override INTEGER NULL
```

Alternative names are acceptable if semantics remain explicit.

### 11.2 Atomic claim

The transaction that creates the run must also:

1. read pending manual metadata;
2. compute the attempt's effective delete limit;
3. write that value into the new `sync_runs` row;
4. clear/consume the one-shot manual override;
5. preserve any newer intent that arrived after the generation/options being consumed.

Do not consume a manual override in a separate transaction before the run row exists.

### 11.3 Retry behavior

If the attempt fails:

- automatic retry remains represented by durable generation debt/retry state;
- the manual override does not reappear;
- the next automatic attempt uses the profile's persistent delete budget unless the user submits a new manual override.

### 11.4 Execution options

`executeReconcileAttempt()` should receive attempt options derived from the claimed durable run/claim result, not from the original CLI process.

Avoid maintaining a hidden in-memory map of manual CLI options in the worker.

---

## 12. Make the worker the sole full-reconcile executor

### 12.1 Refactor command entrypoints

Change `runReconcile()` and related command paths so they no longer:

```text
ClaimRun -> executeReconcileAttempt in CLI process
```

Instead:

```text
persist intent
best-effort wake worker
optionally wait for submitted generation
```

Manual `reconcile-now`:

- submit manual durable intent;
- ensure/wake worker;
- wait using the unified status client until the submitted generation succeeds or a blocking lifecycle/terminal condition occurs.

Scheduled reconcile:

- submit scheduled durable intent;
- wake worker;
- exit success once submission is durable.

### 12.2 Worker claiming

The worker remains the only code path allowed to claim a reconciliation attempt for execution.

A future cleanup should make non-worker direct calls to `executeReconcileAttempt()` difficult by package/API structure, not merely by comments.

### 12.3 `force` cleanup

Once manual/scheduled "run even without source changes" is represented by advancing `desired_generation`, reduce or remove `force` claim modes where possible.

The desired end state is one debt-driven claim rule:

```text
desired_generation > last_success_generation
```

with explicit metadata for debounce/authorization rather than a parallel force path.

Do this carefully and only after migration tests prove the existing safety-net behavior is preserved.

---

## 13. Move fast upserts into the worker

### 13.1 Watcher responsibility after migration

The watcher should only:

```text
fswatch event
  -> classify/filter
  -> RecordEvent(...) transaction
  -> best-effort wake/invalidate worker
```

It must no longer call rclone copy/upsert itself.

### 13.2 Durable debounce

Keep the existing effective fast-event semantics:

```text
settle: 3s
max delay: 30s
```

Move due-batch evaluation into worker scheduling using durable `pending_events.first_seen` / `last_seen` timestamps.

Do not rely solely on watcher in-memory debounce maps after the execution migration; otherwise a watcher restart could change work eligibility.

### 13.3 Full debt priority

Preserve the current semantic rule:

> If full reconciliation debt exists, full reconcile wins over fast upsert.

Worker scheduling order for a profile should be conceptually:

```text
1. lifecycle/deletion gates
2. eligible full reconciliation debt
3. due fast pending-event batch
4. idle
```

Remote lease/concurrency rules still apply.

### 13.4 Fast-upsert execution

Worker algorithm:

1. read a stable pending event batch;
2. recheck profile/filter eligibility;
3. execute fast upsert through the existing remote scheduler/lease mechanism;
4. publish live activity kind `fast_upsert` and throughput telemetry;
5. on success, clear only the exact event versions included in the batch using the existing generation-safe deletion behavior;
6. mark fast success;
7. if a destructive/uncertain/new full-debt condition appeared meanwhile, leave/advance full reconciliation debt.

### 13.5 Fast status durability

Do not create a `sync_runs` row for every fast batch solely for telemetry.

Use live worker activity for status and retain the existing coarse runtime markers such as last fast success.

---

## 14. Unified observer client

Create one observer abstraction used by:

```text
profile status
profile status --watch
profile wait
profile add --wait
reconcile-now waiting
```

### 14.1 One-shot status

Algorithm:

1. resolve socket path;
2. attempt connection with a short bounded timeout;
3. subscribe to profile;
4. receive initial full snapshot;
5. render once and exit;
6. on connection/protocol failure, render SQLite fallback.

Do not make one-shot status sleep for a telemetry tick.

### 14.2 Watch mode

Algorithm:

```text
connect socket
subscribe
render each new full snapshot

on disconnect:
    switch to DB fallback
    periodically retry socket

on reconnect:
    replace fallback view with worker snapshot
```

Terminal conditions remain equivalent to current semantics:

- ready -> success;
- terminal error -> failure;
- deleted/deleting/disabled -> appropriate failure for wait/watch operations.

### 14.3 Wait mode

`profile wait` and `profile add --wait` use the same stream but do not need to render every telemetry frame.

They may ignore non-terminal presentation-only updates and only evaluate lifecycle/generation conditions.

Ctrl-C/SIGTERM behavior remains observer-only: interrupting the waiter must not cancel worker reconciliation.

### 14.4 Generation waiter

For manual reconciliation:

```go
type WaitCondition struct {
    ProfileID         string
    MinimumGeneration int64
}
```

Success is reached only when:

```text
last_success_generation >= MinimumGeneration
```

A `ready` state for an older generation must not prematurely satisfy a newly submitted manual reconcile waiter.

---

## 15. Worker availability and launchd lifecycle

### 15.1 Installation invariant

An enabled profile requires the global worker job.

Update flows so:

- profile creation of an enabled profile ensures worker job installation;
- profile enable ensures worker job installation;
- `install` continues installing the worker when profiles are installed;
- removing/disabling one profile must not stop the global worker if other enabled profiles exist.

### 15.2 Wake behavior

Use the socket invalidate/wake when available.

If the worker is managed by launchd but socket connection fails, a command may request launchd kickstart/reload using existing launchd helpers where appropriate, then let the observer reconnect.

Do not execute the transfer inside the CLI as a fallback.

### 15.3 Config path restart

Changing `socket-path` must restart the managed worker so worker and clients do not remain on different paths indefinitely.

The restart sequence should preserve durable work; worker recovery already owns orphaned-attempt handling.

---

## 16. Database migrations

Add one new schema migration version after the current latest migration.

The migration should cover only durable fields required by this plan.

Expected categories:

### `profile_sync_state`

Pending manual execution metadata, minimally sufficient to represent:

- which submitted generation the current manual metadata belongs to;
- one-shot delete override;
- manual debounce bypass/origin if not derivable elsewhere.

### `sync_runs`

Audit fields:

- effective delete budget for this attempt;
- nullable manual override value or explicit source flag.

### `settings`

No schema change is required because the settings table already stores arbitrary key/value rows. Add only the new setting constant and CLI handling.

### Existing telemetry columns

Do not drop old progress/speed columns in this change. A destructive schema cleanup can be evaluated after the new architecture is stable.

Migration requirements:

- existing databases upgrade in place;
- default values preserve existing run scans;
- old historical rows remain readable;
- no migration derives new values from environment-specific files;
- migration tests cover a pre-change schema upgraded to the new version.

---

## 17. Implementation phases

Implement in the following order so each step remains testable and avoids a large flag-day refactor.

### Phase 1 - Introduce protocol, socket resolver, and throughput estimator

Files likely touched/added:

```text
internal/state/settings.go
internal/live/protocol.go
internal/live/socket_path.go
internal/live/throughput.go
internal/live/*_test.go
internal/cli/config.go
internal/cli/root.go
```

Deliverables:

- socket-path setting/resolution;
- config get/set/unset command surface;
- protocol structs/version constant;
- estimator with deterministic unit tests;
- no behavior change to worker execution yet.

### Phase 2 - Add worker socket server and full status snapshots

Files likely touched:

```text
internal/cli/worker.go
internal/live/server.go
internal/live/hub.go
internal/live/snapshot.go
internal/state/* read helpers as needed
```

Deliverables:

- worker listener after singleton lock acquisition;
- stale socket protection;
- subscribe/initial snapshot;
- durable cache;
- latest-value subscriber fan-out;
- protocol mismatch handling;
- socket failure does not break reconciliation correctness.

At this phase live activity may still be minimal until progress wiring is added.

### Phase 3 - Add 1 Hz telemetry and correct recent throughput

Files likely touched:

```text
internal/exec/rclone.go
internal/exec/rclone_test.go
internal/cli/worker.go
internal/live/throughput.go
```

Deliverables:

- `--stats 1s`;
- live activity updates in worker;
- two-sample throughput rate;
- `speed_known` semantics;
- phase-aware Speed visibility;
- no dependency on rclone top-level average speed for public status.

Initially it is acceptable to keep existing DB progress writes until Phase 5 if doing so reduces migration risk, but tests must already prove socket Speed is sourced from the estimator.

### Phase 4 - Move observer commands to socket-first

Files likely touched:

```text
internal/cli/profilestatus.go
internal/cli/wait.go
internal/cli/profile.go
internal/live/client.go
```

Deliverables:

- one-shot status socket-first;
- watch subscription/reconnect;
- wait reuse;
- DB fallback without Speed/stall claim;
- no normal 2s SQLite polling while socket is healthy.

### Phase 5 - Introduce sparse DB telemetry persistence

Files likely touched:

```text
internal/cli/worker.go
internal/state/syncstate.go
internal/state/runs.go
```

Deliverables:

- remove per-frame `UpdateRunStats` writes;
- lifecycle/opportunistic/1h checkpoint policy;
- clear stale live speed on fallback persistence;
- tests counting or instrumenting progress SQL writes over many frames.

This phase realizes the primary SQL-write-reduction goal.

### Phase 6 - Durable manual/scheduled intent metadata and run audit

Files likely touched:

```text
internal/state/migrate.go
internal/state/syncstate.go
internal/state/runs.go
internal/state/*_test.go
```

Deliverables:

- migration;
- manual submit helper returning generation;
- scheduled submit helper;
- merge semantics;
- atomic claim-time consumption of one-shot options;
- run audit fields;
- retry does not inherit manual delete override.

### Phase 7 - Move full reconcile execution exclusively into worker

Files likely touched:

```text
internal/cli/reconcile.go
internal/cli/worker.go
internal/cli/async_test.go
```

Deliverables:

- CLI no longer directly executes `executeReconcileAttempt()`;
- worker claims/executes all reconciliation attempts;
- reconcile-now waits by submitted generation;
- scheduled reconcile queues and exits;
- gate/debounce behavior preserved;
- remove/reduce force-mode claim paths when safe.

### Phase 8 - Move fast upsert execution into worker

Files likely touched:

```text
internal/cli/watch.go
internal/watch/watcher.go
internal/cli/worker.go
internal/state/events.go
internal/cli/scheduler.go
```

Deliverables:

- watcher is event-only;
- worker evaluates durable fast debounce;
- worker executes fast upsert;
- live `fast_upsert` activity status;
- full-debt priority preserved;
- generation-safe event clearing preserved.

### Phase 9 - Worker lifecycle hardening

Files likely touched:

```text
internal/cli/jobs.go
internal/cli/install.go
internal/cli/profile.go
internal/launchd/*
```

Deliverables:

- add/enable/install ensure global worker;
- socket-path config restart;
- worker unavailable errors are explicit without CLI transfer fallback;
- multi-profile disable/remove behavior does not unnecessarily remove the global worker.

### Phase 10 - Cleanup and compatibility assertions

Deliverables:

- remove dead direct execution helpers/call sites;
- update comments that currently claim worker ownership inaccurately;
- ensure old DB speed is not rendered as live;
- update command help text;
- add architecture comments where invariants are non-obvious;
- run repository-wide tests and security/sensitive-content checks.

---

## 18. Test plan

Tests are required at unit, state-machine, IPC, and end-to-end levels.

All examples and fixtures must be synthetic per `AGENTS.md`.

### 18.1 Throughput estimator unit tests

Required cases:

- first sample -> unknown;
- two samples with exact byte/time delta -> expected rate;
- zero byte delta -> known zero;
- irregular sample intervals use actual elapsed time;
- negative cumulative byte delta -> reset/unknown;
- non-positive elapsed duration -> safe unknown;
- reset between runs prevents cross-run spike;
- large byte values do not overflow intermediate arithmetic.

### 18.2 Socket path tests

Required cases:

- default path uses injected/controlled temp dir where testable;
- configured path overrides default;
- unset returns to default;
- default runtime directory gets private permissions on supported systems;
- stale Unix socket is replaceable after worker ownership;
- regular file at socket path is never deleted;
- symlink at socket path is never blindly removed;
- multiple worker server attempts respect singleton ownership.

### 18.3 Protocol/server tests

Required cases:

- subscribe returns an immediate initial snapshot;
- unknown profile returns a stable error response;
- incompatible protocol causes client fallback, not panic;
- multiple subscribers receive the same latest status;
- slow subscriber does not block publisher;
- dropped intermediate frames still lead to newest snapshot;
- disconnect/reconnect succeeds;
- server shutdown closes clients cleanly enough for fallback.

### 18.4 Durable cache tests

Required cases:

- subscribe refreshes durable state once;
- telemetry publish after subscribe performs no additional DB read when durable state is unchanged (instrument/fake DB if practical);
- invalidate reloads durable profile state;
- lost invalidate is eventually corrected by periodic worker rescan;
- worker-owned durable mutation updates subsequent snapshot promptly.

### 18.5 Observer tests

Required cases:

- one-shot status uses socket snapshot when available;
- one-shot status falls back to DB when unavailable;
- fallback omits Speed;
- fallback does not print false `possible stall` from sparse checkpoint age;
- `--watch` switches socket -> DB -> socket across worker restart;
- `profile wait` shares stream behavior and terminal conditions;
- observer interruption never cancels reconciliation.

### 18.6 Sparse write tests

Use an injected/fake progress stream with many frames.

Assert:

- N live progress frames do not produce N `UpdateRunStats` writes;
- phase transition forces checkpoint;
- success/failure forces checkpoint;
- one-hour checkpoint guard behaves correctly with a fake clock;
- live snapshots continue at frame cadence despite sparse DB writes.

### 18.7 Manual intent merge tests

Required sequences:

```text
manual -> filesystem -> claim
manual -> scheduled -> claim
manual A -> manual B -> claim
filesystem -> manual -> claim
```

Assert:

- desired generation is monotonic;
- non-manual intent does not erase pending manual options;
- newest manual request replaces older unconsumed manual options;
- one claimed run captures the latest target generation;
- older submitted generation waiters can be satisfied by the later run.

### 18.8 Delete override audit tests

Required cases:

- manual override is written into claimed run audit fields;
- default budget produces no fake manual override;
- override is consumed atomically with claim;
- retry after failed attempt does not inherit override;
- a later explicit manual request can grant a new override;
- crash/recovery does not resurrect already-consumed override.

### 18.9 Worker-only full reconcile tests

Required cases:

- `reconcile-now` submits intent and does not execute rclone in CLI process;
- scheduled command submits and exits;
- worker claims and executes submitted work;
- worker unavailable leaves durable intent intact;
- no CLI fallback transfer occurs;
- generation waiter returns only after its submitted generation is successful.

### 18.10 Worker-only fast upsert tests

Required cases:

- watcher records event but does not execute rclone;
- worker executes due fast batch;
- fast activity emits live status;
- exact event versions are cleared on success;
- newer same-path event survives clear;
- full debt suppresses fast execution and wins priority;
- destructive events promote full reconcile rather than unsafe fast copy;
- worker restart can resume from durable pending events.

### 18.11 Launchd/lifecycle tests

Where launchd integration is unit-testable via generated configs/mocks:

- add/enable ensure worker job;
- socket-path change restarts managed worker;
- changing path without managed worker reports next-start semantics;
- disabling one profile does not remove worker needed by another profile.

### 18.12 Repository regression suite

Run at minimum:

```bash
go test ./...
```

plus existing project-specific integration/e2e test commands documented in the repository.

---

## 19. Failure-mode requirements

### Worker crashes mid full reconcile

- existing orphan-run recovery remains authoritative;
- live socket disappears;
- observers fall back to coarse DB state;
- launchd restart reacquires singleton ownership and recreates socket;
- retry proceeds according to durable retry/generation state;
- consumed `--allow-deletes` does not reappear.

### Worker crashes mid fast upsert

- un-cleared `pending_events` remain durable;
- on restart the worker reevaluates the batch;
- operation may repeat idempotently as required by current fast-upsert semantics;
- no event is considered completed merely because telemetry said it was in progress.

### Client crashes or is slow

- worker keeps running;
- no reconciliation cancellation;
- no blocked progress callback;
- no unbounded per-client queue.

### Socket path is unusable

- worker logs live IPC failure;
- reconciliation can remain durable/correct;
- observers use SQLite fallback;
- no unrelated path is deleted or chmodded.

### External mutation invalidate is lost

- SQLite commit remains authoritative;
- new subscriber forces a fresh read;
- periodic worker rescan eventually refreshes existing subscribers.

### New CLI talks to old worker / old CLI talks to new worker

- protocol version mismatch causes graceful fallback;
- no corrupted status state;
- commands whose correctness requires the new worker-only execution path must fail clearly rather than accidentally executing competing transfer logic during partial upgrade.

The implementation should document any minimum same-version requirement for execution commands during rolling local upgrades.

---

## 20. Security and repository-safety requirements

Follow `AGENTS.md` throughout implementation.

In particular:

- no real remote names, Drive IDs, account identifiers, home paths, filenames, or logs in tests/docs;
- socket tests use `t.TempDir()` or synthetic paths;
- protocol examples use synthetic profile IDs and filenames;
- socket directory/file permissions are restrictive;
- configured socket paths are treated as untrusted user input for filesystem mutation purposes;
- no secret/auth material is added to the socket protocol;
- the local Unix socket relies on filesystem permissions and same-user local access rather than inventing token authentication for V1.

Before every implementation commit, perform the repository's required staged-diff and sensitive-content review.

---

## 21. Acceptance criteria

The implementation is complete only when all of the following are true.

### Live speed correctness

- During a controlled progress test, displayed speed equals recent `delta bytes / delta time`, not rclone's cumulative average.
- First sample does not display a false zero/average speed.
- A genuine no-byte interval can display `0 B/s` as a known value.
- Speed disappears outside transfer phase.

### SQL-write reduction

- A long-running transfer with 1 Hz progress does not perform 1 SQL telemetry write per second.
- In the no-other-write case, coarse periodic telemetry persistence occurs no more frequently than once per hour.
- Phase and terminal boundaries remain durably represented.

### Status transport

- Healthy worker + healthy socket: status/watch/wait do not use SQLite as a repeated IPC polling loop.
- New subscribers get an immediate full snapshot.
- Multiple observers do not affect transfer throughput or worker progress.
- Worker restart causes temporary fallback and automatic live reconnection.

### Worker ownership

- No CLI reconciliation command directly runs the synchronization rclone data plane.
- The watcher does not directly run fast synchronization rclone operations.
- Worker is the sole synchronization executor.

### Durable correctness

- `desired_generation` remains the sole full-reconcile intent authority.
- Manual/scheduled/filesystem intents coalesce without losing required work.
- One-attempt destructive override cannot leak into automatic retry.
- `sync_runs` records the effective destructive budget for audit.
- waiter-by-generation behaves correctly across coalesced requests.

### Fallback correctness

- Socket failure does not lose durable work.
- DB fallback does not claim to know live Speed.
- DB fallback does not produce false live-stall warnings from intentionally sparse checkpoints.

---

## 22. Recommended implementation commit sequence

Keep implementation commits reviewable and behavior-focused. A suggested sequence is:

```text
1. Add live status protocol and throughput estimator
2. Add configurable worker Unix socket server
3. Serve worker status snapshots over Unix socket
4. Use socket-first status and wait observers
5. Reduce durable progress checkpoint writes
6. Persist manual reconcile intent metadata and delete audit
7. Route full reconciliation execution through worker
8. Route fast upserts through worker
9. Harden worker launchd lifecycle and socket reconfiguration
10. Remove obsolete direct-execution and live-speed DB paths
```

Each commit should keep tests passing where practical. If a temporary compatibility shim is required between phases, make it explicit and remove it in the final cleanup commit.

---

## 23. Implementation invariants checklist

An implementation review should reject a change if any of these become false:

- `desired_generation` is the only durable full-reconciliation work authority.
- `sync_runs` describes attempts/history, not queued intent.
- pending fast events remain durable until worker-confirmed success or deliberate promotion to full reconciliation.
- only the worker executes synchronization rclone operations.
- socket telemetry is observational and cannot block synchronization.
- live Speed comes from recent cumulative-byte deltas.
- unknown speed and measured zero are different states.
- live telemetry does not require per-frame SQL writes.
- new subscribers get a durable refresh before their first snapshot.
- normal telemetry snapshots do not query SQLite at 1 Hz.
- external mutations commit SQLite before best-effort worker notification.
- socket failure always has a durable fallback path.
- fallback data is not mislabeled as live data.
- a manual delete override applies to one claimed attempt only.
- destructive authorization is auditable in the run record.
- observers wait by durable generation when they need to identify their submitted reconciliation opportunity.

---

## 24. Final target state

After this plan is implemented, the system should have a clear division of responsibilities:

```text
SQLite
  = durable lifecycle, reconciliation intent, retry state,
    event facts, run audit, sparse recovery checkpoints

Worker
  = only synchronization executor,
    scheduler, durable-state aggregator, live telemetry owner

Unix socket
  = low-latency local observation and best-effort wake/invalidation

CLI observers
  = socket-first views/waiters with SQLite fallback

Watcher
  = filesystem event producer only
```

The original speed bug then disappears as a consequence of the correct architecture rather than being patched at the presentation layer:

```text
rclone cumulative bytes @ ~1 Hz
        -> worker recent-rate estimator
        -> in-memory full status snapshot
        -> Unix socket
        -> CLI Speed
```

SQLite remains essential for durability, but live telemetry no longer turns it into a high-frequency cross-process message bus.
