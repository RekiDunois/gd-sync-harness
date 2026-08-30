# Harness plan: local knowledge sources → Google Drive direct API sync

## 1. Goal and invariants

Build a local macOS harness that mirrors one or more local knowledge sources to Google Drive through the Google Drive API, without Google Drive for desktop and without a mounted Drive filesystem.

Primary use case: make local Obsidian and other knowledge directories available through ChatGPT's Google Drive connection with low latency after local edits.

Core invariants:

1. The local source is always the source of truth.
2. Sync is one-way: local → Google Drive. Remote edits are never imported.
3. Do not use Google Drive for desktop.
4. Do not mount Google Drive as a filesystem.
5. Do not use `rclone bisync`.
6. Each local source is one named profile.
7. Profile source roots must not overlap or nest.
8. Profile remote mirror roots must not overlap or nest.
9. The harness creates and owns its managed Google Drive roots through rclone. Do not pre-create those roots manually when using `drive.file` scope.
10. Destructive reconciliation must fail closed when ownership metadata is missing, the destination is ambiguous, remote duplicates exist, or the delete budget is exceeded.
11. Real-time fast sync may create/update remote files, but must never delete remote files.
12. Full reconciliation is the only path that makes destructive decisions and establishes final mirror correctness.

## 2. High-level architecture

```text
Local sources
  ├── Obsidian Vault A
  ├── Research directory
  └── Project docs
          |
          | fswatch / macOS FSEvents
          v
  knowledge-sync (Go)
  persistent SQLite control plane
          |
          +-------------------------------+
          |                               |
          v                               v
  fast upsert path                 full reconciliation
  create/modify only               rename/delete/final truth
          |                               |
          +---------------+---------------+
                          |
                          | rclone / Google Drive API
                          v
              per-profile Drive Folder ID
                          |
                          v
                    Google Drive
                          |
                          v
                ChatGPT Drive access
```

Two sync paths deliberately coexist:

- **Fast path**: low-latency create/modify propagation for day-to-day use.
- **Full reconciliation**: slower but authoritative comparison used for deletes, renames, missed events, filter changes, large backlogs, manual deep sync, and periodic repair.

The fast path is an optimization. Full reconciliation remains the correctness authority.


### 2.1 Implementation stack and scope boundary

The harness is implemented as a compiled **Go** command-line/background-service binary named:

```text
knowledge-sync
```

V1 runtime stack:

```text
Go binary
  ├── CLI and profile lifecycle
  ├── SQLite control plane
  ├── persistent event/manifest state
  ├── safety/preflight policy
  ├── per-profile and per-remote scheduling
  ├── launchd installation/management
  ├── fswatch process supervision
  └── rclone process orchestration

external runtime dependencies
  ├── rclone   -> transfer/data plane + Google Drive API/OAuth
  └── fswatch  -> macOS FSEvents event source

macOS
  └── launchd  -> lifecycle and scheduled repair
```

The implementation must remain a **thin control plane around rclone**.

Do not reimplement in Go:

- Google Drive OAuth;
- Google Drive upload/download protocols;
- remote tree synchronization;
- hashing/checking already provided by rclone;
- Drive quota, dedupe, or backend-specific operations already exposed by rclone.

Use rclone as a subprocess/data-plane boundary and consume machine-readable output where available.

V1 also deliberately keeps `fswatch` as the FSEvents adapter rather than implementing a native CoreServices/FSEvents binding in Go. A native watcher may be considered later only if measured limitations in `fswatch` justify the additional platform-specific code.

SQLite is accessed directly by the Go process. The installed runtime must not depend on the `sqlite3` command-line tool. Prefer a build arrangement that preserves a self-contained macOS binary; the exact Go SQLite driver is an implementation choice unless profiling proves it material.

A Go toolchain is required only to build from source. Users installing a prebuilt binary do not need Go installed.

## 3. Multi-profile model

A profile is the unit of ownership, scheduling, filtering, state, locking, and remote mirroring.

Example:

```text
profile: obsidian-main
  type: obsidian
  source: ~/Obsidian/Main
  rclone remote: gdrive-main
  remote mirror: ChatGPT Knowledge/Obsidian

profile: research
  type: generic
  source: ~/Documents/Research
  rclone remote: gdrive-secondary
  remote mirror: ChatGPT Knowledge/Research
```

Profile IDs are explicit, stable machine identities, for example:

```text
obsidian-main
research
project-docs
```

Recommended ID grammar:

```text
[a-z0-9][a-z0-9-]*
```

Each profile also receives a permanent random `profile_uuid`.

The user-visible profile ID may be reused only after an explicit tombstone removal operation. `profile add` must not silently overwrite a previously removed profile.

## 4. Google account topology

### 4.1 Initial setup

It is valid to use the same Google account for both:

- rclone storage ownership, and
- the Google Drive account connected to ChatGPT.

Example:

```text
Local profile
    |
    v
Google Account A owns mirror
    |
    v
ChatGPT connected to Account A
```

### 4.2 Future storage expansion

If that account later runs low on storage, a profile may migrate to another Google account without changing the ChatGPT Hub account.

Example:

```text
Profile Research
    |
    v
Google Account B owns mirror
    |
    | Viewer share
    v
Google Account A (ChatGPT Hub)
    |
    v
ChatGPT
```

Rules:

- Storage owner is configurable per profile through its rclone remote.
- ChatGPT Hub identity is logically separate from storage ownership.
- When an external storage owner is used, share the relevant knowledge root to the Hub as **Viewer**, not Editor.
- Sharing is user-managed. The local harness does not continuously prove ChatGPT connector visibility.
- Optional ChatGPT-side probe tools may be used for diagnostics, but they are not a mandatory profile lifecycle gate.

## 5. rclone OAuth policy

### 5.1 Preferred scope

Prefer a custom Google OAuth Desktop client and the least-privilege `drive.file` scope.

With `drive.file`, the harness contract is:

> Managed mirror roots and harness metadata roots are created by this rclone OAuth app.

Do not rely on rclone discovering arbitrary manually pre-created folders when operating in this least-privilege mode.

If arbitrary pre-existing Drive content must be manipulated, that is a different deployment mode and may require full `drive` scope.

### 5.2 Credentials

The rclone config contains OAuth credentials and refresh tokens.

Discover the config path with:

```zsh
rclone config file
```

Requirements:

- user-readable only;
- never commit to Git;
- never copy OAuth secrets into the project directory;
- do not store OAuth tokens in the harness SQLite database;
- do not use a service account for the normal personal My Drive case unless there is a specific requirement.

## 6. Stable remote identity

Google Drive path names are display metadata, not sufficient identity.

After the harness creates a profile mirror root, record its stable Google Drive Folder ID.

Profile remote identity is therefore:

```text
rclone remote/account
+ Google Drive Folder ID
+ profile UUID sidecar marker
```

The human-readable path remains useful for display and diagnostics, but destructive operations must resolve and validate the stored Folder ID rather than trusting a path string alone.

This protects against Google Drive duplicate names and accidental path ambiguity.

## 7. Remote sidecar ownership metadata

Harness metadata must live outside the actual knowledge mirror root.

Example on each storage-owner Drive:

```text
.knowledge-sync/
  profiles/
    <profile_uuid>.json

ChatGPT Knowledge/
  Obsidian/
  Research/
```

The sidecar records at minimum:

```text
schema_version
profile_id
profile_uuid
remote_folder_id
```

Before any destructive reconciliation, validate:

1. sidecar exists;
2. `profile_uuid` matches the local profile;
3. `remote_folder_id` matches the profile's stored destination Folder ID;
4. the resolved remote is the expected rclone backend/account.

Never auto-recreate or bypass a missing marker during destructive reconciliation.

## 8. Local control plane: one SQLite database

Use one SQLite database for all profiles and runtime state, opened directly by the `knowledge-sync` Go process.

Suggested location:

```text
~/.local/share/knowledge-sync/knowledge-sync.sqlite
```

SQLite is the canonical source of truth for profile configuration and harness runtime state.

Do not maintain parallel canonical plist/env configuration files.

Suggested schema groups:

```text
profiles
  id
  profile_uuid
  type
  source_path
  remote_name
  remote_folder_id
  remote_display_path
  enabled
  max_delete
  max_file_size
  deleted_at
  ...

profile_excludes
  profile_id
  rule_type
  rule_value

pending_events
  profile_id
  path
  event_kind
  first_seen
  last_seen
  source_generation

profile_runtime
  profile_id
  source_generation
  reconcile_requested
  last_fast_success
  last_reconcile_success
  last_error
  watcher_status
  ...

remotes
  remote_name
  backend
  last_quota_check
  total_bytes
  used_bytes
  free_bytes
  quota_status
  ...
```

Use WAL mode and short database transactions. SQLite schema migrations are owned by the Go binary and run transactionally before normal operation.

Do not hold a SQLite transaction open while rclone performs network I/O.

Drive mutation concurrency is controlled separately by per-profile locks and a remote-level scheduler.

After profile/config mutations, create a consistent rolling SQLite backup. This is an implementation invariant, not an optional feature.

## 9. Profile lifecycle

Expected CLI shape:

```zsh
knowledge-sync profile add <id> <source> <remote> <remote-path> [options]
knowledge-sync profile show <id>
knowledge-sync profile list
knowledge-sync profile disable <id>
knowledge-sync profile enable <id>
knowledge-sync profile remove <id>
knowledge-sync profile restore <id>
knowledge-sync profile forget <id>
knowledge-sync profile migrate <id> <new-remote> <new-remote-path>
```

### 9.1 Add

`profile add` must:

1. validate profile ID syntax and uniqueness;
2. reject reuse of a tombstoned ID;
3. validate source directory exists;
4. for `type=obsidian`, require `<source>/.obsidian`;
5. reject local source overlap/nesting with any active profile;
6. reject remote mirror overlap/nesting with any active profile on the same storage owner;
7. validate rclone remote exists and uses the Google Drive backend;
8. create the managed remote root through rclone;
9. record stable Google Drive Folder ID;
10. create sidecar ownership metadata;
11. perform initial copy-mode rollout and verification;
12. install/start the profile's watcher and reconciliation jobs only after validation.

### 9.2 Disable

`profile disable`:

- stops watcher and reconciliation jobs;
- keeps SQLite config/state;
- keeps remote data untouched.

### 9.3 Remove

`profile remove`:

- stops and removes launchd jobs;
- clears pending runtime work;
- tombstones the profile row;
- keeps remote data untouched.

Remote deletion is never implied by profile removal.

### 9.4 Restore and forget

`profile restore` explicitly reactivates a tombstoned profile after validation.

`profile forget` permanently removes the tombstone. Only after that may a new `profile add` reuse the same human-readable profile ID and receive a new UUID.

### 9.5 Remote purge

Remote deletion is a separate explicit destructive operation, for example:

```zsh
knowledge-sync purge-remote <profile>
```

It must validate the sidecar UUID and Folder ID before moving the remote root to Trash.

## 10. Source types and filter contract

Supported source types:

```text
obsidian
generic
```

### 10.1 Structured filters only for real-time profiles

Do not make normal fast-path profiles depend on arbitrary raw rclone filter syntax.

The harness owns one structured filter model and applies the same semantics to:

- watcher eligibility;
- local manifest scans;
- fast-path file lists;
- full rclone reconciliation.

Supported rule categories should include at least:

```text
exclude path prefix
exclude directory name
exclude exact filename
exclude extension
maximum file size
```

This avoids a correctness split where `--files-from` ignores rclone filter flags in the fast path.

If an advanced raw-rclone-filter mode is ever added, it must disable the fast path unless semantic equivalence can be proven.

### 10.2 Obsidian defaults

Obsidian profiles should default to excluding configuration/system/private/noisy content such as:

```text
.obsidian/
.git/
.trash/
Private/
.DS_Store
*.tmp
*.swp
*.lock
*.mp4
*.mov
```

Do not exclude the entire `Attachments/` directory by default.

Knowledge attachments such as PDF, text, images, and office documents may be mirrored when they satisfy profile rules.

Default Obsidian maximum file size:

```text
512 MiB
```

The value is profile-configurable and may be disabled deliberately.

Oversize files remain local and are reported as skipped.

### 10.3 Generic defaults

Generic profiles must not inherit Obsidian-specific assumptions such as `.obsidian/`, `Private/`, or `Attachments/` semantics.

Use only minimal obvious temporary/system exclusions by default. Additional exclusions are explicit profile configuration.

### 10.4 Symlinks

Do not follow symlinks.

Report skipped symlinks in status/reconciliation output.

If the linked target should be mirrored, add it as its own non-overlapping profile rather than silently widening another profile's source boundary.

## 11. File-event watcher

Install `fswatch` alongside rclone:

```zsh
brew install rclone fswatch
```

Use macOS FSEvents through `fswatch` for recursive local change detection.

The watcher is not the correctness authority; it is a latency optimization.

For each active profile, watcher events are first persisted to SQLite before being considered handled.

Relevant events update:

```text
pending_events
source_generation
reconcile_requested
```

Events that indicate dropped/coalesced/uncertain FSEvents state must request a full reconciliation rather than guessing individual file changes.

## 12. Fast path: low-latency non-destructive upsert

The fast path handles only files that currently exist locally and are eligible under the structured profile filters.

Typical flow:

```text
file create/modify
    |
    v
fswatch event
    |
    v
persist event in SQLite
    |
    v
per-profile debounce
    |
    v
build changed-files list
    |
    v
rclone copy using files-from/no-traverse style targeted upload
    |
    v
confirm success
    |
    v
clear corresponding pending entries
```

Initial event batching defaults:

```text
FAST_SETTLE_SECONDS=3
FAST_MAX_DELAY_SECONDS=30
```

Meaning:

- normally wait for about 3 seconds of quiet time;
- if edits continue continuously, do not postpone forever;
- force a batch by about 30 seconds after the first pending event.

The fast path must never perform remote deletion.

Excluded events must not start a sync batch.

## 13. Backlog promotion

Targeted no-traverse uploads are appropriate for small changed-file sets, not arbitrarily large backlogs.

If the pending queue becomes large, promote the profile to full reconciliation instead of issuing thousands of per-file remote operations.

Initial policy may use both absolute and relative thresholds, for example:

```text
pending files > 500
OR
pending files > 5% of known manifest
→ request full reconciliation
```

These are tuning defaults, not correctness constants. Benchmark with real 20k+ profiles and adjust without changing the architecture.

Any destructive or uncertain event also requests full reconciliation regardless of backlog size.

## 14. Destructive-event reconciliation trigger

Delete, rename, directory move, filter change, or uncertain watcher state should not wait until the hourly safety reconciliation.

Use a separate destructive debounce:

```text
RECONCILE_SETTLE_SECONDS=10
RECONCILE_MAX_DELAY_SECONDS=60
```

Conceptually:

```text
delete/rename detected
    |
    v
reconcile_requested=1 persisted
    |
    v
wait for burst to settle
    |
    v
full reconciliation
```

A large directory reorganization should therefore collapse into one reconciliation rather than triggering hundreds of independent full scans.

## 15. Full reconciliation

Full reconciliation establishes final mirror correctness.

Use it for:

- destructive events;
- large fast-path backlog;
- uncertain/dropped watcher events;
- profile filter changes;
- `reconcile-now`;
- hourly safety repair;
- initial mirror-mode validation;
- post-migration validation.

Recommended Google Drive reconciliation options include:

```text
--fast-list
--track-renames
```

The exact final command must be derived from the structured filter set and safety checks.

### 15.1 Stable-generation preflight

The harness does not promise database-transaction-level snapshots of an actively edited filesystem.

Instead use practical strong safety:

1. wait for the source to settle;
2. record the current source generation;
3. perform dry-run/preflight;
4. if any relevant local change occurs during preflight, discard the result and restart after settling;
5. if preflight is accepted, start live reconciliation;
6. keep rclone's deletion limiter enabled as a second guard;
7. if local changes occur during live sync, mark the profile dirty and immediately schedule another reconciliation afterward.

This ensures the live sync is not started based on a preflight already known to be stale.

## 16. Delete budget

Default per-profile destructive deletion budget:

```text
MAX_DELETE=100
```

Before starting live destructive reconciliation:

1. run a dry-run/preflight against a stable source generation;
2. calculate expected destructive removals;
3. if expected removals exceed `MAX_DELETE`, do not start live reconciliation;
4. report the exact count and require explicit operator action.

Keep rclone's own `--max-delete`-style guard enabled during live sync as a second layer against concurrent changes.

For legitimate large cleanup, use a one-shot override rather than permanently widening the profile's normal safety threshold:

```zsh
knowledge-sync reconcile-now <profile> --allow-deletes <N>
```

The override applies only to that reconciliation.

## 17. Excludes define the final mirror

A profile exclusion means the excluded content should not remain visible in the remote knowledge mirror.

Therefore, if a previously mirrored file becomes excluded, the next full reconciliation should remove it from the remote mirror, subject to:

- stable-generation preflight;
- ownership marker validation;
- deletion-budget enforcement.

Do not keep the old plan's semantics where excluded remote objects remain forever simply because `--delete-excluded` was omitted.

Profile filter changes are potentially destructive and must cross an explicit reconciliation boundary.

Normal configuration changes should be made through CLI operations such as:

```zsh
knowledge-sync profile exclude-add <profile> <rule>
knowledge-sync profile exclude-remove <profile> <rule>
```

If the SQLite database is modified outside supported CLI operations, the harness should fail closed until configuration is explicitly revalidated rather than silently performing destructive changes.

## 18. `sync-now` versus `reconcile-now`

These commands intentionally have different performance and correctness scopes.

### 18.1 `sync-now`

```zsh
knowledge-sync sync-now <profile>
```

Goal: low-latency freshness for the common workflow:

> save a new/edited local file, then immediately ask ChatGPT about it.

Because `fswatch` does not expose a cross-process FSEvents flush barrier, `sync-now` must not trust the event queue alone.

Before returning success:

1. drain already-persisted pending work;
2. perform a **local-only manifest scan** of the profile source;
3. compare local path/size/mtime/identity metadata to the SQLite manifest;
4. detect local creates/changes that may not yet have reached the watcher queue;
5. fast-upsert those files;
6. if the local-only scan discovers delete/rename/uncertain state, automatically upgrade to full reconciliation;
7. wait for the required Drive writes to succeed.

`sync-now` must not scan the entire Google Drive destination merely to establish its fast barrier.

### 18.2 `reconcile-now`

```zsh
knowledge-sync reconcile-now <profile>
```

Always performs a full authoritative reconciliation with preflight, rename tracking, ownership validation, and delete-budget enforcement.

## 19. launchd architecture

Each active profile has two independent launchd jobs:

```text
com.local.knowledge-sync.<profile>.watch
com.local.knowledge-sync.<profile>.reconcile
```

### 19.1 Watch job

Long-running LaunchAgent invoking the same installed binary, conceptually:

```zsh
knowledge-sync watch <profile>
```

The Go process supervises that profile's `fswatch` child process and performs event persistence/batching. Responsibilities:

- owns the `fswatch` process for that profile;
- persists events;
- batches fast upserts;
- schedules destructive reconciliation requests.

launchd provides singleton service lifecycle for the watcher job.

### 19.2 Reconciliation job

Independent periodic LaunchAgent invoking the same installed binary, conceptually:

```zsh
knowledge-sync reconcile-scheduled <profile>
```

This entrypoint uses the same internal reconciliation implementation as manual `reconcile-now`, with normal profile deletion limits and no interactive prompts.

Independent periodic safety job.

Use `StartCalendarInterval`, not the old 15-minute `StartInterval` design.

Default cadence:

```text
once per hour
```

Prefer a non-round minute to avoid needless alignment with other scheduled jobs.

Calendar-based scheduling is selected so missed schedule points during sleep can coalesce into a wake-time catch-up run.

The hourly job is a repair mechanism, not the normal low-latency sync path.

## 20. Locking and concurrency

### 20.1 Per-profile writer lock

All mutation paths for the same profile share one PID-aware lock.

The lock protects cross-entrypoint concurrency, for example:

```text
watcher fast-upsert
manual sync-now
manual reconcile-now
periodic reconciliation
migration operation
```

If the lock owner PID no longer exists and the lock is stale, recover it safely.

Do not rely on a lock directory that can permanently wedge after reboot or `SIGKILL`.

### 20.2 Remote-level scheduler

Different profiles may share the same Google Drive account/rclone remote.

Do not let every profile independently consume Drive API concurrency without coordination.

Create a remote-level scheduler with:

- bounded concurrent rclone operations per remote;
- high priority for small fast upserts;
- lower priority for long full reconciliations;
- conservative per-process request pacing so aggregate activity remains within Google Drive API limits.

Initial concurrency may be small, for example two active operations per remote, but the exact number should be benchmarked.

Different independent storage-owner remotes may run concurrently.

## 21. Initial rollout for a profile

### Phase A — prerequisites

The installed harness runtime requires only the `knowledge-sync` binary plus the external `rclone` and `fswatch` executables.

Validate through a harness diagnostic command, conceptually:

```zsh
knowledge-sync doctor
```

At minimum it checks:

```zsh
command -v rclone
command -v fswatch
```

Homebrew is the recommended macOS installation source for missing external dependencies:

```zsh
brew install rclone fswatch
```

Homebrew itself is not a runtime invariant if compatible `rclone` and `fswatch` binaries already exist.

The `sqlite3` CLI is not required; SQLite access is embedded in the Go control plane.

Do not install Google Drive for desktop.

### Phase B — configure rclone remote

Create or validate a Google Drive remote with a custom OAuth Desktop client.

For the normal least-privilege path, select `drive.file`.

Validate:

```zsh
rclone listremotes
rclone backend features <remote>:
```

The harness must record and use the explicit rclone config path because launchd does not inherit an interactive shell environment reliably.

Every rclone invocation made by the harness must use the intended config rather than accidentally falling back to another default config location.

### Phase C — add profile

Example conceptual command:

```zsh
knowledge-sync profile add \
  obsidian-main \
  "/ABSOLUTE/PATH/TO/VAULT" \
  gdrive-main \
  "ChatGPT Knowledge/Obsidian" \
  --type obsidian
```

The harness creates the remote tree through rclone, captures the Folder ID, writes sidecar ownership metadata, and starts in non-destructive rollout mode.

### Phase D — initial copy

Initial rollout uses copy semantics:

```text
local create → remote create
local edit   → remote update
remote-only  → preserved
```

Creating the remote tree and sidecar marker is an explicit initialization write. Do not describe this phase as a completely zero-write dry run.

### Phase E — initial verification

Confirm at minimum:

- expected Markdown notes appear;
- eligible attachments appear;
- `.obsidian/` does not appear;
- `.git/` does not appear;
- configured excluded/private paths do not appear;
- symlinks are not followed;
- sidecar metadata exists outside the mirror root;
- stored Folder ID resolves to the expected remote root;
- no remote duplicate-object condition is present.

### Phase F — enable destructive mirror mode

Before allowing normal full reconciliation:

1. run authoritative dry-run/preflight;
2. inspect expected deletions;
3. verify deletion count is within budget;
4. enable normal reconciliation jobs.

## 22. Migration to another Google storage owner

Do not migrate by transferring folder ownership and assuming all descendants change owner.

Use copy → verify → cutover.

Example:

```text
old remote/account A
        |
        | local remains source of truth
        v
new remote/account B
```

Migration procedure:

1. configure/validate the new rclone remote;
2. create a new managed mirror root on the new account through rclone;
3. record the new stable Folder ID;
4. create the new profile sidecar marker;
5. `rclone copy` local source to the new destination;
6. require copy success;
7. run mandatory `rclone check --one-way --size-only` or equivalent verification;
8. optionally run a full hash audit for deeper verification;
9. atomically update the profile's remote binding in SQLite;
10. perform one successful sync/reconciliation against the new destination;
11. move the old remote root to Google Drive Trash;
12. do not empty Trash automatically.

If the new owner account differs from the ChatGPT Hub, the user is responsible for sharing the knowledge root to the Hub as Viewer.

The harness does not block migration completion on a ChatGPT-side connector probe.

## 23. Remote duplicate handling

Google Drive can contain duplicate objects with the same apparent name/path.

If rclone detects destination duplicates or the harness cannot resolve the expected unique destination identity:

```text
REMOTE_DUPLICATES
→ fail closed
```

Pause destructive reconciliation for that profile.

Do not automatically run destructive `rclone dedupe` in the background.

Provide an explicit repair flow, for example:

```zsh
knowledge-sync repair-duplicates <profile>
```

The repair process should first list/plan duplicates, compare against local source-of-truth, and apply normal deletion-budget protections before deleting redundant objects.

## 24. Quota monitoring

Google Drive capacity belongs to the storage-owner remote, not to an individual profile.

Track quota per rclone remote.

Example status:

```text
gdrive-main
  Used: 13.8 GiB
  Free: 1.2 GiB
  Status: QUOTA_LOW
```

Default behavior:

- warn when free space drops below a configurable threshold, initially around 2 GiB;
- never automatically create another account;
- never automatically share content;
- never automatically migrate profiles.

The warning exists to give the user time to initiate an explicit profile migration.

## 25. Health model

Separate freshness from mechanism failure.

Useful per-profile states include:

```text
HEALTHY
PENDING_FAST_SYNC
RECONCILE_REQUESTED
STALE
BROKEN
CONFIG_CHANGED_UNVALIDATED
REMOTE_DUPLICATES
QUOTA_LOW
DISABLED
TOMBSTONED
```

Do not treat laptop sleep itself as a sync failure.

Health checks should consider:

1. watcher launchd job exists and is running/restartable;
2. hourly reconciliation launchd job exists;
3. latest fast/reconcile success timestamps;
4. pending queue age;
5. recent rclone authentication errors;
6. sidecar marker presence and UUID/Folder ID match;
7. remote duplicate-object status;
8. quota status;
9. stale PID locks;
10. SQLite integrity/backup status.

A machine that was asleep may be **STALE** immediately after wake without being **BROKEN**. A catch-up run should restore freshness.

## 26. ChatGPT-facing use

The harness guarantees the local → Google Drive side of the pipeline.

For the common immediate-use workflow:

```text
save local file
→ automatic fast upsert in a few seconds
→ or run knowledge-sync sync-now <profile>
→ file is confirmed on Google Drive
→ ask ChatGPT
```

ChatGPT/Google Drive connector visibility is an external access layer, not something the local daemon continuously certifies.

If diagnostics are needed, create a unique probe file and use ChatGPT's connected Google Drive search/read path to verify that the Hub can see and read it. This is a targeted access test, not a requirement to read every mirrored file.

Recommended Obsidian root note:

```text
AI_CONTEXT.md
```

It can document:

- Vault folder meanings;
- authoritative areas;
- wikilink conventions;
- important MOCs/index notes;
- treatment of Daily/Inbox notes.

## 27. Operations

Expected operational commands:

```zsh
knowledge-sync status
knowledge-sync status <profile>
knowledge-sync sync-now <profile>
knowledge-sync reconcile-now <profile>
knowledge-sync reconcile-now <profile> --allow-deletes <N>
knowledge-sync verify <profile> --full
knowledge-sync profile list
knowledge-sync profile show <profile>
knowledge-sync profile disable <profile>
knowledge-sync profile enable <profile>
knowledge-sync profile migrate <profile> <remote> <path>
knowledge-sync repair-duplicates <profile>
```

`verify --full` may perform a complete hash-level audit and is intentionally more expensive than normal migration verification.

## 28. Rollback and emergency stop

### 28.1 Stop one profile

Disable that profile's watcher and reconciliation LaunchAgents.

Do not alter remote data.

### 28.2 Stop all writes

Provide a harness-level stop command that unloads all active watcher/reconciliation jobs.

### 28.3 Preserve uploads but stop remote deletion

Disable full destructive reconciliation while leaving non-destructive fast upserts disabled or explicitly controlled according to incident needs.

Do not implement rollback by editing an executable environment file; canonical state is in SQLite and changes go through supported CLI operations.

### 28.4 OAuth revocation

Uninstalling or disabling the harness must not automatically revoke Google OAuth or delete Drive data.

OAuth revocation is a separate deliberate user action.

## 29. Security and failure policy

Fail closed for destructive operations when any of the following is true:

- profile sidecar marker missing;
- profile UUID mismatch;
- Folder ID mismatch;
- wrong/unexpected rclone backend;
- destination duplicates detected;
- source generation changed during preflight;
- delete count exceeds budget;
- configuration changed outside validated control flow;
- SQLite profile binding is unavailable/corrupt.

Do not invent automatic recovery that weakens these ownership checks.

Safe automatic recovery is appropriate for:

- stale PID locks whose owner no longer exists;
- watcher restart;
- persisted pending-event replay;
- hourly catch-up reconciliation;
- promotion from large fast backlog to full reconciliation.

## 30. Performance target for 20k+ files

The architecture must remain usable for profiles with at least tens of thousands of files.

Therefore:

- ordinary create/modify events must not trigger two complete remote traversals;
- fast upserts operate on changed paths only;
- `sync-now` may scan local metadata, but should not scan the entire remote unless it upgrades to full reconciliation;
- full reconciliation uses Google Drive-appropriate listing optimizations such as `--fast-list`;
- large pending queues promote to full reconciliation;
- migration's mandatory verification uses size-only one-way checking by default;
- full hash audit is explicit and optional;
- benchmark real profile sizes before tuning queue thresholds, rclone checker/transfer counts, and remote concurrency.

## 31. Go implementation and replacement scope

The existing single-profile shell implementation is not extended in place as a collection of ad-hoc environment variables or shell daemons.

### 31.1 One binary, multiple roles

Ship one compiled executable:

```text
knowledge-sync
```

The same binary provides both operator CLI commands and launchd service entrypoints.

Conceptual command surface:

```text
knowledge-sync doctor
knowledge-sync install
knowledge-sync uninstall
knowledge-sync status [profile]
knowledge-sync profile ...
knowledge-sync sync-now <profile>
knowledge-sync reconcile-now <profile>
knowledge-sync watch <profile>                 # launchd/internal service entrypoint
knowledge-sync reconcile-scheduled <profile>  # launchd/internal service entrypoint
```

Public commands and background entrypoints must call the same internal packages/state machine rather than duplicate synchronization logic.

### 31.2 External-process boundary

`rclone` and `fswatch` remain external executables supervised by Go.

The Go layer must:

- discover and persist their absolute executable paths so launchd does not depend on interactive-shell `PATH`;
- pass the explicit intended rclone config path on every rclone invocation;
- use `context` cancellation/timeouts and capture exit status/stdout/stderr;
- prefer rclone machine-readable/JSON output when available instead of parsing human prose;
- never shell-concatenate untrusted profile paths into commands; pass subprocess arguments directly;
- terminate/reap child processes cleanly during launchd stop/restart.

Do not embed a second Google Drive client in Go.

### 31.3 SQLite ownership

The Go binary owns:

- schema creation and migrations;
- profile/config transactions;
- pending event queue;
- local manifest;
- locks/leases needed for cross-process scheduling;
- runtime health state;
- consistent rolling database backups.

Do not use shell `source`, executable env files, or the `sqlite3` CLI as the canonical control path.

### 31.4 launchd installation

The Go CLI generates, bootstraps, updates, and removes launchd plist files.

Shell scripts may exist as developer conveniences or packaging wrappers, but normal installed operation must not depend on a project checkout or a collection of mutable shell scripts.

Recommended installed layout:

```text
<installed binary path>/knowledge-sync
~/.local/share/knowledge-sync/knowledge-sync.sqlite
~/.local/share/knowledge-sync/backups/
~/Library/Logs/knowledge-sync/
~/Library/LaunchAgents/com.local.knowledge-sync.<profile>.watch.plist
~/Library/LaunchAgents/com.local.knowledge-sync.<profile>.reconcile.plist
```

Exact binary installation prefix may be managed by Homebrew or another installer and is not encoded into profile identity.

### 31.5 Packaging target

Prefer prebuilt macOS binaries so users do not need a Go toolchain.

A Homebrew formula/tap is an appropriate distribution target once the CLI stabilizes. Packaging must declare `rclone` and `fswatch` as runtime dependencies rather than vendoring or reimplementing them.

### 31.6 Replaced architecture

Replace the old architecture:

```text
one config.env
one shell worker
one LaunchAgent
one log/state directory
15-minute StartInterval
full rclone operation for every scheduled run
```

with:

```text
one installed Go harness binary
one canonical SQLite control database
N profiles
2 LaunchAgents per active profile
  - Go watcher service supervising fswatch
  - hourly Go reconciliation entrypoint
persistent event/manifest state
fast changed-file upsert path
full authoritative reconciliation path
remote-level scheduler
per-profile locks
Drive sidecar metadata
stable Folder IDs
rclone as the only Google Drive data plane
```

The old Google Drive Desktop / mounted-filesystem package remains obsolete and must not be used by this design.

## 32. Acceptance tests

Before considering the new harness complete, test at least:

1. Add an Obsidian profile and verify rclone creates the Drive root.
2. Confirm Folder ID and sidecar ownership metadata are recorded.
3. Create a Markdown file and confirm automatic fast upload.
4. Modify the file repeatedly and confirm debounce batching.
5. Run `sync-now` immediately after a save and confirm the local-manifest barrier catches it.
6. Add a PDF attachment and confirm it uploads when under the size limit.
7. Confirm `.obsidian/`, `.git/`, Private, video, temp, and symlink targets are excluded according to policy.
8. Delete a local file and confirm a delayed full reconciliation removes it remotely.
9. Rename/move a directory and confirm rename tracking plus final removal of old paths.
10. Trigger more than `MAX_DELETE` expected removals and confirm live destructive sync does not start.
11. Approve a legitimate larger deletion using a one-shot `--allow-deletes` override.
12. Change an exclude so previously mirrored content becomes excluded and confirm removal is preflight-protected.
13. Kill the watcher after persisting pending work; restart and confirm the queue recovers.
14. Reboot with a stale lock and confirm PID-aware recovery.
15. Simulate a large pending backlog and confirm promotion to full reconciliation.
16. Run two profiles on the same remote and verify bounded remote-level concurrency/priority.
17. Run profiles on different remotes and confirm independent concurrency.
18. Simulate/introduce a remote duplicate condition and confirm fail-closed behavior.
19. Migrate a profile to another Google account using copy + size-only check + cutover + old-root Trash.
20. Confirm Viewer sharing to the ChatGPT Hub works when using an external storage owner.
21. Confirm quota warning behavior without automatic migration.
22. Remove a profile and confirm its remote data remains untouched and the profile becomes tombstoned.
23. Restore a tombstoned profile.
24. Forget a tombstone and confirm the human profile ID can then be reused with a new UUID.
25. Validate behavior on a representative 20k+ file profile and tune performance defaults from measured results.
26. Install a prebuilt `knowledge-sync` binary on a clean macOS user account without a Go toolchain and confirm normal runtime works with only `rclone` and `fswatch` present.
27. Confirm launchd jobs use absolute `knowledge-sync`, `rclone`, `fswatch`, and rclone-config paths and do not depend on interactive-shell environment variables.
28. Stop/restart watcher jobs repeatedly and confirm the Go parent cleanly terminates/reaps its `fswatch` child without orphan processes.
29. Run concurrent profile operations and confirm SQLite WAL/cross-process scheduler state remains consistent without holding database transactions across rclone network operations.

