# knowledge-sync modification plan: durable async initial sync

## 0. Status

This is an **incremental modification plan** for the existing `knowledge-sync` implementation.

The local implementation is already substantially complete and tests are passing. The problem addressed here is specifically the first-run behavior of:

```bash
knowledge-sync profile add <profile> ...
```

when creating a real profile for the first time.

Current behavior effectively couples profile creation to the initial remote upload, so a large first synchronization can make `profile add` appear to hang while rclone / Drive transfer is still running.

This plan changes that lifecycle without redesigning the existing sync engine.

The core change is:

> `profile add` should commit the profile and durable initial-sync intent, then return. The worker owns the actual initial reconciliation. Progress and final success are observed independently.

This is **not** implemented as an ad-hoc detached child process.

The intended architecture remains:

```text
CLI
  -> control plane / durable state

worker
  -> execution plane / reconciliation

Google Drive
  -> remote convergence target
```

The existing synchronous behavior remains available through an explicit wait mode for tests, scripts, and users who need it.


### 0.1 Design decisions locked by plan review

The following details are settled for this modification and should not be reopened during implementation unless inspection of the existing implementation reveals a concrete incompatibility:

```text
generation state = sole durable reconciliation intent
sync_runs         = execution attempts/history, not a second queue authority

initialized       = sticky evidence that at least one successful reconciliation completed
state             = current operational state

V1 worker model   = one worker process at runtime
core invariant    = at most one active run per profile
```

Additional locked semantics:

- every claimed run captures a durable `target_generation`;
- filesystem events during a run may advance `desired_generation` but cannot expand that run's target retroactively;
- retryability is a structured result from the reconciliation/transport boundary, not string parsing in the worker;
- retry/backoff gates are durable and survive restart;
- terminal errors are not cleared by ordinary filesystem changes;
- `knowledge-sync sync <profile>` is the explicit control-plane request to retry/reconcile now; it never runs a competing transfer inside the CLI process;
- `profile wait` and `profile add --wait` observe readiness across retryable attempts; interrupting a waiter never cancels worker-owned reconciliation;
- profile deletion intent is durable and has higher scheduling priority than reconciliation/retry intent;
- V1 serializes deletion behind an active run rather than introducing transfer cancellation solely for this feature;
- `profile remove` is asynchronous when an active run must finish first;
- `paused` is not a new V1 requirement: if pause/resume already exists, preserve it with the semantics defined below; otherwise do not add it solely for this change.

---

## 1. Problem statement

### 1.1 Current UX problem

For a new profile with a large local source:

```bash
knowledge-sync profile add vault ~/Vault
```

may need to perform:

```text
profile validation
profile persistence
local scan
remote comparison
initial upload
full reconciliation
```

before the command exits.

If the source is large, the user sees a long-running foreground command whose main remaining work is network transfer.

Operationally this creates several problems:

1. `profile add` appears stuck even though the system is making progress.
2. closing the terminal risks coupling user session lifetime to the initial sync execution path;
3. progress is visible only through the command that started the work;
4. profile existence and successful initial synchronization become implicitly conflated;
5. scripts cannot easily distinguish:
   - profile creation failure;
   - initial sync still running;
   - initial sync failure after successful profile creation;
6. future commands such as compiler execution need an explicit way to know whether a profile exists but has never completed a successful reconciliation.

### 1.2 Architectural problem

A profile is control-plane state.

An initial sync is execution-plane work.

They should not share the same command lifetime.

The desired invariant is:

> Once `profile add` returns success, the profile and the need for its initial reconciliation are durably recorded even if the invoking terminal disappears immediately afterward.

---

## 2. Goals

This modification should provide:

1. non-blocking `profile add` by default;
2. durable initial-sync scheduling;
3. worker-owned initial reconciliation;
4. observable synchronization state independent of the creating CLI process;
5. progress reporting for large first uploads;
6. an explicit foreground compatibility mode;
7. clean semantics for profile existence vs sync readiness;
8. crash/restart recovery without relying on detached child processes;
9. no duplication of sync execution logic;
10. minimal disruption to the already-working synchronization implementation.

---

## 3. Non-goals

This change does **not** attempt to:

- redesign rclone transport;
- redesign full reconciliation correctness;
- add a distributed job queue;
- introduce another daemon;
- add compiler-trigger automation;
- add incremental compiler behavior;
- change local → Drive sync eligibility rules;
- make `profile add` responsible for remote completion again through another code path;
- guarantee byte-perfect progress if the underlying transfer backend cannot expose it reliably;
- implement arbitrary concurrent sync jobs for the same profile.

The existing sync engine remains the correctness authority.

---

## 4. Core behavior change

### 4.1 New default

After this change:

```bash
knowledge-sync profile add vault ~/Vault
```

means:

```text
validate requested profile
        |
        v
persist profile
        |
        v
durably mark initial reconciliation required
        |
        v
signal/wake worker if applicable
        |
        v
return success
```

It does **not** wait for the initial remote upload to finish.

Example output:

```text
Profile "vault" created.
Initial sync queued.

Check progress:
  knowledge-sync profile status vault

Follow progress:
  knowledge-sync profile status vault --watch
```

### 4.2 Explicit blocking mode

Retain a synchronous user path:

```bash
knowledge-sync profile add vault ~/Vault --wait
```

Meaning:

```text
create profile
enqueue durable initial reconciliation
observe the same worker-owned run
wait until ready or a terminal blocking condition
return final result
```

`--wait` must not invoke a separate sync implementation.

It is only an observation/waiting mode over the same durable execution path.

---

## 5. Terminology

Use explicit terminology in code and CLI behavior.

### Profile created

The profile definition has been validated and committed to the local control plane.

### Initial sync queued

The system has durably recorded generation/control-plane intent requiring the profile's first full reconciliation.

`queued` here is a user-facing description of durable intent. It does not require a queued `sync_runs` row.

### Initial sync running

The worker has claimed an execution attempt and is actively scanning, comparing, transferring, or reconciling.

### Initialized

At least one complete successful reconciliation has been durably committed for the profile.

Initialization is a sticky historical fact. A later synchronization error does not make the profile “never initialized.”

### Sync ready

The profile is initialized and the currently known desired generation has been successfully reconciled, with no known reconciliation debt preventing current convergence.

Therefore:

```text
initialized = true
ready       = false
```

is valid while a later reconciliation is pending/running or currently blocked by an error.

### Sync error

The current operational reconciliation state is blocked by a failed attempt.

An error may be:

```text
retryable
  -> automatic durable retry/backoff remains scheduled

terminal
  -> automatic claim is blocked until an explicit sync/retry request reopens eligibility
```

A profile may therefore validly exist in this state:

```text
profile exists = true
initialized    = false
state          = error
```

A previously initialized profile may also be in `error` while retaining `initialized = true`.

These distinctions are intentional.

---

## 6. Desired lifecycle

Recommended lifecycle:

```text
                   +------------------+
                   | profile missing  |
                   +---------+--------+
                             |
                         profile add
                             |
                             v
                   +------------------+
                   | profile created  |
                   | initial pending  |
                   +---------+--------+
                             |
                         worker claims
                             |
                             v
                   +------------------+
                   | initial syncing  |
                   +----+--------+----+
                        |        |
                  success        failure
                        |        |
                        v        v
                  +---------+  +---------+
                  |  ready  |  |  error  |
                  +----+----+  +----+----+
                       |            |
                 later changes     retry /
                       |         reconciliation
                       +-------> sync worker
```

A failed initial sync must not delete the profile.

A retry must use the persisted profile and normal reconciliation logic.

---

## 7. Control-plane model

### 7.1 Profile persistence

`profile add` should complete its durable control-plane write before returning success.

At minimum:

```text
profiles
  id
  type
  source_path
  configuration
  created_at
  updated_at
```

Existing schema should be reused where possible.

Profile lifecycle state and synchronization state are separate concepts. If durable asynchronous deletion needs an explicit field, use either an existing lifecycle mechanism or an additive equivalent such as:

```text
lifecycle_state
  active
  deleting
```

or:

```text
deletion_requested_at
```

Do not overload synchronization `state` with deletion ownership.

### 7.2 Durable sync state

Add or formalize profile sync state.

Recommended conceptual fields:

```text
profile_sync_state
  profile_id

  desired_generation
  last_success_generation

  initialized_at
  last_success_at

  current_run_id
  current_state
  current_phase

  retry_classification
  consecutive_failures
  next_retry_at

  last_error_code
  last_error
  last_progress_at
```

The exact schema may differ if equivalent fields already exist.

`desired_generation` is the **sole durable source of reconciliation intent**. A `sync_runs` row must not become a second competing queue authority.

The base debt predicate is:

```text
desired_generation > last_success_generation
```

with `NULL` / never-successful initialization handled explicitly by project conventions.

However, reconciliation debt is not by itself sufficient to claim work. Claimability also respects durable lifecycle and retry gates:

```text
reconciliation debt exists
AND profile lifecycle is active
AND no active run exists
AND pause gate is open, if pause already exists
AND (
      no error gate
      OR retryable error AND now >= next_retry_at
      OR explicit sync/retry request has reopened eligibility
    )
```

A terminal error keeps reconciliation debt durable but closes the automatic claim gate. Ordinary filesystem events may advance `desired_generation`; they do **not** prove that a terminal error such as authentication/configuration failure has been repaired and therefore do not clear that gate.

### 7.3 Initialization is a sticky fact, not the current state

Keep these concepts orthogonal:

```text
initialized / initialized_at
  = historical evidence that at least one complete reconciliation succeeded

current_state
  = current operational condition
```

Therefore this is valid:

```text
initialized = true
state       = error
```

A later sync failure must not erase evidence that the profile was successfully initialized previously.

### 7.4 Do not use process lifetime as task state

Do not rely on:

```text
fork
nohup
setsid
shell background &
parent PID lifetime
terminal lifetime
```

as the correctness mechanism.

Those may be implementation details in unrelated launcher code, but they must not represent whether initial synchronization, retry, or deletion is pending.

---

## 8. Sync run model

If the implementation already has a run/history table, extend it rather than introducing a parallel abstraction.

A run represents **one execution attempt**, not the durable reconciliation queue.

Recommended conceptual shape:

```text
sync_runs
  id
  profile_id

  kind
    initial
    full
    incremental

  target_generation

  status
    running
    succeeded
    failed
    cancelled        // only if already supported/needed elsewhere

  phase
    scanning
    comparing
    uploading
    downloading
    deleting
    reconciling
    finalizing

  started_at
  completed_at

  files_discovered
  files_completed

  bytes_total
  bytes_completed

  warnings
  last_progress_at

  error_code
  error_classification
  error
```

If an existing schema already has `queued`, it may be retained for compatibility, but a queued run row must not be the authoritative durable signal that work exists. Durable intent comes from generation/control-plane state.

### 8.1 Target generation

Every run must capture the generation it promises to reconcile:

```text
run.target_generation = desired_generation at atomic claim time
```

If local events arrive while the run is active:

```text
run.target_generation = 10
desired_generation    = 11
```

then success of that run may advance only through generation `10`. It must not mark generation `11` successful merely because `desired_generation` changed while the run was executing.

On successful commit:

```text
last_success_generation = max(
    last_success_generation,
    run.target_generation
)
```

If debt remains afterward, another reconciliation stays eligible according to the normal gates.

### 8.2 Progress fields are operational only

Not all counters must be known at run start.

Unknown totals should be represented explicitly rather than fabricated.

Example:

```text
Files: 8421 completed
Bytes: 1.8 GB transferred
Total: calculating
```

Progress counters never determine correctness or readiness.

---

## 9. Worker behavior

### 9.1 Claim pending work

The worker should detect profiles requiring reconciliation from durable state.

Conceptually:

```text
for each active profile:
    if reconciliation_claimable(profile):
        run = claim_run(profile)
        perform_reconciliation(run)
```

Avoid making the worker dependent on an in-memory event emitted by `profile add`.

A wake signal may reduce latency, but durable state is authoritative.

### 9.2 Atomic claim boundary

There must be one authoritative claim path. Even though V1 runs a single worker process, do not scatter non-atomic `read -> decide -> write running state` logic through the code.

Conceptually:

```text
BEGIN

verify profile lifecycle allows work
verify reconciliation debt exists
verify retry/pause gates allow work
verify no active run exists

target_generation = desired_generation

insert sync_run(
    profile_id,
    target_generation,
    status = running,
    ...
)

set profile_sync_state.current_run_id
set operational state/phase

COMMIT
```

Only after this transaction owns the run should scanning/transfer begin.

This design is deliberate future-proofing: V1 may enforce one worker process, but the data/state model must not depend on “there can never be another worker.” If future capacity or HA requirements justify multiple workers, the claim boundary can be upgraded with owner/lease semantics without redesigning generation intent or reconciliation correctness.

### 9.3 Wake-up signal

After committing the profile and pending sync state, the CLI may:

```text
signal existing worker
poke local socket
notify daemon
touch/wake scheduler
```

depending on current implementation.

If that signal is lost, the worker must still discover the pending work on its next normal scheduling/recovery pass.

### 9.4 Single active run per profile

For V1:

> At most one reconciliation run may actively mutate remote state for a profile.

If another trigger occurs while the profile is already syncing:

```text
record newer desired generation
do not launch competing transfer
current reconciliation completes
worker checks whether another reconciliation is required
```

This prevents first-run sync from racing with normal file events.

### 9.5 V1 worker cardinality

V1 should enforce a single worker process/runtime owner because that is the smallest model compatible with this modification.

Treat this as a **runtime deployment policy**, not a permanent state-machine assumption. Code that mutates run ownership must still go through the atomic claim abstraction described above.

---

## 10. Progress reporting

### 10.1 Progress must be independent of `profile add`

Progress should be queryable after the creating process exits.

Primary interface:

```bash
knowledge-sync profile status vault
```

Example during the first-ever reconciliation:

```text
Profile: vault
Initialized: no
State:   initializing
Phase:   uploading

Source scan:      complete
Eligible files:   20,384
Completed files:  8,421
Transferred:      1.8 GB / 4.7 GB

Started:          12m ago
Last progress:    2s ago
Last error:       none
```

For a profile that has initialized previously but is reconciling later changes, the corresponding public state is `syncing`.

For retryable backoff:

```text
Profile: vault
Initialized: no
State: error
Retryable: yes
Next retry: 8m
Last error: temporary network failure
```

A one-shot `profile status` is a state query. If the profile lookup/query itself succeeds, it exits `0` even when the reported synchronization state is `error`.

### 10.2 Watch mode

Add:

```bash
knowledge-sync profile status vault --watch
```

This should continuously refresh or emit durable state/progress rather than depend on the creating CLI process.

Possible active output:

```text
Profile: vault
State: initializing
Phase: uploading
Files: 8421 / 20384
Bytes: 1.8 GB / 4.7 GB
Last progress: just now
```

Watch semantics:

```text
retryable failure / backoff
  -> continue observing

ready
  -> print final state and exit 0

terminal error
  -> print final state and exit nonzero

deleting/deleted
  -> exit nonzero

paused, only if pause already exists
  -> exit nonzero

Ctrl-C
  -> stop only this observer; never cancel worker-owned reconciliation
```

When complete:

```text
Profile: vault
State: ready
Last successful sync: just now
```

### 10.3 Watch implementation

Prefer one of:

1. read/poll local durable state at a modest interval;
2. subscribe to an existing local event mechanism if already available.

Do not make `--watch` required for correctness.

Polling local SQLite state is acceptable for V1 if simple and reliable.

---

## 11. Public state model

Keep user-facing states stable and simple.

Required V1 top-level states:

```text
initializing
syncing
ready
error
```

`paused` is conditional, not a new requirement for this modification:

- if pause/resume already exists, preserve `paused` and apply the semantics below;
- if it does not exist, do not add pause/resume solely for async initial sync.

Internal phases may be more detailed:

```text
queued
scanning
planning
uploading
downloading
deleting
reconciling
finalizing
```

Recommended mapping:

```text
not initialized + reconciliation pending/claimed
  -> initializing

initialized + reconciliation pending/claimed
  -> syncing

active transfer/reconciliation
  -> initializing or syncing according to initialized history

retryable failure waiting for next_retry_at
  -> error
     (with retryable=true and next_retry_at visible)

terminal failure
  -> error

successful run AND desired_generation <= last_success_generation
  -> ready
```

`initializing` is specifically the pre-first-success state. A profile that has already initialized but is queued for later reconciliation is `syncing`, not `initializing`.

Do not mark a profile `syncing` merely because `next_retry_at` has arrived. Until a run is actually claimed, the profile remains `error` with retry eligibility/due information.

Do not briefly transition to `ready` after a successful run if a newer desired generation is already pending. Success commit should atomically check remaining reconciliation debt.

If pause already exists:

```text
pause requested
  -> persist pause gate
  -> prevent new claims
  -> do not cancel an already active run solely for this feature
  -> after active run finishes, remain paused

resume
  -> clear only the pause gate
  -> do not clear terminal error
  -> retain future retry time
  -> if retry time already passed, work becomes claimable immediately
```

Do not expose implementation-specific rclone process states as the primary public state model.

---

## 12. `profile add` exit semantics

Default asynchronous mode:

```bash
knowledge-sync profile add ...
```

Exit `0` means:

> The profile was successfully validated and durably created, and initial reconciliation was durably scheduled.

It does **not** mean:

> The initial upload has already completed successfully.

Possible nonzero cases include:

```text
invalid profile configuration
invalid or unsafe source root
profile ID conflict
database transaction failure
unable to persist initial sync intent
```

Remote transfer failures occurring later belong to sync status, not to the already-finished `profile add` process.

---

## 13. `--wait` exit semantics

For:

```bash
knowledge-sync profile add ... --wait
```

`--wait` is an observer over durable profile readiness. It is not an owner of the run and must not invoke a separate sync implementation.

Exit `0` means:

> The profile was created and its initialization goal reached a committed successful state.

The waiter follows the readiness goal across retryable execution attempts:

```text
retryable run failure
  -> keep waiting while automatic retry remains eligible/scheduled

ready
  -> return 0

terminal error
  -> return nonzero

paused, if pause exists
  -> return nonzero because progress is intentionally blocked

deleting / deleted
  -> return nonzero

Ctrl-C / waiter process termination
  -> waiter exits only; worker reconciliation continues
```

Nonzero after profile creation must explicitly distinguish creation success from synchronization/readiness failure.

Example:

```text
Profile "vault" was created successfully.
Initial sync failed: remote quota exceeded.

The profile has been kept.
Inspect or retry with:
  knowledge-sync profile status vault
  knowledge-sync sync vault
```

Do not roll back profile creation solely because remote synchronization failed after the durable profile commit.

---

## 14. Explicit wait command

Add V1 support for:

```bash
knowledge-sync profile wait vault
```

This is the canonical machine-friendly readiness waiter used by:

- CI;
- shell scripts;
- integration tests;
- automation;
- `profile add --wait`.

Semantics are the same as Section 13:

```text
observe profile initialization/readiness goal
cross retryable attempts without failing early
return 0 on ready
return nonzero on terminal error
return nonzero on paused/deleting/deleted conditions
never cancel worker-owned reconciliation when the waiter is interrupted
```

`profile add --wait` should reuse this waiter implementation rather than implementing its own run-following behavior.

---

## 15. Readiness invariant

Do not mark a profile `ready` merely because:

```text
profile row exists
worker started
files began uploading
rclone process exited partially
one run succeeded while newer desired state remains pending
```

Two related predicates are required:

```text
initialized
  = at least one complete successful reconciliation has been durably committed

ready
  = initialized
    AND no unsatisfied desired generation remains
    AND no current operational error/active reconciliation prevents current convergence
```

`initialized` is sticky historical evidence. `ready` is current operational convergence.

Conceptually:

```text
profile persisted
        |
initial sync pending
        |
scan / transfer / reconciliation
        |
commit run.target_generation
        |
initialized = true
        |
remaining desired generation?
    yes -> initializing/syncing continues
    no  -> ready
```

This gives later subsystems a reliable predicate without conflating “has ever initialized” with “is currently error-free and caught up.”

---

## 16. Compiler integration consequence

The Knowledge Compiler remains local-only and independent from Google Drive transport.

This modification improves that boundary.

Recommended future precondition logic:

```text
profile exists?
  no -> fail

profile sync initialized?
  no -> warn or refuse depending on command semantics

compile
  -> read eligible local corpus
  -> publish .knowledge-derived locally

sync worker
  -> observes generated eligible files
  -> converges them to Drive
```

The compiler should not need to wait inside its process for Drive upload completion unless an explicit future command requests end-to-end remote confirmation.

This preserves:

```text
compiler = deterministic local publication
syncer    = remote availability
```

---

## 17. Crash and restart semantics

### 17.1 CLI exits immediately after commit

Scenario:

```text
profile add commits
CLI process exits
worker has not started yet
```

Expected:

```text
pending reconciliation remains visible in durable generation state
worker later discovers and executes it
```

### 17.2 Worker crashes during initial upload

A `sync_run` is one execution attempt. If the worker dies while a run is `running`, a new worker must not pretend to resume the same attempt record.

After the V1 singleton worker ownership is reacquired:

```text
old running run
  -> failed/orphaned
  -> error_code = worker_interrupted
  -> retryable

reconciliation debt remains
new run claims current desired generation
normal full reconciliation converges safely
```

`worker_interrupted` should be immediately retryable (`retry delay = 0`) and should not increment transport/network consecutive-failure backoff counters. A process restart is not evidence that the remote transport needs exponential delay.

The implementation does not need transfer-level resume semantics to satisfy correctness if a later full reconciliation safely converges.

### 17.3 Machine reboot

After restart:

```text
daemon/worker acquires singleton runtime ownership
loads durable profile state
marks inherited running attempts orphaned
re-evaluates reconciliation/retry/deletion gates
runs eligible reconciliation
```

No terminal from the original `profile add` invocation is required.

### 17.4 Lost wake signal

Expected:

```text
profile + desired generation already committed
worker eventually discovers work through normal scan/scheduler
```

Wake signals are optimization only.

### 17.5 Retry schedule survives restart

Retry timing is durable state. Restart must preserve the already-computed `next_retry_at` rather than silently resetting every transient failure to immediate retry.

The special `worker_interrupted` recovery above is the exception because the interruption itself is local process ownership loss, not a transport failure.

---

## 18. Retry behavior

Provide the explicit control-plane command:

```bash
knowledge-sync sync vault
```

or preserve the existing equivalent if the project already has one with compatible semantics.

This command means:

> Request reconciliation for this existing profile now.

It does **not** execute a competing reconciliation inside the CLI.

### 18.1 Structured error classification

The reconciliation/transport boundary should return a structured classification rather than requiring the worker to parse error strings.

Conceptually:

```text
Retryable
Terminal
```

with optional structured metadata:

```text
error_code
retry_after
human_message
```

Unknown/unclassified internal failures default to `Terminal` unless implementation evidence establishes that they are safely retryable.

### 18.2 Durable retry gate

For retryable errors, persist state equivalent to:

```text
retry_classification = retryable
consecutive_failures = N
next_retry_at         = timestamp
last_error_code
last_error
```

Use exponential backoff with a finite maximum interval and jitter. Exact constants should follow existing project conventions or be implementation constants; they do not need to become new user configuration.

Retryable errors do not become terminal merely because an arbitrary attempt-count limit was reached. Once the backoff reaches its cap, retry may continue at the capped interval until success, explicit pause (if supported), terminal reclassification, deletion, or another existing administrative control stops it.

### 18.3 Terminal error gate

A terminal error keeps desired-generation debt intact but prevents automatic claims.

Filesystem changes while terminally blocked may increase `desired_generation`; they do not clear the terminal gate.

A fresh explicit:

```bash
knowledge-sync sync vault
```

reopens eligibility and requests a new worker-owned attempt. It may also advance/refresh desired reconciliation intent according to existing generation conventions.

### 18.4 Manual sync while other conditions exist

```text
active run exists
  -> record/request newer desired state as needed
  -> do not create a competing run

retryable backoff exists
  -> explicit sync makes the next attempt eligible now

terminal error exists
  -> explicit sync clears/reopens the terminal retry gate for a fresh attempt

deleting
  -> reject/ignore new sync request; deletion lifecycle wins
```

### 18.5 Success cleanup

On successful reconciliation commit, clear current error/retry operational state:

```text
last_error = null
last_error_code = null
retry_classification = null
next_retry_at = null
consecutive_failures = 0
```

Historical failures remain available in `sync_runs` and structured logs. Profile-level state should describe current operational truth, not serve as the audit history.

---

## 19. Deletion interaction

Deletion intent must be durable.

For V1, if a user removes a profile while synchronization is active, **serialize deletion behind the active run** rather than adding transfer cancellation solely for this feature.

Recommended lifecycle:

```text
profile remove
  -> atomically persist lifecycle = deleting (or equivalent deletion_requested_at)
  -> prohibit new reconciliation claims
  -> return success once deletion intent is durable

if active run exists
  -> allow that owned run to finish naturally
  -> do not claim a follow-up generation/retry

when no active run remains
  -> finalize profile deletion using existing profile-delete semantics
```

Deletion lifecycle has higher priority than:

```text
desired generation debt
automatic retry due
manual sync request
pause/resume execution state
```

Do not allow:

```text
profile control-plane state fully deleted
old worker continues mutating remote indefinitely
```

### 19.1 `profile remove` exit semantics

If deletion must wait for an active run, `knowledge-sync profile remove <profile>` should still be asynchronous rather than blocking for the entire transfer duration.

Exit `0` means:

> The deletion request was durably accepted, the profile cannot claim new reconciliation work, and deletion will finalize after current execution ownership is released.

It does not necessarily mean every profile row has already been physically removed.

Example output may state:

```text
Profile "vault" deletion requested.
An active sync is finishing before removal.
```

Do not add a new delete `--wait` mode solely for this modification unless the existing CLI/API already requires equivalent blocking semantics.

---

## 20. File event interaction during initial sync

Local changes may occur while the first full reconciliation is running.

Do not start independent competing sync executions.

Required rule:

```text
run claimed at target_generation = N
        |
fs events occur
        |
desired_generation becomes N+1 (or later)
        |
current run continues against its captured target_generation
        |
current run succeeds
        |
commit success only through run.target_generation
        |
if desired_generation remains newer
  keep state initializing/syncing
  leave follow-up reconciliation eligible
```

This follows the principle:

> Event-driven sync is an optimization; full reconciliation remains the correctness mechanism.

A newer filesystem event must not expand the meaning of an already-running attempt, and the system must not momentarily report `ready` while durable generation debt is already known.

---

## 21. Progress data quality

Progress metrics should be best-effort operational information, not correctness state.

Correctness must depend on reconciliation completion, not counters.

Acceptable fields:

```text
files discovered
files compared
files transferred
bytes transferred
current phase
last activity time
```

If rclone only exposes a subset reliably, expose that subset.

Do not invent totals.

Example:

```text
Phase: uploading
Transferred files: 8421
Transferred bytes: 1.8 GB
Total files: unknown
```

is preferable to an inaccurate percentage.

---

## 22. CLI examples

### 22.1 Default add

```bash
$ knowledge-sync profile add vault ~/Vault

Profile "vault" created.
Initial sync queued.

Check progress:
  knowledge-sync profile status vault
```

### 22.2 Status during first sync

```bash
$ knowledge-sync profile status vault

Profile: vault
Initialized: no
State: initializing
Phase: uploading

Eligible files: 20,384
Completed:      8,421
Transferred:    1.8 GB / 4.7 GB

Started:        12m ago
Last progress:  2s ago
```

### 22.3 Watch

```bash
$ knowledge-sync profile status vault --watch
```

The watch remains active across retryable failures/backoff and exits on ready or a meaningful blocking terminal condition.

### 22.4 Ready

```bash
$ knowledge-sync profile status vault

Profile: vault
Initialized: yes
State: ready
Initialized at: 2026-08-30T00:31:22+08:00
Last successful sync: 2026-08-30T00:31:22+08:00
```

### 22.5 Add and wait

```bash
$ knowledge-sync profile add vault ~/Vault --wait

Profile "vault" created.
Initial sync queued.

[worker-owned progress observation]

Initial sync complete.
Profile state: ready
```

If the waiter is interrupted, the profile and worker-owned reconciliation continue.

### 22.6 Retryable failure after async add

```bash
$ knowledge-sync profile status vault

Profile: vault
Initialized: no
State: error
Retryable: yes
Next retry: 8m

Last error:
  temporary network failure

Profile is still configured.
Automatic retry remains scheduled.
```

### 22.7 Terminal failure and explicit retry

```bash
$ knowledge-sync profile status vault

Profile: vault
State: error
Retryable: no
Last error:
  authentication failed
```

After fixing the underlying cause:

```bash
$ knowledge-sync sync vault

Reconciliation requested for "vault".
```

The command changes durable control-plane eligibility; the worker still owns execution.

### 22.8 Explicit wait

```bash
$ knowledge-sync profile wait vault
```

This observes the readiness goal across retryable attempts and returns nonzero on terminal blocking conditions.

### 22.9 Remove while sync is active

```bash
$ knowledge-sync profile remove vault

Profile "vault" deletion requested.
An active sync is finishing before removal.
```

Successful return means deletion intent is durable and no new reconciliation may be claimed; final row removal may occur after active execution ownership ends.

---

## 23. Database migration strategy

Prefer the smallest additive migration compatible with the existing implementation.

### Option A — existing run/state schema is already sufficient

If existing tables already represent equivalent concepts, extend them rather than creating a parallel state system.

Likely missing concepts to inspect for include:

```text
initialized_at / durable successful-initialization evidence
desired_generation
last_success_generation
current_run_id
current_phase
target_generation on run attempts
retry classification / next_retry_at / consecutive failures
last error code/message
durable deletion intent
progress counters
```

### Option B — introduce a small profile sync state table

If current profile rows should remain configuration-only, an additive conceptual shape is:

```sql
profile_sync_state (
    profile_id TEXT PRIMARY KEY,
    desired_generation INTEGER NOT NULL,
    last_success_generation INTEGER,
    initialized_at TEXT,
    last_success_at TEXT,
    current_run_id TEXT,
    state TEXT NOT NULL,
    phase TEXT,
    retry_classification TEXT,
    consecutive_failures INTEGER NOT NULL DEFAULT 0,
    next_retry_at TEXT,
    last_progress_at TEXT,
    last_error_code TEXT,
    last_error TEXT
)
```

Durable deletion lifecycle may live on `profiles` or an existing lifecycle table rather than this sync-state table.

Exact SQL types and placement should follow current project conventions.

Avoid a large generic job framework solely for this feature.

### 23.1 Implementation-time migration fact check

The plan cannot determine from the currently supplied materials whether the pre-migration implementation has trustworthy durable evidence that an existing profile completed a successful reconciliation.

The implementation agent must inspect the existing schema, migrations, run/history state, and reconciliation success commit path and apply this rule:

```text
if trustworthy durable successful-reconciliation evidence exists:
    backfill initialized/last-success state from that evidence

else:
    do not infer readiness from profile existence
    mark initialization as unproven
    create/advance durable reconciliation intent so the profile can establish evidence
```

If available evidence is ambiguous and choosing a branch would change externally visible readiness semantics, stop and ask the user rather than guessing.

This is an implementation fact check, not an invitation to redesign the settled readiness semantics.

---

## 24. Transaction boundaries

### 24.1 Profile creation

Profile creation and durable initial-sync intent should succeed atomically from the user's perspective.

Preferred transaction:

```text
BEGIN

insert profile
insert/init profile_sync_state
advance/initialize desired_generation to require reconciliation

COMMIT
```

Do **not** require insertion of a queued `sync_run` as part of durable scheduling. Generation/control-plane state is the queue authority; a run is created when the worker atomically claims an execution attempt.

Only after commit:

```text
wake worker
print success
return 0
```

If persistence of reconciliation intent fails, `profile add` should not report success.

### 24.2 Run success

Run completion must atomically bind success to the run's captured target:

```text
BEGIN

verify run still owns current_run_id as required by implementation
mark run succeeded
advance last_success_generation through run.target_generation
set initialized_at if this is the first successful complete reconciliation
update last_success_at
clear current error/retry state
clear current_run_id

if desired_generation <= last_success_generation:
    state = ready
else:
    state = initializing/syncing according to initialized history

COMMIT
```

Never copy the current `desired_generation` into `last_success_generation` unless it is exactly the generation proven by the completed run.

### 24.3 Deletion request

A deletion request must durably close future claim eligibility before `profile remove` reports success:

```text
BEGIN
mark lifecycle deleting / deletion_requested_at
COMMIT
```

After that point, scheduler/retry/manual-sync paths must not create new runs for the profile.

---

## 25. Worker claim safety

Run claiming must be idempotent and atomic even though V1 enforces one worker process.

The worker should use one authoritative claim operation that verifies and commits, in a single transaction, at least:

```text
profile lifecycle permits work
reconciliation debt exists
retry/pause gates permit work
no active run exists
capture target_generation = desired_generation
create running attempt
bind current_run_id
```

The same profile must not accidentally launch two simultaneous reconciliations because of duplicate wakeups, scheduler passes, or future changes to worker topology.

### 25.1 Singleton is a V1 runtime policy

V1 should enforce a single worker runtime owner if the current project does not already have a stronger ownership mechanism.

Do not use that fact to justify non-atomic state transitions. The core invariant is **one active run per profile**, expressed through the claim abstraction.

If a future version requires multiple workers for throughput or high availability, evolve the claim implementation with durable owner/lease/expiry semantics while preserving:

```text
generation intent
target_generation per attempt
single active run per profile
full reconciliation as correctness authority
```

This keeps the current V1 decision from becoming a hard-to-remove architectural dependency.

---

## 26. Logging

Keep structured logs for:

```text
profile created
initial sync queued
worker claimed run
phase changed
progress checkpoint
run succeeded
run failed
retry scheduled/requested
profile became ready
```

Logs should include:

```text
profile_id
run_id
generation where applicable
```

Do not require logs for user-facing progress; status should come from operational state.

---

## 27. Tests

### 27.1 `profile add` returns before transfer completion

Use a controlled fake/slow transport.

Verify:

```text
profile add returns
remote transfer is still blocked/running
profile exists
desired generation debt exists
```

### 27.2 durable pending work survives CLI exit

Create profile, terminate CLI, then start/continue worker.

Verify the worker performs the initial reconciliation without any parent CLI process.

### 27.3 worker restart creates a new attempt

Inject worker termination during initial sync.

Restart worker.

Verify:

```text
profile is retained
old running attempt becomes failed/orphaned with worker_interrupted
worker_interrupted does not increment transport failure backoff
new run id is created
safe full reconciliation executes
eventual ready state is possible
```

### 27.4 lost wake-up recovery

Suppress the immediate worker notification.

Verify normal worker polling/startup discovers generation debt.

### 27.5 atomic target-generation capture

Block around claim and inject a filesystem event that advances `desired_generation`.

Verify the claimed run has one stable `target_generation` and cannot claim success for a later generation it did not reconcile.

### 27.6 file events during initial sync

While a run with `target_generation=N` is blocked, advance desired generation.

Verify:

```text
no competing run mutates the same profile simultaneously
run N may commit success only through N
new desired state remains pending
profile does not briefly report ready if debt remains
a follow-up reconciliation eventually converges
```

### 27.7 status during upload

Block transfer midway.

Verify status exposes:

```text
state = initializing for never-initialized profile
state = syncing for already-initialized profile
phase = uploading
last_progress_at
available counters
```

### 27.8 status after success

Verify:

```text
state = ready
initialized_at != null
last_success_at != null
last_error = null
last_error_code = null
retry state cleared
```

Historical failed attempts, if any, remain in run history/logs.

### 27.9 retryable failure

Inject a transport error classified as retryable.

Verify:

```text
state = error
retryable classification is durable
next_retry_at is durable
consecutive failure count advances as expected
status does not claim syncing merely because retry time becomes due
worker retries after eligibility time
```

Restart the worker during backoff and verify the retry schedule is preserved.

### 27.10 terminal failure gate

Inject a terminal/unknown failure.

Verify:

```text
profile still exists
state = error
automatic claim is blocked
desired generation debt remains
filesystem changes may advance desired_generation but do not clear the gate
```

Then run:

```bash
knowledge-sync sync <profile>
```

and verify it reopens eligibility without executing reconciliation in the CLI process.

### 27.11 automatic retry has no attempt-count terminal conversion

Drive repeated explicitly retryable failures through enough attempts to reach the configured backoff cap.

Verify the error remains retryable and attempts continue at the capped interval rather than becoming terminal solely because of count.

### 27.12 `profile add --wait`

Verify it:

```text
uses the canonical profile waiter
uses the same worker-owned reconciliation path
continues waiting across retryable attempt failures
returns success on ready
returns nonzero on terminal error
does not delete the profile on remote failure
Ctrl-C exits the waiter without cancelling reconciliation
```

### 27.13 `profile wait`

Verify:

```text
ready -> exit 0
retryable/backoff -> continue waiting
terminal error -> nonzero
deleting/deleted -> nonzero
paused -> nonzero, only if pause already exists
```

### 27.14 `profile status` exit semantics

Verify a successful status query exits `0` even when the reported sync state is `error`.

Verify `profile status --watch`:

```text
continues across retryable failures
ready -> exit 0
terminal error -> nonzero
deleting/deleted -> nonzero
paused -> nonzero, if pause exists
Ctrl-C does not affect worker execution
```

### 27.15 duplicate add

Existing duplicate-profile behavior should remain unchanged.

The async change must not make duplicate creation race-prone.

### 27.16 delete while initial sync is active

Verify:

```text
profile remove durably marks deleting and returns without waiting for long transfer completion
new reconciliation claims are forbidden
retry/manual sync cannot reactivate the profile
active run is allowed to finish naturally
no follow-up generation is claimed
delete finalizes after run ownership ends
restart preserves deletion intent
```

### 27.17 pause/resume compatibility, only if already implemented

Do not add pause solely for this test.

If the project already has pause/resume, verify:

```text
pause blocks new claims but does not cancel active run
resume clears only the pause gate
resume does not clear terminal error
future retry time is preserved
expired retry becomes immediately eligible after resume
```

### 27.18 migration compatibility

Existing profiles created before this migration must follow the implementation-time evidence rule:

```text
trustworthy durable successful reconciliation evidence
  -> backfill initialization

no trustworthy evidence
  -> do not infer ready
     schedule reconciliation to establish evidence
```

If evidence is ambiguous, implementation must escalate rather than silently mark ready.

---

## 28. Integration tests

Add an end-to-end fixture approximately matching:

```text
large local source
slow/fake remote
background worker
CLI process
SQLite state
```

Test sequence:

```text
1. start worker
2. run profile add
3. assert CLI exits quickly
4. assert profile persisted
5. assert desired generation debt exists
6. assert worker creates a run with captured target_generation
7. observe progress through status
8. mutate local source during the blocked run and advance desired_generation
9. release remote transfer
10. assert first run cannot satisfy the newer generation
11. observe follow-up reconciliation
12. assert ready only after all known generation debt is satisfied
13. assert remote contents converge
```

Also test:

```text
worker down during profile add
```

Expected:

```text
profile add still succeeds if durable intent commit succeeds
status shows initializing/queued semantics
starting worker later completes the sync
```

And test a restart/backoff sequence:

```text
retryable transport failure
retry schedule persisted
worker restart
retry schedule preserved
subsequent success clears current error gate and reaches ready
```

Finally test active deletion serialization across restart:

```text
active run
durable profile remove request
CLI returns
worker/machine restart if supported by fixture
no new run is claimed
delete finalizes after active ownership is resolved
```

---

## 29. Backward compatibility

The behavior change is user-visible.

Existing users may expect:

```text
profile add returns 0
=> remote initial upload already finished
```

After this change that assumption is no longer valid.

Mitigation:

1. document the new exit semantics;
2. add `--wait`;
3. update shell examples and integration tests;
4. ensure success output explicitly says `Initial sync queued`;
5. provide `profile status` / `profile wait` for scripts.

If strict CLI compatibility is required, a temporary release may support:

```text
--wait
```

while warning that asynchronous creation will become the default.

However, if this is still pre-stable/internal V1, prefer switching the default immediately rather than carrying transitional complexity.

---

## 30. Documentation updates

Update CLI documentation for:

```text
knowledge-sync profile add
knowledge-sync profile status
knowledge-sync profile status --watch
knowledge-sync profile wait
knowledge-sync profile remove
knowledge-sync sync
```

Document the distinction:

```text
profile created
!=
initial synchronization complete
```

Also document:

```text
A successful asynchronous `profile add` guarantees durable scheduling,
not remote completion.
```

Document waiter/query distinctions:

```text
profile status
  = query current durable state; query success can exit 0 even if state=error

profile status --watch
  = observe until ready or a meaningful blocking terminal condition

profile wait
  = machine-friendly readiness waiter
```

Document retry semantics:

```text
retryable error
  = durable automatic retry with backoff

terminal error
  = automatic retry blocked until explicit `knowledge-sync sync <profile>`
```

Document asynchronous deletion when an active run exists: successful `profile remove` means the durable deletion request has been accepted and new sync claims are blocked; final row removal may occur after current execution ownership finishes.

If pause/resume already exists, update its documentation with the confirmed gate semantics. Do not add pause/resume commands solely for this modification.

---

## 31. Implementation sequence

### Step 1 — inspect existing state/run/lifecycle evidence

Before migration design, inspect current schema, migrations, worker ownership, run history, pause/resume support, deletion semantics, and successful reconciliation commit path.

Apply the implementation-time decision rules in this plan rather than asking the user for facts available in the repository.

Escalate only when evidence is ambiguous and the branch would change externally visible semantics.

### Step 2 — formalize authoritative predicates

Implement one authoritative function/predicate for each:

```text
has_ever_initialized(profile)
reconciliation_debt(profile)
reconciliation_claimable(profile)
```

Ensure `reconciliation_claimable` includes lifecycle and retry gates rather than equating debt with immediate execution eligibility.

### Step 3 — durable initial-sync intent

Modify profile creation transaction so a new profile always records generation debt requiring reconciliation.

Success criteria:

- pending work survives process exit/restart;
- worker can discover it without CLI memory state;
- no queued run row is required as a second intent authority.

### Step 4 — run attempt model and atomic claim

Add/extend `target_generation` and route every claim through one atomic claim operation.

Success criteria:

- target generation is captured atomically;
- no two active runs for the same profile;
- V1 singleton worker does not leak into non-atomic correctness assumptions.

### Step 5 — worker restart/orphan recovery

On singleton ownership acquisition, identify inherited `running` attempts as orphaned/failed with `worker_interrupted`, preserve generation debt, and immediately re-evaluate claimability using a fresh attempt.

### Step 6 — structured retry gates

Add structured `Retryable` / `Terminal` classification at the reconciliation/transport boundary and durable:

```text
next_retry_at
consecutive_failures
last_error_code
last_error
```

Implement capped exponential backoff + jitter for retryable transport failures.

Ensure terminal errors are not reopened by filesystem events.

### Step 7 — make `profile add` asynchronous

Change default command lifetime:

```text
persist profile + generation intent
wake
return
```

Success criteria:

- command no longer waits for large Drive upload;
- output clearly communicates queued/initializing state.

### Step 8 — expose status progress and exit semantics

Add/extend:

```bash
knowledge-sync profile status <profile>
knowledge-sync profile status <profile> --watch
```

Expose at least:

```text
initialized/current state
phase
run timestamps
progress counters
retryability / next retry
last current error
```

A successful one-shot status query exits `0` even if reported sync state is `error`.

### Step 9 — canonical waiter

Implement:

```bash
knowledge-sync profile wait <profile>
```

Then implement:

```bash
knowledge-sync profile add ... --wait
```

by reusing the same readiness waiter.

The waiter crosses retryable attempts and never owns/cancels reconciliation.

### Step 10 — explicit sync/retry request

Implement or adapt:

```bash
knowledge-sync sync <profile>
```

as a control-plane request that can make retryable backoff eligible now or reopen a terminal gate, while leaving execution to the worker.

### Step 11 — durable asynchronous deletion

Persist deletion intent before returning success from `profile remove`.

Block all new claims once deleting, serialize final deletion behind any active run, and recover deletion intent after restart.

Do not introduce transfer cancellation solely for this feature.

### Step 12 — conditional pause compatibility

If pause/resume already exists, preserve it and enforce the settled gate semantics.

If it does not exist, skip this step and do not add a new V1 pause feature.

### Step 13 — crash/restart/failure tests

Add failure injection around:

```text
post-profile-commit
pre-worker-claim
post-claim / pre-execution
mid-transfer
pre-success-commit
retry backoff
active deletion
```

### Step 14 — update existing tests/docs

Tests that previously assumed `profile add` implies completed remote upload must explicitly use:

```text
--wait
```

or wait on readiness.

---

## 32. Acceptance checklist

This modification is complete when all are true:

1. `profile add` does not wait for first remote upload by default.
2. A successful `profile add` durably records both the profile and required reconciliation generation intent.
3. Generation/control-plane state is the sole durable reconciliation intent authority; `sync_runs` represent attempts/history.
4. Initial synchronization is executed by the normal worker/reconciliation path.
5. Closing the creating terminal does not cancel or lose pending initial sync.
6. Every run atomically captures a `target_generation` and cannot claim success for newer state it did not reconcile.
7. Filesystem events during a run advance desired state without creating competing remote mutation runs.
8. At most one active reconciliation exists per profile.
9. V1 may enforce one worker process, but claim correctness remains atomic and does not depend on a permanent singleton assumption.
10. Worker restart marks inherited running attempts orphaned/failed and safely converges via a fresh run.
11. `worker_interrupted` recovery is immediately retryable and does not pollute transport-failure backoff counts.
12. Immediate worker wake-up is optional for correctness.
13. `initialized` is sticky historical evidence separate from current operational `state`.
14. `profile status` distinguishes at least initializing/syncing/ready/error and exposes retry information when applicable.
15. A previously initialized profile with new pending work is `syncing`, not `initializing`.
16. `ready` is never reported while a known newer desired generation remains unsatisfied.
17. Transfer progress can be viewed independently from the original `profile add` process.
18. One-shot `profile status` exits `0` when the query succeeds even if the reported sync state is `error`.
19. `profile status --watch` follows retryable attempts and returns nonzero on terminal blocking conditions.
20. `profile wait` exists as the canonical machine-friendly readiness waiter.
21. `profile add --wait` reuses the canonical waiter and does not duplicate sync logic.
22. Interrupting any waiter/watch process does not cancel worker-owned reconciliation.
23. Retryability is structured at the reconciliation/transport boundary rather than inferred by parsing error text.
24. Retry/backoff timing and gates survive worker/machine restart.
25. Retryable failures use capped exponential backoff with jitter and do not become terminal solely because of attempt count.
26. Terminal errors block automatic retry and are not cleared by ordinary filesystem events.
27. `knowledge-sync sync <profile>` explicitly requests a fresh/accelerated reconciliation without executing it in the CLI.
28. Successful reconciliation clears current profile-level error/retry state while preserving run history.
29. A remote failure after creation does not delete the profile.
30. Profile deletion intent is durable and prevents all new claims once accepted.
31. Active deletion is serialized behind the current run; `profile remove` does not block for a long transfer merely to finalize row deletion.
32. Restart does not lose an accepted deletion request.
33. If pause/resume already exists, pause blocks new claims without cancelling active work and resume clears only the pause gate; if it does not exist, this modification does not add it.
34. Existing full reconciliation remains the correctness authority.
35. Compiler behavior remains local-only and does not acquire a Drive dependency.
36. Migration backfills initialization only from trustworthy durable success evidence; otherwise reconciliation is scheduled to establish evidence.
37. Ambiguous implementation-time evidence that changes externally visible semantics is escalated rather than guessed.
38. Existing passing sync tests remain green after their assumptions are updated for async creation.
39. New generation-race, retry, restart, lost-wake, failure, wait/watch, and deletion tests pass.

---

## 33. Recommended V1 decision

Adopt:

```text
profile add = async by default
profile add --wait = explicit blocking observation mode
profile wait = canonical readiness waiter
profile status = durable state query
profile status --watch = progress/readiness observation
knowledge-sync sync <profile> = explicit reconciliation/retry request

worker = sole owner of reconciliation execution
V1 runtime = one worker process
claim path = atomic and future multi-worker compatible

SQLite/control plane generation state = sole source of truth for pending reconciliation
sync_runs = execution attempts/history
each run = captured target_generation

retryable errors = durable automatic backoff/retry
terminal errors = durable automatic-retry gate

deleting = durable lifecycle gate with highest scheduling priority
profile remove = asynchronous when active execution must finish first
```

Conditionally adopt only if already present in the implementation:

```text
pause/resume
```

If present, pause prevents new claims without cancelling the active run, and resume clears only the pause gate. If absent, do not add it solely for this modification.

Do **not** adopt:

```text
profile add
  -> spawn upload subprocess
  -> detach subprocess
  -> hope process lifecycle preserves correctness
```

Do not introduce a second durable queue authority in `sync_runs`, and do not let V1's singleton worker justify non-atomic claim logic.

The intended change is not "background the upload command."

It is:

> Separate profile creation from synchronization execution, represent desired reconciliation durably, execute it through one worker-owned attempt model, and make readiness/progress/retry/deletion independently observable and crash-safe.

That gives the current implementation a cleaner operational model, leaves a clear path to future multi-worker leasing if capacity or HA ever requires it, and establishes the correct lifecycle boundary for later deterministic compiler integration.

---