# knowledge-sync implementation plan: `.gitignore` policy snapshots, suppression ledger, and explicit prune

## 0. Status

This is the implementation plan for evolving the existing `knowledge-sync` exclude lifecycle into a Git-compatible, durable policy model with deferred destructive cleanup.

**Implementation baseline:** `master` at `b8835f20dbbe7527cd12e7853978ab79375f51c6` (`Route full reconciliation and fast upserts through the worker`).

This revision supersedes the earlier design that proposed storing individual `exclude_gitignore` rules in SQLite. The user-facing policy source is now the profile source tree's repository-local `.gitignore` files, committed explicitly into a durable policy snapshot.

The change addresses three original problems:

1. structured path excludes cannot express general Git wildcard patterns such as matching arbitrary nested paths;
2. editing several excludes one by one creates several reconciliation requests;
3. making an existing mirrored path excluded can immediately imply remote deletion and collide with `max_delete` while the user is still editing policy.

The target lifecycle is:

```text
edit root/nested .gitignore files
        |
        | explicit `profile ignore update`
        v
durable immutable policy snapshot + policy_hash
        |
        v
worker-owned policy refresh
        |
        +--> active managed objects
        |
        +--> suppressed managed objects (kept remotely)
                    |
                    | explicit durable prune preview + authorization
                    v
               targeted deletion
```

The central invariant is:

> Ignore policy decides future eligibility; changing ignore policy does not itself authorize deletion of already managed remote objects.

---

## 0.1 Locked design decisions

The following decisions are settled for this implementation.

### Gitignore source and semantics

- Use `github.com/git-pkgs/gitignore` for Git-compatible pattern semantics.
- Do not invent a second parser, validator, wildcard language, or semantic-equivalence layer.
- The profile's source root is the logical Gitignore root even when it is not a Git repository and has no `.git/` directory.
- Read only repository `.gitignore` files: the root `.gitignore` plus nested `.gitignore` files that are reachable under Git traversal semantics.
- Do **not** load `.git/info/exclude`.
- Do **not** load global excludes or `core.excludesFile`.
- Runtime behavior must not require the `git` executable.
- Do not call `gitignore.New(root)` or `gitignore.NewFromDirectory(root)` for committed policy construction because those APIs can include non-repository-local sources. Build the matcher from `gitignore.New("")` plus snapshot bytes/scopes.
- Invalid patterns follow the third-party library's behavior: they remain part of the raw snapshot, are skipped by matching as the library defines, and are surfaced as warnings through `Matcher.Errors()`. `knowledge-sync` does not add a stricter validation language.
- `.gitignore` itself is an ordinary syncable file. It is not implicitly excluded merely because it is policy input.

### Policy commit lifecycle

- `.gitignore` files on disk are an editable working copy, not live synchronization policy.
- `profile ignore update <profile>` is the explicit policy commit action.
- The command captures a stable snapshot of the current reachable root/nested `.gitignore` files and atomically persists it with a new `policy_hash` and reconciliation generation.
- Worker, scans, fast-upsert eligibility, and reconciliation use only the durable committed snapshot, never the current live disk `.gitignore` contents.
- A byte-identical snapshot is a no-op and does not advance generation. Comment-only or whitespace changes are real snapshot changes and do advance generation.
- An empty snapshot is valid after migration: deleting all `.gitignore` files and running `ignore update` commits a policy with no Gitignore exclusions.
- New profiles capture the initial `.gitignore` snapshot during `profile add`, before first synchronization.
- `ignore status` compares disk policy to the committed snapshot read-only; it never commits policy implicitly.

### Legacy structured excludes

The existing rule types:

```text
exclude_path_prefix
exclude_dir_name
exclude_filename
exclude_extension
```

are migration inputs, not permanent parallel policy semantics.

- Upgrade converts existing structured excludes into standard Gitignore patterns that preserve their current effective behavior as closely and mechanically as possible.
- Converted rules are stored as a synthetic durable `legacy_migrated` policy snapshot and are evaluated by the same Gitignore matcher as future policies.
- The old structured-rule CLI must stop accepting new rules after migration support lands.
- The first `ignore update` for a profile that still has `policy_source=legacy_migrated` requires explicit `--accept-legacy-drop` confirmation. No semantic-equivalence inference bypasses this confirmation.
- After that successful commit, `policy_source=gitignore` permanently becomes the sole policy source.
- Profiles with no legacy excludes may migrate directly to an empty `gitignore`-source committed policy, preserving current behavior without an unnecessary legacy-drop gate.

### Generation and policy identity

- Keep one monotonic profile input/reconciliation generation. Policy edits and filesystem changes must not create competing numeric counters.
- Policy snapshot identity is separately represented by a content-derived `policy_hash`; this is an identity/fingerprint, not a second scheduling generation.
- Policy commit must atomically persist the snapshot, `policy_hash`, policy metadata, and newer reconciliation debt.
- Ordinary filesystem edits may advance the unified input generation without changing `policy_hash`.
- Prune authorization is bound to `policy_hash`, not to the general input generation, so unrelated file edits do not invalidate an approved policy cleanup.

### Suppression and deletion

- A previously mirrored managed object that becomes ignored is `suppressed`, not deleted.
- Suppressed objects remain in the managed ledger and are protected during ordinary reconciliation.
- Removing an ignore rule before prune can reactivate a suppressed object without requiring a delete/re-upload cycle when the remote object is still present.
- Explicit prune is the only normal path that deletes an object solely because ignore policy suppresses it.
- V1 prune guarantees targeted file deletion only. It does not promise removal of now-empty remote directory shells.

### Prune authorization

- `profile prune preview <profile>` creates a durable immutable prune request, rather than a traditional side-effect-free `--dry-run`.
- A request records the committed `policy_hash` and freezes the candidate managed paths that the user inspected.
- Only one unexecuted current preview is active per profile. Creating a newer preview marks older `previewed` / `approval_required` requests `superseded`.
- If committed policy changes, an unexecuted old request becomes `stale` and must never delete anything.
- `--allow-deletes N` is a safety ceiling, never a request to delete the first N objects. If the candidate count exceeds the effective limit, delete zero objects.
- The profile `max_delete` is the default prune ceiling. `prune execute <request-id> --allow-deletes N` may grant a one-request override without changing profile configuration.
- Once a prune request is authorized, that authorization is durable across retryable worker failures for the same immutable request and `policy_hash`.
- This prune authorization is intentionally different from the existing `reconcile-now --allow-deletes` one-attempt override, which is consumed at reconcile claim time and is not re-granted to retries. The two mechanisms must not share authorization state.

### Worker ownership and status

- The worker remains the single rclone data-plane owner.
- CLI and watcher persist durable facts/intents and wake/observe the worker; they do not run competing transfers.
- After acquiring profile operation ownership, the worker must reload the current durable profile and committed policy snapshot before full reconciliation, policy refresh, fast-upsert, or prune execution.
- Policy refresh must be `ready` for the current `policy_hash` before prune preview/execution is available.
- `approval_required`, `stale`, and `superseded` are normal prune control states, not profile synchronization failures.
- `profile status` shows concise policy/suppression/prune summary; `profile ignore status` shows detailed policy state.
- Existing socket-first live status architecture is extended rather than replaced.

---

## 1. Current master architecture and relevant gaps

### 1.1 Worker is already the only data-plane owner

As of the baseline commit:

- `reconcile-now`, scheduled reconcile, `sync`, and `sync-now` submit durable intent/events;
- watcher records durable events and wakes the worker;
- worker claims full reconciliation work and also drains due fast-event batches;
- full reconciliation debt has priority over fast batches;
- rclone execution happens in the worker under the profile lock and remote lease.

This is the desired foundation. This plan must extend that ownership model, not create a second execution path.

### 1.2 Fast-upsert now rechecks eligibility, but still receives a stale profile snapshot

`runWorkerPass` currently obtains profiles before acquiring the profile lock. After lock/lease acquisition, `runFastUpsertBatch` and full reconciliation can continue using that earlier `*state.Profile`.

`runFastUpsertBatch` does recheck source existence and current filter eligibility before upload, which is directionally correct, but it builds the filter from the passed profile object.

Required change:

```text
acquire profile operation lock
        |
        v
reload durable profile
reload committed policy snapshot
construct matcher/filter
        |
        v
claim/execute work using that owned snapshot
```

Do not treat a profile object obtained during the scheduling scan as execution authority.

### 1.3 Watcher is now a fact producer, but still has a startup filter

`runWatcher` still constructs `filter.FromProfile(p)` once at startup and `Watcher.classify` can discard events that match that cached filter.

That can permanently lose future events after a committed policy change makes a formerly ignored path eligible.

V1 correctness requirement:

> No event may be discarded solely because it matches a policy snapshot older than the currently committed `policy_hash`.

The simplest implementation is to stop using path policy as a watcher-side drop gate and let the worker apply the committed matcher to durable events. An implementation may instead hot-reload a watcher matcher keyed by committed `policy_hash`, but stale policy must fail conservative (record the event), not fail by dropping it.

### 1.4 Current manifest is not sufficient for deferred prune

The current manifest models an eligible local snapshot and full reconcile can replace it wholesale. Fast-upsert success does not currently require a manifest update.

Deferred prune needs a managed-object ledger. In particular:

```text
fast upload succeeds
        |
        | if ledger is not updated
        v
later policy suppresses path
        |
        v
system cannot prove remote object is managed
```

Therefore successful fast-upsert must participate in ledger maintenance.

### 1.5 Existing manual delete override is not prune authorization

Current master has durable `ManualReconcileIntent` metadata and `reconcile-now --allow-deletes N`:

- attached to a submitted reconciliation generation;
- atomically captured/consumed when a run is claimed;
- intended for one reconciliation attempt;
- automatic retry does not inherit the override.

Keep that behavior for ordinary authoritative reconciliation. Do not reuse those columns or claim semantics for durable prune approval.

### 1.6 Live status is socket-first

Current status uses worker socket snapshots when available and SQLite fallback otherwise. The snapshot currently separates durable profile/sync lifecycle from ephemeral activity.

Add policy/prune durable summaries to this model. Do not add a second status daemon or per-frame SQLite telemetry path.

---

## 2. Target architecture

The system should have four clearly separated concepts:

```text
1. Disk ignore working copy
   root/nested .gitignore files currently on disk

2. Committed policy snapshot
   durable exact bytes/scopes + policy_hash

3. Managed mirror ledger
   remote-owned file paths in active/suppressed state

4. Prune request
   immutable user-inspected suppressed target set + authorization/progress
```

Data flow:

```text
filesystem events ------------------------------+
                                                 |
.gitignore edits --(ignore update)--> policy ---+--> unified reconciliation debt
                                                 |
                                                 v
                                      worker policy/full refresh
                                                 |
                         +-----------------------+-----------------------+
                         |                                               |
                         v                                               v
                    active ledger                                  suppressed ledger
                         |                                               |
                         | ordinary reconcile                            | prune preview
                         v                                               v
                       remote                                  durable immutable request
                                                                         |
                                                                         | explicit execute
                                                                         v
                                                                targeted worker delete
```

---

## 3. Gitignore matcher integration

### 3.1 Dependency

Add/use:

```text
github.com/git-pkgs/gitignore
```

Committed policy matcher construction must look conceptually like:

```go
m := gitignore.New("")
for _, f := range snapshot.FilesInScopeOrder() {
    m.AddPatterns(f.Content, f.ScopeDir)
}
```

Do not call APIs that implicitly load global Git configuration, `.git/info/exclude`, or execute `git config`.

### 3.2 Directory awareness

Change the filter API so Git directory-only patterns have a real directory bit, for example:

```go
Excluded(relPath string, isDir bool) (bool, string)
```

or a typed equivalent.

All candidate paths passed to the matcher are:

- relative to `profile.SourcePath`;
- slash-separated;
- free of `.` / `..` escape;
- accompanied by correct file-vs-directory identity.

### 3.3 Non-path eligibility remains separate

`max_file_size`, symlink handling, and other non-path restrictions remain separate checks in `filter.Engine`. Do not encode them as fake `.gitignore` rules.

### 3.4 Invalid patterns and provenance

The committed snapshot retains exact file bytes. Build the matcher and expose `Matcher.Errors()` as warnings containing source/scope/line where possible.

Warnings:

- do not reject `ignore update` merely because the library reports an invalid pattern;
- do not rewrite or normalize the user's `.gitignore` to make it valid;
- appear in `profile ignore status` and update output;
- should be testable using the library's own behavior.

### 3.5 Nested traversal semantics

The stable snapshot collector must discover nested `.gitignore` files under the same traversal semantics used by the matcher:

1. start with `gitignore.New("")`;
2. load root `.gitignore` bytes into root scope if present;
3. traverse entries deterministically;
4. use the current matcher to decide whether a directory is ignored before descending;
5. upon entering a reachable directory, load that directory's `.gitignore` before processing its children;
6. never descend into a directory ignored by the current ancestor policy merely to discover a deeper `.gitignore`.

This preserves the Git rule that rules in an ignored parent subtree cannot revive content by virtue of a nested `.gitignore` that Git would never traverse.

---

## 4. Durable policy snapshot

### 4.1 Snapshot contents

A committed policy is not just a flattened list of patterns. Persist enough information to reconstruct scoped matching exactly:

```text
profile_id
policy_hash
policy_source            gitignore | legacy_migrated
committed_generation
committed_at
refresh_state            pending | running | ready | error
refreshed_policy_hash    nullable
matcher_warning_count
```

Snapshot file rows should contain at least:

```text
profile_id
policy_hash
relative_path            e.g. .gitignore, src/.gitignore
scope_dir                 e.g. "", src
content                   exact bytes/text
content_order             deterministic scope order
```

A synthetic migrated legacy snapshot may use a reserved non-filesystem source name while retaining root scope.

### 4.2 `policy_hash`

Compute `policy_hash` from a canonical encoding of the complete snapshot, including:

- relative file identity;
- scope directory;
- exact content length and bytes;
- deterministic file ordering.

Do not hash mtimes, inode numbers, or other incidental metadata.

Two byte-identical snapshots produce the same hash. A comment-only change produces a different hash.

### 4.3 No-op update

`ignore update` compares the newly captured canonical snapshot hash with the committed one.

If equal:

- no policy rows are rewritten unnecessarily;
- unified generation is not advanced;
- no policy refresh debt is created;
- command exits success and reports `unchanged`.

No semantic normalization is attempted.

---

## 5. Stable snapshot collection

### 5.1 Why stability is required

A commit must not combine a root `.gitignore` from one editor save with a nested `.gitignore` from another save into a policy state that never existed on disk as a stable configuration.

### 5.2 Collector algorithm

Implement a collector that can return:

```go
type IgnoreSnapshot struct {
    Files []IgnoreFileSnapshot
    Hash  string
    Warnings []PatternWarning
}
```

A robust update flow is:

1. collect snapshot A using deterministic Git-aware traversal;
2. collect snapshot B again;
3. require `A.Hash == B.Hash`;
4. otherwise fail with `ignore files changed during snapshot; retry update`;
5. acquire the profile operation lock;
6. perform a final verification collection/hash comparison before the DB commit if lock acquisition may have waited behind another operation;
7. atomically persist only if the candidate is still current.

An equivalent implementation with the same stability guarantee is acceptable.

### 5.3 I/O failure handling

- Missing `.gitignore`: normal; simply absent from the snapshot.
- No `.gitignore` anywhere: valid empty snapshot.
- Permission/read/traversal error that prevents knowing the reachable policy set: fail the update.
- On failure, retain the old committed snapshot, old hash, and old generation unchanged.

Do not commit a partial policy with warnings for I/O errors. Pattern compilation warnings and filesystem read failures are different classes.

---

## 6. Profile creation and legacy migration

### 6.1 New profile creation

Before first synchronization becomes eligible, `profile add` captures a stable `.gitignore` snapshot using the same collector as `ignore update`.

Create the profile and its initial policy atomically enough that no worker can run the profile with an accidental empty policy between profile creation and policy persistence.

A source that contains no `.gitignore` commits a valid empty policy.

If snapshot collection fails, fail profile creation rather than starting an unsafe first upload.

### 6.2 Legacy migration strategy

Do not keep legacy structured rules as a permanent second matcher.

For each profile with existing structured excludes:

1. read existing rules in their current effective form;
2. convert each rule to literal/standard Gitignore syntax with escaping;
3. construct a synthetic root-scoped Gitignore snapshot;
4. persist it as `policy_source=legacy_migrated`;
5. keep existing synchronization eligibility behavior through the one Gitignore matcher;
6. disable creation of new structured exclude rows through the CLI.

Migration must be covered by compatibility tests against the current `filter.Engine` for representative and adversarial literal values.

### 6.3 Legacy conversion requirements

The converter must preserve the old implementation's **actual** behavior, including historical quirks, instead of assuming the rule type's name is a perfect specification.

At minimum:

- `exclude_path_prefix` becomes a root-anchored literal path pattern that excludes that path and descendants;
- `exclude_dir_name` becomes a Gitignore expression that excludes descendants under directories with that literal name at arbitrary depth;
- `exclude_filename` becomes an unanchored literal name pattern; current behavior can also match a directory with that final name, so migration must test both candidate kinds;
- `exclude_extension` preserves current case-insensitive extension comparison, using standard Gitignore bracket expressions where necessary rather than adding a custom matcher mode;
- Git metacharacters in legacy values (`*`, `?`, `[`, `]`, `!`, `#`, backslash, leading/trailing spaces, etc.) are escaped so values that were historically literal remain literal after conversion.

Do not hand-wave this conversion. Add a table-driven oracle test that evaluates old engine vs migrated matcher over a large candidate matrix.

### 6.4 First switch to disk `.gitignore`

For `policy_source=legacy_migrated`, the first:

```bash
knowledge-sync profile ignore update <profile>
```

must refuse to commit and show the migration boundary unless the user supplies:

```text
--accept-legacy-drop
```

Suggested message:

```text
This profile is using policy migrated from legacy excludes.
The current disk .gitignore snapshot will become the sole policy source.

legacy rules:          N
disk ignore files:     M
currently suppressed:  K

Re-run with --accept-legacy-drop to commit this policy source change.
```

Do not bypass this gate because a home-grown comparator believes the two policies are equivalent.

After successful commit, set `policy_source=gitignore`; the gate never reappears for that profile.

---

## 7. Atomic policy commit and reconciliation debt

### 7.1 Unified generation

Continue using the existing monotonic profile reconciliation/input generation as the work-debt authority.

Policy commit is another input capable of changing desired mirror state, so it advances that same sequence exactly once per changed snapshot.

The physical database column may retain an older name such as `source_generation` during incremental migration, but new APIs/comments should not assume all generation changes originate from filesystem events.

### 7.2 Transaction

The final `ignore update` DB transaction must atomically:

1. verify profile is still valid/enabled for policy mutation as appropriate;
2. replace committed snapshot rows;
3. persist new `policy_hash` and `policy_source`;
4. increment unified input generation exactly once;
5. advance `desired_generation` to include the new input;
6. mark policy refresh `pending` for this hash/generation;
7. set the durable reconciliation/debounce timestamps required by current scheduler semantics;
8. mark older unexecuted prune requests stale where their `policy_hash` differs;
9. commit.

A crash must never leave the new snapshot durable without corresponding reconciliation debt.

### 7.3 Debounce/coalescing

The user's edit loop is outside the synchronizer:

```text
edit .gitignore many times
        |
        | one explicit update
        v
one policy generation
```

After commit, reuse the existing durable destructive/policy quiet-period behavior where appropriate. Repeated explicit updates may still coalesce worker refresh opportunities, but every distinct committed snapshot retains a monotonic debt boundary.

Do not reintroduce the old per-rule batch CLI as the primary interface.

---

## 8. Policy refresh

### 8.1 Definition

A policy refresh is the worker-owned operation that applies the current committed policy to local state and the managed ledger.

A policy refresh may require a full local scan, but it is not authorization to delete newly ignored remote objects.

```text
full local classification
    !=
prune
```

### 8.2 State

Track current policy refresh explicitly enough to answer:

```text
committed policy_hash
refresh pending/running/ready/error
which policy_hash was last successfully refreshed
```

Prune preview is available only when:

```text
refresh_state == ready
AND refreshed_policy_hash == committed_policy_hash
```

### 8.3 Worker ownership

Worker scheduling must acquire the profile operation lock, then reload:

- profile;
- committed policy snapshot/hash;
- relevant reconciliation state;
- managed ledger state.

Construct the matcher from that owned durable snapshot.

Do not use the `*state.Profile` captured before the lock as filter authority.

### 8.4 Policy commit while work is running

`ignore update` and destructive prune execution use the same profile operation lock.

A policy update that arrives while a full reconcile or prune owns the lock waits for that ownership boundary. Once committed, it creates newer durable debt.

This serialization ensures prune never partially switches to a new policy midway through its immutable authorized set.

---

## 9. True local deletion vs policy suppression

### 9.1 Required distinction

When an existing managed path disappears from the eligible scan, the system must determine whether it was:

- actually deleted/renamed locally while active; or
- merely hidden by a newly committed ignore policy.

Do not infer ordinary deletion solely from absence in an eligible scan.

### 9.2 Durable event chronology

Preserve ordinary deletion when a durable delete/rename event can be proven to have been recorded while the path was active under the prior committed policy.

Implement this with durable ordering evidence, preferably by recording policy context with pending events in the same SQLite transaction, for example:

```text
observed_policy_hash
observed_policy_generation
policy_context_known
```

The exact schema may vary, but the ordering relationship must be durable rather than reconstructed from wall-clock timestamps alone.

### 9.3 Fail-safe ambiguity

If restart/catch-up means chronology cannot be proven and the current policy suppresses a previously managed path, classify it as `suppressed` rather than authorizing an ordinary delete.

A later explicit prune can remove it safely.

False suppression is recoverable; an unproven destructive delete is not.

---

## 10. Managed mirror ledger

### 10.1 Replace snapshot-only manifest semantics

Evolve `manifest` into a managed file ledger with at least:

```text
profile_id
rel_path
state                    active | suppressed
size / mod_time / hash   as currently useful
suppressed_policy_hash   nullable
suppressed_generation    nullable
managed_at / updated_at  optional
```

Existing manifest rows migrate as `active`.

The ledger tracks managed **files** in V1. It does not attempt to own remote directory objects.

### 10.2 State transitions

Policy/full refresh applies transitions such as:

```text
active + still eligible                 -> active
active + now ignored                    -> suppressed
suppressed + eligible again + exists    -> active
suppressed + still ignored              -> suppressed
```

A true proven local deletion follows ordinary reconcile deletion semantics rather than becoming suppression merely because the file is absent.

### 10.3 Never blanket-replace away suppressed rows

Remove or constrain `ManifestReplaceAll` behavior so a full eligible scan cannot erase suppressed ownership records.

Use merge/upsert/delete operations with explicit state transitions.

### 10.4 Fast-upsert ledger barrier

Current fast-upsert clears exact pending event versions after successful remote copy but does not require manifest rewrite. Under this plan, change the success ordering to:

```text
1. remote fast upload succeeds
2. durable managed-ledger upsert marks path active
3. clear only the exact pending event version(s)
4. record fast success
```

If the worker crashes after step 1 and before step 2, the pending event remains and retrying the upload is idempotent. The retry then repairs the ledger before clearing the event.

Do not clear a successful fast-upload event before durable managed ownership is recorded.

---

## 11. Ordinary authoritative reconciliation

### 11.1 Core invariant

Ordinary reconciliation converges active managed content and legitimate local deletions **without deleting suppressed content**.

The current coupling:

```text
not in source list
  -> --delete-excluded
  -> remote deletion
```

cannot remain the policy-suppression behavior.

### 11.2 Keep rclone as transport

Do not build a custom transfer engine. Rclone remains responsible for copy/check/delete transport.

The application decides:

- active desired paths;
- protected suppressed paths;
- proven ordinary deletion paths;
- delete budgets/authorization.

### 11.3 Transport spike and integration tests

Before finalizing command composition, prove with rclone integration tests that the selected approach satisfies:

1. active files upload/update;
2. proven local deletes are removed subject to ordinary delete budget;
3. existing authoritative remote drift semantics remain intentional;
4. suppressed files survive ordinary reconcile;
5. ignored local files are not newly uploaded;
6. reactivated files resume ordinary synchronization;
7. prune later deletes only its frozen suppressed targets.

If one `rclone sync` command cannot express those invariants cleanly, prefer multiple rclone operations with application-approved path lists over reimplementing transfer semantics.

### 11.4 Ordinary delete budget remains distinct

Keep existing profile `max_delete` and `reconcile-now --allow-deletes N` behavior for ordinary reconciliation.

The existing manual override remains one-attempt claim metadata. A failed automatic retry does not inherit it unless a later explicit manual reconcile grants a new override.

Suppressed objects must not be counted as ordinary local-delete candidates merely because they are absent from the active desired set.

---

## 12. Watcher and fast-event integration

### 12.1 Watcher responsibilities

Watcher is a durable filesystem fact producer, not the final eligibility authority and not a transfer owner.

It should:

- classify safe file modify/create vs destructive/uncertain events;
- persist event facts;
- wake the worker;
- avoid losing facts due to stale ignore policy.

### 12.2 Policy filtering at watcher boundary

Preferred V1 implementation: remove committed path-policy filtering as an event-drop gate in `Watcher.classify` and let the worker filter current durable events under the owned committed snapshot.

If a watcher policy cache is retained for volume reduction:

- cache must be keyed by committed `policy_hash`;
- it must hot-reload when hash changes;
- if cache freshness is uncertain, record the event rather than drop it.

### 12.3 Worker fast batch

When no full/policy reconciliation debt exists and a fast batch is due:

1. worker already owns profile lock and remote lease;
2. reload profile + committed policy snapshot under ownership;
3. recheck each event's source existence and current committed eligibility;
4. upload eligible paths;
5. upsert managed ledger active rows;
6. clear only exact event versions included in the successful batch.

A destructive/uncertain event continues to promote full reconciliation rather than unsafe fast deletion.

---

## 13. Prune preview model

### 13.1 Why preview is durable

A destructive authorization should refer to exactly the objects the user inspected.

Therefore:

```bash
knowledge-sync profile prune preview <profile>
```

creates a durable request instead of merely printing a transient current set.

### 13.2 Eligibility

Preview refuses when current policy refresh is not ready:

```text
policy refresh pending; prune preview not ready
```

It never prints an incomplete suppressed candidate set as if it were authoritative.

### 13.3 Immutable candidate set

A preview transaction creates:

```text
prune_request
  request_id
  profile_id
  policy_hash
  state
  candidate_count
  candidate_digest
  default_max_delete
  authorized_limit nullable
  created_at / updated_at
```

and target rows:

```text
request_id
rel_path
state            pending | deleted | missing
attempt_count
last_error nullable
updated_at
```

Copy candidates from managed ledger rows that are `suppressed` for the current refreshed `policy_hash`.

Do not derive the execution set again later from a mutable query.

### 13.4 Superseding previews

Creating a new preview marks older unexecuted requests in `previewed` or `approval_required` as `superseded`.

Do not supersede a request already `pending`, `running`, or retrying merely because the user asks for another preview; report the existing active destructive operation instead.

---

## 14. Prune authorization and execution

### 14.1 States

Recommended durable request states:

```text
previewed
approval_required
pending
running
retrying
completed
stale
superseded
failed
```

`approval_required`, `stale`, and `superseded` do not set profile sync health to error.

### 14.2 Effective delete ceiling

At preview time, record candidate count and profile default `max_delete` for user context.

At execute time:

```bash
knowledge-sync profile prune execute <request-id>
knowledge-sync profile prune execute <request-id> --allow-deletes N
```

compute an effective ceiling:

- explicit `--allow-deletes N` when supplied;
- otherwise current/recorded profile default according to implementation choice, documented consistently.

Hard invariant:

```text
candidate_count > effective_ceiling
        => delete zero objects
```

Do not partially delete the first N candidates.

Persist the accepted authorization limit on the request before queueing it so worker retries retain the same authorization.

### 14.3 Policy staleness

Before authorizing and again under the worker profile lock before first destructive action, require:

```text
request.policy_hash == current committed policy_hash
AND current refresh is ready for that hash
```

Otherwise mark request `stale`, delete zero files, and require a new preview.

Do not bind staleness to unrelated filesystem generation changes.

### 14.4 Profile lock serialization

Actual destructive prune execution owns the same profile operation lock used by policy commit/reconciliation.

Once prune enters destructive execution, a concurrent `ignore update` waits for the lock. This preserves an immutable authorization boundary.

If policy commit happens first, the old request becomes stale before deletion starts.

### 14.5 Targeted deletion only

Delete only the immutable managed target rows for the request.

- Remote missing target: success, mark `missing`.
- Successfully deleted target: mark `deleted`.
- Do not broaden to unrelated remote drift.
- Do not run broad empty-directory cleanup in V1.

### 14.6 Crash recovery

Persist progress per target/batch.

If worker crashes after remote delete but before the target row is marked, retry may observe remote missing and converge that target to success.

Retryable remote/provider errors move request to `retrying`; worker automatically retries the same authorized request while policy hash remains valid.

Terminal execution errors may move the request to `failed` while retaining target rows for diagnosis/resume policy.

### 14.7 Completed-request compaction

After all targets are confirmed `deleted` or `missing`:

1. remove corresponding suppressed ledger rows;
2. mark request `completed` with summary counts/timestamps;
3. retain request summary/audit fields;
4. delete per-target rows for the completed request to avoid unbounded SQLite growth.

Never compact target rows for running/retrying/failed requests needed for recovery.

---

## 15. Scheduling priority

Extend the worker's existing rule that full reconciliation debt beats fast events.

Recommended per-profile priority under the single worker data plane:

```text
1. profile deletion lifecycle / existing terminal ownership rules
2. claimable full reconciliation or policy-refresh debt
3. authorized pending/retrying prune
4. due fast-upsert batch
```

Rationale:

- prune cannot run until policy refresh is ready;
- full/policy debt must establish authoritative ledger state before destructive cleanup;
- an already authorized durable prune should not be starved indefinitely by frequent fast events;
- fast events remain hints that can be retried/coalesced.

Use the existing profile lock and remote lease discipline for all remote operations.

---

## 16. Status and observability

### 16.1 Extend socket-first snapshot

Add durable policy/prune summary to the current `live.StatusSnapshot` model rather than introducing a separate observer channel.

Conceptually:

```go
type PolicyS struct {
    Source              string
    PolicyHash          string
    CommittedGeneration int64
    RefreshState        string
    RefreshedPolicyHash *string
    SuppressedCount     int64
    MatcherWarnings     int64
}

type PruneS struct {
    RequestID       *string
    State           *string
    CandidateCount  int64
    CompletedCount  int64
}
```

Exact struct layout may vary.

SQLite fallback should expose the same durable policy/prune facts while continuing to omit ephemeral speed/stall claims.

### 16.2 `profile status`

Show concise operational summary, for example:

```text
Sync:              healthy
Policy:            committed, refresh ready
Disk ignore:       modified
Suppressed:        412
Prune:             approval required (#42, 412 targets)
```

`Disk ignore` is a read-only comparison against current disk snapshot. In `--watch`, avoid an expensive full tree policy rescan at telemetry frame frequency; sample at a bounded cadence or react to relevant filesystem facts.

### 16.3 `profile ignore status`

Show detailed policy information:

```text
policy_source
policy_hash
committed_generation
committed_at
committed ignore file count
current disk snapshot: clean | modified | unreadable
matcher warnings with source/line
refresh state / refreshed hash
active managed count
suppressed managed count
latest prune request summary
```

The command never commits disk policy.

### 16.4 CLI exit codes

- successful preview creation: exit 0 even when state is `approval_required`;
- `prune execute` that cannot execute because authorization is insufficient: non-zero, request remains `approval_required`;
- `prune execute` that discovers stale policy: non-zero, request becomes `stale`;
- `stale`, `superseded`, and `approval_required` remain non-error profile health states;
- ordinary one-shot `profile status` continues to report state rather than treating a reported non-healthy state as a command transport failure, consistent with existing status conventions.

---

## 17. CLI surface

### 17.1 New ignore commands

Primary interface:

```bash
knowledge-sync profile ignore update <profile>
knowledge-sync profile ignore update <profile> --accept-legacy-drop
knowledge-sync profile ignore status <profile>
```

`ignore update` output should include at least:

```text
policy: changed | unchanged
ignore files captured: N
policy hash: ...
generation: old -> new (when changed)
matcher warnings: N
policy refresh: pending | unchanged
```

When feasible after lightweight ledger inspection, also show current/newly suppressed context without blocking the command on a full local scan.

### 17.2 Legacy exclude CLI

Deprecate/remove mutation commands that create new structured excludes.

If retained temporarily for compatibility, they must fail with a clear migration message rather than creating a second policy source, for example:

```text
structured exclude editing is deprecated; edit .gitignore and run `profile ignore update`
```

Read-only legacy diagnostics may remain during migration if useful.

### 17.3 Prune commands

Primary interface:

```bash
knowledge-sync profile prune preview <profile>
knowledge-sync profile prune execute <request-id>
knowledge-sync profile prune execute <request-id> --allow-deletes N
knowledge-sync profile prune status <profile-or-request>
```

A deprecated `--dry-run` alias may map to `preview`, but documentation should teach `preview` because the operation intentionally creates durable state.

### 17.4 Existing reconcile command

Keep:

```bash
knowledge-sync reconcile-now <profile> --allow-deletes N
```

or its current command path/alias with its existing one-attempt ordinary reconciliation semantics.

Documentation must explicitly distinguish it from prune:

```text
reconcile --allow-deletes
  -> one reconcile attempt's ordinary deletion budget

prune execute --allow-deletes
  -> durable authorization ceiling for one immutable suppressed-object request
```

---

## 18. Database migration sketch

Exact migration numbering follows current master (which already includes v8 work). Add new migrations after the latest version.

### 18.1 Policy tables

Possible schema:

```sql
CREATE TABLE profile_ignore_policy (
    profile_id TEXT PRIMARY KEY,
    policy_source TEXT NOT NULL,
    policy_hash TEXT NOT NULL,
    committed_generation INTEGER NOT NULL,
    committed_at TEXT NOT NULL,
    refresh_state TEXT NOT NULL,
    refreshed_policy_hash TEXT,
    matcher_warning_count INTEGER NOT NULL DEFAULT 0,
    FOREIGN KEY(profile_id) REFERENCES profiles(id) ON DELETE CASCADE
);

CREATE TABLE profile_ignore_snapshot_files (
    profile_id TEXT NOT NULL,
    policy_hash TEXT NOT NULL,
    relative_path TEXT NOT NULL,
    scope_dir TEXT NOT NULL,
    content BLOB NOT NULL,
    content_order INTEGER NOT NULL,
    PRIMARY KEY(profile_id, policy_hash, relative_path),
    FOREIGN KEY(profile_id) REFERENCES profiles(id) ON DELETE CASCADE
);
```

If only the current committed snapshot is retained, `policy_hash` may not need to be part of every primary key. Keeping it can make atomic replacement/audit easier. Choose based on transaction simplicity.

### 18.2 Ledger migration

Add active/suppressed state and suppression provenance to manifest/managed ledger.

If rebuilding the table is cleaner than additive columns, preserve current file metadata and existing primary key behavior.

### 18.3 Pending event policy context

Add durable policy-order evidence needed for true deletion vs suppression classification. Prefer a policy hash/generation captured atomically with event persistence over relying only on timestamps.

### 18.4 Prune tables

Possible schema:

```sql
CREATE TABLE prune_requests (
    request_id TEXT PRIMARY KEY,
    profile_id TEXT NOT NULL,
    policy_hash TEXT NOT NULL,
    state TEXT NOT NULL,
    candidate_count INTEGER NOT NULL,
    candidate_digest TEXT NOT NULL,
    default_max_delete INTEGER NOT NULL,
    authorized_limit INTEGER,
    deleted_count INTEGER NOT NULL DEFAULT 0,
    missing_count INTEGER NOT NULL DEFAULT 0,
    last_error TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    completed_at TEXT,
    FOREIGN KEY(profile_id) REFERENCES profiles(id) ON DELETE CASCADE
);

CREATE TABLE prune_targets (
    request_id TEXT NOT NULL,
    rel_path TEXT NOT NULL,
    state TEXT NOT NULL,
    attempt_count INTEGER NOT NULL DEFAULT 0,
    last_error TEXT,
    updated_at TEXT NOT NULL,
    PRIMARY KEY(request_id, rel_path),
    FOREIGN KEY(request_id) REFERENCES prune_requests(request_id) ON DELETE CASCADE
);
```

Add indexes for current active request by profile/state and for retryable requests.

### 18.5 Do not overload current manual reconcile columns

Do not store prune approval in:

```text
pending_manual_allow_deletes
manual_delete_override
```

or equivalent run claim fields. Those belong to the existing one-attempt reconcile lifecycle.

---

## 19. State-layer APIs

Favor explicit transactional APIs instead of command code manipulating individual rows.

Suggested interfaces:

```go
CollectIgnoreSnapshot(sourceRoot string) (*IgnoreSnapshot, error)

CommitIgnoreSnapshot(
    profileID string,
    snapshot IgnoreSnapshot,
    acceptLegacyDrop bool,
) (PolicyCommitResult, error)

GetCommittedIgnorePolicy(profileID string) (*CommittedPolicy, error)

MarkPolicyRefreshRunning(profileID, policyHash string, runID string) error
MarkPolicyRefreshReady(profileID, policyHash string, generation int64) error
MarkPolicyRefreshError(profileID, policyHash string, err error) error

ApplyManagedRefresh(profileID string, policyHash string, scan ScanResult, evidence EventEvidence) error
UpsertManagedActive(profileID, relPath string, metadata ...) error

CreatePrunePreview(profileID string) (*PruneRequest, error)
AuthorizePrune(requestID string, allowDeletes *int) (*PruneRequest, error)
ClaimPrune(profileID string, workerID string) (*PruneRequest, ClaimResult, error)
MarkPruneTargetResult(requestID, relPath string, result TargetResult) error
CommitPruneComplete(requestID string) error
```

Use transactions so state transitions and debt/authorization cannot be torn apart by crashes.

---

## 20. Worker execution changes

### 20.1 Restructure execution snapshot loading

`runWorkerPass` currently scans active profiles before acquiring their locks. Retain that as scheduling discovery only.

Inside the lock:

```text
reload profile
reload sync state as needed
reload committed policy snapshot
construct filter/matcher
then claim/execute
```

Pass an execution context/snapshot object to full reconcile and fast-upsert so they do not accidentally reconstruct filters from stale pre-lock profiles.

### 20.2 Policy refresh integration

A full reconciliation claim whose target includes policy debt should run scan/classification against the owned committed policy and update ledger state.

Only mark refresh ready after the ledger classification for that policy hash is durable.

If remote reconciliation later fails but local ledger classification succeeded, decide carefully whether refresh may be ready independently. Conservative V1 recommendation: mark policy refresh ready only at the point where ordinary reconciliation has successfully established the protected active/suppressed remote model for that policy. Tests should encode the chosen boundary.

### 20.3 Prune activity

Add a worker activity kind such as `prune` so socket observers can show:

- request ID;
- current phase;
- completed/total targets;
- current item when useful;
- retry state.

Do not overload `full_reconcile` activity with prune semantics.

### 20.4 Recovery pass

Extend `workerRecover` to:

- orphan/retry prune requests left `running` by a dead worker;
- preserve durable authorization and per-target progress;
- mark requests stale if policy hash no longer matches before retrying;
- never re-create an execution set from current suppressed rows for an existing request.

---

## 21. Test plan

### 21.1 Matcher conformance

Table-driven tests for:

```text
*
**
?
[abc]
[a-z]
POSIX character classes supported by dependency
leading /
trailing /
backslash escaping
comments
escaped # / !
negation
last-match-wins
nested .gitignore scope
ignored parent traversal
file vs directory candidates
```

Where Git is installed in development/CI, optional oracle tests may compare representative behavior to Git. Runtime code must not depend on Git.

### 21.2 Source-boundary tests

Prove collector does **not** read:

```text
.git/info/exclude
global core.excludesFile
~/.config/git/ignore
```

and works when no `.git/` directory exists.

Run runtime matching tests with controlled/empty `PATH` to prove no Git executable requirement.

### 21.3 Snapshot stability tests

- root `.gitignore` changes between pass A/B -> update fails, old policy retained;
- nested file added/removed during capture -> update fails;
- read permission error -> update fails;
- empty snapshot commits successfully;
- byte-identical update does not bump generation;
- comment-only change does bump generation;
- invalid pattern commits with matcher warning according to dependency behavior.

### 21.4 Profile add tests

- existing `.gitignore` is committed before first sync;
- nested rules are honored on first sync;
- no `.gitignore` is safe empty policy;
- unreadable policy source prevents unsafe profile initialization.

### 21.5 Legacy migration tests

For every legacy rule type:

- build old `filter.Engine` with representative literal values;
- convert to synthetic Gitignore snapshot;
- compare eligibility across files/directories and nested paths;
- include uppercase/lowercase extension cases;
- include literal Git metacharacters and spaces;
- verify first disk-policy switch requires `--accept-legacy-drop`;
- verify gate disappears permanently after successful switch.

### 21.6 Generation/race tests

- changed policy snapshot advances unified generation exactly once;
- byte-identical snapshot does not;
- policy snapshot and generation commit atomically;
- worker obtains lock then reloads new policy rather than using scheduling snapshot;
- policy commit waiting behind prune cannot mutate running prune's policy;
- newer policy debt remains after older work completes when ordering requires it.

### 21.7 Watcher/fast-path tests

- formerly ignored path becomes eligible after update without watcher restart and later modification is not permanently lost;
- fast batch rechecks current committed policy under worker ownership;
- stale/ignored pending events do not upload;
- successful fast upload writes active ledger row before clearing exact pending event;
- crash simulation after upload/before ledger upsert retains event for idempotent repair;
- destructive events continue to promote full reconcile.

### 21.8 Suppression tests

Sequence:

```text
mirror file
commit ignore policy
refresh
```

assert:

- ledger row becomes suppressed;
- remote file remains;
- ordinary reconciliation does not count/delete it;
- prune preview includes it.

Then remove ignore before prune and refresh:

- row reactivates;
- stale old prune request cannot delete it;
- no forced delete/re-upload is required when remote remains valid.

### 21.9 True deletion ordering tests

- delete event durably recorded under active prior policy, then policy excludes path -> preserve ordinary delete evidence;
- policy commit first, then disappearance -> suppression, not ordinary delete;
- catch-up ambiguity/unknown policy context -> suppression fail-safe.

### 21.10 Prune request tests

- preview unavailable before current policy refresh ready;
- preview freezes exact targets and policy hash;
- newer preview supersedes old unexecuted preview;
- policy change marks old request stale;
- unrelated file modification/generation change does not stale request;
- `candidate_count > limit` deletes zero;
- exact/sufficient authorization queues request;
- missing remote target is success;
- retryable error preserves durable approval and retries same request;
- crash after some targets resumes remaining targets;
- completed request compacts target rows but keeps summary;
- prune deletes files only and does not promise empty directory cleanup.

### 21.11 Authorization separation tests

- `reconcile-now --allow-deletes` does not authorize prune;
- prune authorization does not raise ordinary reconcile delete budget;
- failed reconcile retry does not inherit one-attempt manual override;
- failed prune retry does retain its request authorization while policy hash remains valid.

### 21.12 Status tests

Socket-first and SQLite-fallback status both expose durable:

```text
policy hash/source/refresh
suppressed count
latest prune state/count
```

while only live worker snapshots expose ephemeral throughput/stall data as today.

`ignore status` detects modified/unreadable disk snapshot without committing it.

---

## 22. Implementation phases

### Phase 1 — Policy snapshot foundation

1. Add `github.com/git-pkgs/gitignore` dependency if not already present.
2. Add directory-aware filter candidate API.
3. Implement repository-local-only stable `.gitignore` collector using `New("")` + scoped `AddPatterns`.
4. Add policy snapshot/hash state tables and APIs.
5. Add initial policy capture to profile creation.
6. Add matcher/error/status unit tests.

**Exit condition:** profiles can have a durable committed `.gitignore` snapshot independent of live disk contents; runtime does not load global/info Git excludes.

### Phase 2 — Legacy migration and CLI switch

1. Implement exact legacy structured-rule converter with compatibility tests.
2. Migrate profiles into `legacy_migrated` or safe empty `gitignore` policy.
3. Stop accepting new structured exclude mutations.
4. Add `profile ignore update/status`.
5. Add one-time `--accept-legacy-drop` gate.
6. Make changed policy commit atomically create reconciliation debt; byte-identical update is no-op.

**Exit condition:** there is one path-policy matcher and `.gitignore` is the only long-term user editing surface.

### Phase 3 — Worker-owned policy refresh and stale-profile removal

1. Reload profile + committed policy after profile lock acquisition.
2. Remove stale startup policy as watcher event-drop authority or add conservative hash-keyed hot reload.
3. Track policy refresh state/hash.
4. Integrate policy refresh with worker full-debt priority.
5. Add race/debounce/generation tests.

**Exit condition:** committed policy consistently governs worker execution without watcher/worker stale snapshot gaps.

### Phase 4 — Managed ledger

1. Migrate manifest to active/suppressed ledger semantics.
2. Replace blanket manifest refresh with state-aware apply/merge.
3. Add durable event policy-context evidence for deletion vs suppression.
4. Make fast-upsert ledger update a success barrier before event clear.
5. Add suppression/reactivation/crash tests.

**Exit condition:** every managed remote file remains durably known across policy suppression and fast uploads.

### Phase 5 — Ordinary reconciliation protection

1. Add rclone integration spike/tests for protected suppressed paths.
2. Remove policy-suppressed objects from ordinary deletion candidates.
3. Preserve proven local deletion and existing delete-budget semantics.
4. Verify authoritative remote-drift behavior remains intentional.

**Exit condition:** policy changes cannot delete suppressed managed files through ordinary reconciliation.

### Phase 6 — Durable prune

1. Add prune request/target schema and transactional APIs.
2. Implement `prune preview` immutable target snapshot.
3. Implement authorization ceiling and `approval_required`.
4. Add worker prune claim/execution/progress/retry/recovery.
5. Serialize destructive execution with policy commit under profile lock.
6. Compact completed targets while retaining summary.
7. Add stale/superseded/retry/crash tests.

**Exit condition:** suppressed files are deletable only through explicit, bounded, durable, recoverable prune authorization.

### Phase 7 — Status and end-to-end UX

1. Extend live/SQLite status snapshots with policy/prune summaries.
2. Render concise `profile status` fields and detailed `profile ignore status`.
3. Add `profile prune status`.
4. Document reconcile-vs-prune delete authorization distinction.
5. Run full migration and end-to-end suites against rclone local mock remote.

**Exit condition:** users can understand committed-vs-disk policy, refresh state, suppressed count, and destructive approval state without reading SQLite/logs.

---

## 23. Acceptance criteria

The implementation is complete when all of the following are true.

### Policy UX

- User can express wildcard/negation behavior using normal root/nested `.gitignore` files.
- User may make many editor changes and produce one synchronization policy change by running one `profile ignore update`.
- Saving `.gitignore` without `ignore update` does not silently change synchronization policy.
- `ignore status` clearly reports when disk policy differs from committed policy.
- New profiles honor existing `.gitignore` before their first upload.

### Git semantics

- Runtime matcher uses `github.com/git-pkgs/gitignore` semantics.
- Only repository-local `.gitignore` files participate.
- `.git/info/exclude` and global excludes do not affect knowledge-sync.
- No Git repository or Git executable is required at runtime.
- Nested scope and ignored-parent traversal behave like Gitignore.
- Invalid patterns follow dependency behavior and are visible as warnings.

### Migration

- Existing structured excludes do not silently disappear on upgrade.
- Existing structured behavior is preserved through a synthetic Gitignore migration snapshot with compatibility tests.
- First switch from migrated legacy policy to disk `.gitignore` requires explicit confirmation.
- After switch, structured rules no longer form a parallel policy authority.

### Safety

- Committing a new ignore policy never directly authorizes deletion of newly ignored managed files.
- Previously mirrored ignored files become suppressed and remain remotely present during ordinary reconcile.
- Removing the ignore before prune can reactivate them.
- Ambiguous delete-vs-suppress chronology fails safe to suppression.
- Prune cannot run against a policy refresh that is not ready.
- Prune targets exactly the immutable set previewed by the user.
- Policy change makes old unexecuted authorization stale before deletion.
- Insufficient `--allow-deletes` deletes zero files.
- V1 prune does not broadly remove empty directory shells.

### Durability

- Policy snapshot, policy hash, and reconciliation debt commit atomically.
- Fast-uploaded managed objects enter the ledger before their pending events are cleared.
- Prune target progress survives worker restart.
- Retryable prune failures keep valid durable authorization.
- Remote-missing prune targets converge to success.

### Architecture

- Worker remains the only rclone data-plane owner.
- Worker reloads durable profile/policy after acquiring operation ownership.
- Full/policy debt retains priority over fast events.
- Authorized prune is not starved behind an endless stream of fast events.
- Existing one-attempt manual reconcile delete override remains separate from prune approval.
- Socket-first status architecture is extended, not bypassed.

---

## 24. Non-goals for this change

Do not expand scope to:

- automatically committing policy on every `.gitignore` save;
- global Git exclude support;
- `.git/info/exclude` support;
- requiring a `.git/` repository;
- editing the user's `.gitignore` automatically during migration;
- inventing custom wildcard or invalid-pattern rules;
- permanent dual support for structured excludes and Gitignore policy;
- remote directory ownership / recursive empty-directory pruning in V1;
- partial prune batches driven by `--allow-deletes N`;
- a second worker/transfer path in the CLI or watcher;
- long-term per-target audit retention after a completed prune;
- semantic canonicalization of `.gitignore` snapshots.

---

## 25. Likely code areas

Expected implementation touch points include at least:

```text
internal/filter/filter.go
internal/state/migrate.go
internal/state/excludes.go              legacy migration/removal path
internal/state/manifest.go              managed ledger evolution
internal/state/syncstate.go             policy debt integration
internal/state/intent.go                ordinary reconcile intent remains separate
internal/state/*policy*.go              new committed snapshot APIs
internal/state/*prune*.go               new durable prune APIs
internal/sync/scan.go                   directory-aware committed policy matching
internal/sync/reconcile.go              active/suppressed reconciliation semantics
internal/sync/service.go                rclone command composition/protection
internal/cli/profile.go                 ignore CLI + legacy command retirement
internal/cli/worker.go                  owned snapshot reload + scheduling + prune
internal/cli/fastupsert.go              ledger barrier and current policy filter
internal/cli/watch.go
internal/watch/watcher.go               no stale-policy event loss
internal/cli/profilestatus.go
internal/live/snapshot.go               policy/prune durable summary
```

Prefer new focused state files over continuing to grow unrelated modules.

---

## 26. Final invariant summary

After this plan is implemented, the system should obey this compact model:

```text
Disk .gitignore
    is only an editable working copy

`profile ignore update`
    creates one stable durable policy snapshot

Committed policy
    controls eligibility everywhere in the worker

Policy refresh
    classifies managed files as active or suppressed

Suppressed
    means "still owned and protected remotely"
    not "delete now"

Ordinary reconcile
    may delete proven ordinary local deletions within its own budget
    but cannot use policy suppression as deletion authority

`profile prune preview`
    freezes exactly what the user is considering deleting

`profile prune execute`
    authorizes that immutable set under one policy_hash and one safety ceiling

Worker
    is the only process that performs remote data-plane work
```

The resulting behavior should feel Git-native at the policy-editing surface while remaining more conservative than Git/rclone at the destructive boundary: changing ignore policy is cheap and reversible; remote deletion requires a separate explicit, durable authorization.