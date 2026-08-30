# knowledge-sync implementation plan: Git-compatible exclude policy and deferred prune

## 0. Status

This is an **incremental implementation plan** for the existing `knowledge-sync` codebase.

It addresses three related problems in the current exclude lifecycle:

1. path-based excludes cannot express general wildcard patterns such as matching the same nested path at arbitrary depth;
2. each exclude edit currently requests reconciliation, making several consecutive policy edits look like several synchronization requests;
3. adding excludes can turn already mirrored files into remote deletions immediately, which can collide with `max_delete` even when the user is still editing the policy.

The plan intentionally does **not** invent a new wildcard language. Pattern matching should follow Git ignore semantics and use a Go implementation of Git's matcher.

The core architectural change is:

> Exclude policy changes decide what is eligible for future synchronization immediately, but they do not automatically authorize deletion of already mirrored objects. Existing mirrored objects that become excluded are retained as suppressed managed objects until an explicit prune operation removes them.

This separates:

```text
policy
  -> what should participate in synchronization now

reconciliation
  -> converge active/eligible managed content

prune
  -> explicitly remove previously mirrored content that policy now suppresses
```

---

## 0.1 Design decisions locked by this plan

The following decisions are considered settled unless implementation inspection reveals a concrete incompatibility.

### Pattern semantics

- Add Git-compatible ignore patterns rather than inventing `*` / `**` behavior locally.
- Use `github.com/git-pkgs/gitignore` as the initial matcher dependency.
- Construct the matcher with `gitignore.New("")` and add database-backed patterns programmatically.
- Do not load repository `.gitignore`, `.git/info/exclude`, or global Git excludes implicitly.
- Do not require the user's machine to have Git installed.
- The `git` executable may be used only as an optional development/test oracle through `git check-ignore`.
- Preserve existing structured exclude rule types for backward compatibility.
- Add a new rule type named `exclude_gitignore` for Git-style patterns.

### Policy lifecycle

- A successful exclude-policy edit takes effect immediately for new scans, watcher events, and future transfers.
- Policy edits must be durable and atomically advance the same monotonic generation used to protect reconciliation from stale work.
- Several policy edits inside the existing destructive debounce window coalesce into one follow-up policy refresh/reconciliation opportunity.
- A no-op edit, such as inserting an existing rule or removing a missing rule, must not advance generation.
- A run already in progress may finish against its captured policy snapshot, but a policy edit during that run must leave newer durable debt so the old run cannot consume the configuration change.

### Remote deletion lifecycle

- Adding an exclude does **not** immediately delete an already mirrored remote object.
- Such an object becomes `suppressed`: managed by the profile, ignored for ordinary synchronization, but still known to the durable manifest/ledger.
- Ordinary scheduled reconciliation must protect suppressed objects from deletion.
- Explicit prune is the only normal path that deletes suppressed objects because of an exclude-policy change.
- Prune must be targeted to suppressed managed paths. It must not silently broaden into deletion of unrelated remote drift.
- `max_delete` remains a hard safety boundary.
- A prune exceeding the normal delete budget becomes an approval-required condition, not a terminal synchronization failure.
- A one-shot `--allow-deletes N` override may authorize a larger prune without changing the profile's persistent default.

### Execution ownership

- Preserve the current control-plane / worker-owned execution architecture.
- CLI commands persist intent and may wait/observe; they must not create a second competing transfer path.
- Worker execution must reload the latest profile and exclude rules after acquiring profile ownership and before starting work.

---

## 1. Current implementation summary

The current implementation has four structured exclude types:

```text
exclude_path_prefix
exclude_dir_name
exclude_filename
exclude_extension
```

These are evaluated centrally by `internal/filter/filter.go`, which is already the correct architectural location for eligibility decisions shared by scans and reconciliation.

Some requested wildcard use cases are already representable through structured rules. For example, `exclude_dir_name` can match the same directory name at arbitrary depth, and `exclude_filename` can match the same filename at arbitrary depth. However, arbitrary path patterns such as:

```text
**/generated/cache/
packages/*/build/
a/**/b/
```

cannot be expressed consistently.

The current CLI mutates one exclude row and then calls `RequestReconcile()`. The current reconciliation path uses rclone synchronization with deletion behavior that can remove content which becomes excluded. As a result, a policy edit is coupled to potentially destructive remote convergence.

The current manifest is also replaced from the current eligible local scan after reconciliation. That is incompatible with deferred prune: if a previously mirrored path becomes excluded and is simply omitted from the next manifest refresh, the program loses the durable fact that the remote object is still managed and intentionally awaiting prune.

---

## 2. Git behavior adopted by this design

The pattern language should follow `gitignore(5)` semantics rather than shell glob semantics or Go `filepath.Match` semantics.

Required examples include:

```text
foo/
```

matches directories named `foo` below the policy root, while:

```text
/foo/
```

anchors the match to the profile root.

A single `*` does not cross `/` boundaries in path patterns:

```text
foo/*
```

matches one level under `foo` but not arbitrarily deep descendants.

Git-style `**` supports arbitrary directory depth:

```text
**/foo/
a/**/b
abc/**
```

The matcher must also preserve Git behavior for:

```text
?
[abc]
[a-z]
\ escaping
trailing /
leading /
! negation
last matching rule wins
```

Because `!` and last-match-wins semantics are order-sensitive, Git-style patterns cannot be stored as an unordered set.

---

## 3. Dependency strategy

### 3.1 Runtime dependency

Add:

```text
github.com/git-pkgs/gitignore
```

The application should use only its programmatic matching API:

```go
m := gitignore.New("")
m.AddPatterns(patternBytes, "")
ignored := m.MatchPath(relPath, isDir)
```

Passing an empty root is mandatory for this integration. It prevents the matcher from loading repository/global Git configuration on behalf of `knowledge-sync`.

The application must not call:

```text
git check-ignore
git config
```

at runtime.

Therefore the deployed `knowledge-sync` binary has **no new external Git installation requirement**.

### 3.2 Development conformance testing

Where Git is available in development or CI, tests may compare representative matcher results against:

```bash
git check-ignore
```

These tests are an oracle for compatibility, not a production code path.

Tests must skip cleanly when Git is unavailable.

Add a separate test that runs matcher logic with an empty/controlled `PATH` to prove runtime filter evaluation does not invoke Git.

---

## 4. Filter engine changes

### 4.1 Preserve one eligibility authority

`internal/filter/filter.go` remains the single eligibility authority.

Do not add Git-style matching independently to watcher code, scan code, CLI code, or reconciliation code.

The engine should compile Git-style patterns once when it is constructed and then expose the same eligibility result everywhere.

### 4.2 Add directory awareness

The current filter API does not fully express whether the candidate is a file or directory. Git ignore semantics require this distinction because:

```text
foo
```

and:

```text
foo/
```

are not equivalent.

Change the internal API toward:

```go
Excluded(relPath string, isDir bool) (bool, error)
```

or an equivalent typed candidate API.

All callers must normalize candidate paths to profile-root-relative forward-slash paths before matching.

Directory walkers should use the result to prune excluded directory traversal with `filepath.SkipDir` where appropriate.

### 4.3 Keep non-pattern eligibility separate

Gitignore matching handles path policy only.

Existing independent eligibility rules such as maximum file size remain explicit checks in `filter.Engine`. Do not encode non-path policy into fake Gitignore patterns.

### 4.4 Legacy structured rules

Existing rules remain valid:

```text
exclude_path_prefix
exclude_dir_name
exclude_filename
exclude_extension
```

Migration must not require users to rewrite them.

The filter engine may evaluate legacy rules alongside the Git matcher. A later cleanup can translate simple legacy rules internally, but such translation is not required for this change.

---

## 5. Ordered Git-style rule storage

### 5.1 Why ordering is required

The current `profile_excludes` primary key models rules as an unordered unique set. Git-style patterns require deterministic order because the last matching rule wins and `!` can re-include a previously excluded path.

For example:

```text
*.tmp
!important.tmp
```

is not equivalent to the reverse order.

### 5.2 Schema change

Add a stable ordering field for policy rules, for example:

```text
rule_order INTEGER NOT NULL
```

The exact migration may either extend `profile_excludes` or rebuild it with an explicit row identifier and order field.

Required invariants:

- existing structured rules migrate without behavior change;
- Git-style patterns are returned in stable policy order;
- duplicate identical rows remain unnecessary;
- insertion of a new Git-style pattern appends it after existing patterns unless an explicit ordering feature is later added;
- removal is deterministic;
- list/show output exposes the effective ordering.

The first version does not need interactive arbitrary reordering if remove + add can express the intended order, but the storage model must not prevent future reordering.

---

## 6. Atomic policy mutation and generation semantics

### 6.1 Fix the current stale-run race first

Today an exclude edit can request reconciliation without necessarily creating a strictly newer generation than an already-running attempt. A worker may also hold a stale `Profile` snapshot that predates the edit.

This creates a correctness risk where an older run succeeds and clears debt even though filter policy changed while it was running.

The fix is mandatory before broad wildcard rules are enabled because one pattern may affect thousands of paths.

### 6.2 One monotonic input generation

Do not introduce an independent policy counter that can collide with filesystem generations.

Generalize the existing per-profile monotonic generation so every input capable of changing desired mirror state advances the same sequence:

```text
filesystem change
exclude-policy change
future sync-affecting profile policy change
        |
        v
single monotonic profile input generation
```

For migration minimization, the physical SQLite column currently named `source_generation` may remain temporarily, but code should stop assuming it means only filesystem events. New APIs and comments should treat it as the profile's input/change generation.

### 6.3 Transactional policy API

Replace the CLI sequence:

```text
write one exclude row
RequestReconcile
```

with an atomic state-layer operation such as:

```go
ApplyExcludeChanges(profileID, changes) (generation int64, changed bool, err error)
```

Inside one SQLite transaction:

1. apply all requested add/remove operations;
2. determine whether effective policy changed;
3. if unchanged, commit without generation advancement;
4. if changed, increment the profile input generation exactly once for the transaction;
5. advance `desired_generation` to at least that generation;
6. persist the appropriate policy-refresh/reconciliation intent;
7. update the existing debounce timestamps;
8. commit.

A crash must never be able to persist the rule change without also persisting the newer reconciliation generation.

### 6.4 Batch edits

The state API must accept multiple changes in one transaction even if the first CLI version still exposes one-rule commands.

An ergonomic batch CLI can then be added without changing state semantics, for example:

```bash
knowledge-sync profile exclude notes-main \
  --add 'exclude_gitignore:**/.cache/' \
  --add 'exclude_gitignore:**/build/' \
  --add 'exclude_gitignore:*.tmp'
```

All three changes above should produce one generation advancement and one eventual policy refresh.

Separate single-rule CLI invocations should still coalesce through the durable debounce window.

---

## 7. Reconciliation intent and debounce

### 7.1 Policy refresh is not destructive full sync

A policy-only change should schedule a durable **policy refresh**, not immediately request an ordinary destructive full reconciliation.

A policy refresh may require a full local scan so the system can discover newly eligible files after a rule removal or a negation rule, but it must not automatically remove remote content only because it is now excluded.

The distinction is:

```text
full local scan
    !=
full destructive remote sync
```

### 7.2 Coalescing

Reuse the existing durable debounce behavior:

```text
quiet period: approximately 10 seconds
maximum delay: approximately 60 seconds
```

Repeated policy changes push the quiet boundary but do not move the deadline indefinitely.

The worker should process the **final effective policy** once the debounce becomes eligible.

### 7.3 Intent merging

If several reasons for reconciliation are pending, merge them conservatively.

At minimum distinguish:

```text
policy refresh
ordinary reconciliation
explicit prune
```

A normal reconciliation may include a policy refresh, but it must still protect suppressed paths. Prune authorization must never be implied merely because another reconciliation is running.

Implementation may represent this with a durable requested-mode field or flags. The exact representation is less important than the invariant that prune authorization is separate.

---

## 8. Manifest evolution: active vs suppressed managed objects

### 8.1 Current problem

The current manifest is replaced from the currently eligible local scan.

That makes this sequence unsafe:

```text
file was mirrored
        |
add exclude rule
        |
file disappears from eligible scan
        |
replace manifest
        |
program forgets that the remote file still exists and is managed
```

Deferred prune therefore requires a durable managed-object state.

### 8.2 New manifest semantics

Evolve the manifest from a pure "current eligible local snapshot" into a lightweight managed mirror ledger.

Each row should have at least an effective state such as:

```text
active
suppressed
```

Suggested additive metadata:

```text
state                 TEXT NOT NULL DEFAULT 'active'
suppressed_generation INTEGER NULL
```

Exact naming may differ.

Definitions:

- `active`: the path is currently eligible and is part of ordinary synchronization;
- `suppressed`: the path was previously managed/mirrored, currently matches exclude policy, and is intentionally protected until prune.

Existing manifest rows migrate as `active`.

### 8.3 Policy refresh classification

During a policy refresh:

- an existing managed path that still exists locally but is now excluded becomes `suppressed`;
- an eligible path becomes/remains `active`;
- a previously suppressed path that becomes eligible again is reactivated;
- a true local deletion must remain distinguishable from policy suppression;
- suppressed rows must not be removed by a blanket `ManifestReplaceAll` operation.

Replace the current whole-table refresh behavior with state-aware merge/apply operations.

### 8.4 Removing an exclude before prune

This case must be safe and idempotent:

```text
mirrored file
  -> add exclude
  -> state = suppressed
  -> remove exclude before prune
  -> local file still exists
  -> state = active again
  -> ordinary synchronization resumes
```

No remote delete/re-upload cycle should be required if the remote object remained present and unchanged.

---

## 9. Ordinary reconciliation after deferred-prune support

### 9.1 Required invariant

Normal reconciliation must converge active content **without deleting suppressed content**.

Therefore the current unconditional relationship:

```text
excluded from source list
        -> --delete-excluded
        -> delete from remote
```

must be removed for policy-suppressed objects.

### 9.2 Keep rclone as the data mover

Do not implement a custom file-transfer engine.

Rclone remains responsible for copying/checking/deleting remote objects. The application is responsible for supplying the correct desired/protected path sets and for enforcing the delete budget.

### 9.3 Transport implementation spike

Before changing the reconciliation transport, add focused integration tests for rclone filter behavior and choose the simplest command composition that satisfies all of these invariants simultaneously:

1. active eligible files are copied/updated;
2. true managed local deletions can still be removed subject to delete budget;
3. ordinary remote drift can still be handled according to existing authoritative reconciliation semantics;
4. suppressed paths are protected during ordinary reconciliation;
5. policy-excluded local files are not uploaded;
6. prune can later delete only suppressed managed paths.

If rclone `sync` filters can express an explicit protected suppressed set without weakening authoritative deletion semantics, retain `sync`.

If they cannot, use rclone as two operations rather than reproducing its transfer engine:

```text
rclone copy/check for active desired paths
+
targeted rclone delete for application-approved deletion paths
```

The selected implementation must be proven by integration tests before removing the current `--delete-excluded` behavior.

Semantics in this plan take priority over preserving one particular rclone subcommand.

---

## 10. Explicit prune lifecycle

### 10.1 Purpose

Prune means:

> Remove remote objects that are still known as managed by this profile but are currently `suppressed` by the final effective exclude policy.

It does not mean "delete every unexpected object on the remote".

### 10.2 Candidate set

The prune candidate set is derived from suppressed managed manifest entries.

Do not derive prune candidates solely from a fresh remote-vs-source sync dry-run, because that could mix:

```text
policy-suppressed managed objects
true local deletions
unrelated remote drift
```

The explicit prune operation is authorization for the first category only.

### 10.3 Preview

Provide a preview/dry-run path that reports at least:

```text
suppressed managed objects
remote deletions required/observed
persistent delete budget
requested one-shot override, if any
```

Use synthetic output in documentation, for example:

```text
suppressed managed objects: 1,842
remote deletions required:  1,840
delete budget:              100
status:                     approval_required
```

A remote object already missing should not make prune fail; prune is idempotent.

### 10.4 Authorization

Without an override:

```text
required deletions <= profile.max_delete
    -> prune may proceed

required deletions > profile.max_delete
    -> no destructive transfer starts
    -> return/report approval_required
```

With:

```bash
--allow-deletes N
```

the effective budget for that one prune request becomes `N`.

The override must not persist into later operations.

### 10.5 Worker ownership and durability

Prefer prune as durable worker-owned intent rather than a transfer executed directly in the CLI process.

The state model may use a dedicated prune request record or equivalent durable fields containing:

```text
profile_id
policy/input generation being pruned
requested_at
one-shot delete allowance
status
candidate/deleted counts
last error
```

Useful statuses include:

```text
pending
running
approval_required
succeeded
failed
```

An approval-required prune is not a profile synchronization terminal error.

### 10.6 Commit ordering

For each successfully removed remote path:

1. perform/confirm remote deletion;
2. only then remove or finalize the corresponding suppressed manifest entry.

A crash partway through prune must be safe to retry. Already absent remote objects are treated as successfully converged and their suppressed rows may be cleared during retry.

---

## 11. CLI behavior

### 11.1 Backward compatibility

Preserve the existing structured-rule command behavior.

The minimum new syntax can remain compatible with the existing shape:

```bash
knowledge-sync profile exclude notes-main exclude_gitignore '**/.cache/'
knowledge-sync profile exclude notes-main exclude_gitignore '*.tmp'
knowledge-sync profile exclude notes-main exclude_gitignore '!important.tmp'
```

Removal should continue to support the existing remove mechanism until/unless the command hierarchy is reorganized.

Shell-sensitive patterns must be shown quoted in examples.

### 11.2 Listing

Profile/exclude listing should show Git-style rules in effective order.

Example:

```text
1  exclude_gitignore  *.tmp
2  exclude_gitignore  !important.tmp
3  exclude_gitignore  **/.cache/
```

The UI should make ordering visible because ordering changes meaning.

### 11.3 Prune command

Add an explicit prune command under the profile/exclude area. Exact Cobra nesting can follow the least disruptive existing CLI structure.

Required capabilities:

```text
preview / --dry-run
one-shot --allow-deletes N
optional --wait using existing worker observation patterns
```

Example intent:

```bash
knowledge-sync profile prune-excluded notes-main --dry-run
knowledge-sync profile prune-excluded notes-main --allow-deletes 2000 --wait
```

If a more natural `profile exclude prune` hierarchy can be introduced without breaking existing command parsing, it is acceptable, but command naming must not drive the state architecture.

---

## 12. Worker and stale-profile handling

The worker currently enumerates profiles and can hold a profile snapshot while acquiring ownership and claiming a run.

After this change:

1. enumerate candidate profile IDs;
2. acquire the per-profile lock/ownership;
3. claim the durable run/operation;
4. reload the current profile configuration and excludes;
5. build the filter engine from that current snapshot;
6. execute against the run's captured target generation.

If policy changes after step 4:

```text
input generation advances
> current run target generation
```

so successful completion of the current run cannot clear the newer debt.

This invariant must have a regression test.

---

## 13. Status and observability

Expose policy/prune state without making ordinary profile health look failed.

Useful status fields include:

```text
policy generation / desired generation
policy refresh pending
suppressed managed object count
prune status
prune candidate count
prune approval-required count/budget
```

A profile may legitimately be:

```text
sync state: ready
suppressed objects: 1,842
prune: not requested
```

This is not an error. It means active content is synchronized and intentionally excluded legacy content remains remotely retained.

Similarly:

```text
sync state: ready
prune: approval_required
required deletions: 1,842
delete budget: 100
```

should not be represented as a generic terminal reconciliation failure.

---

## 14. Migration and compatibility

### 14.1 Database migration

Add a new schema migration that performs all required additive/rebuild changes atomically.

Likely changes include:

- ordered exclude rule metadata;
- manifest active/suppressed state;
- optional durable reconciliation-mode/policy-refresh metadata;
- durable prune request/status metadata if implemented as a separate table.

Existing profiles and manifest rows must remain valid after in-place upgrade.

### 14.2 Existing excludes

No existing structured exclude may change meaning after migration.

Existing rules do not require meaningful order among themselves because they are currently combined as exclusions. Assign them deterministic migration order only so the table remains stable.

### 14.3 Existing manifest

All existing manifest entries begin as:

```text
state = active
```

Do not infer suppressed rows during migration solely from current exclude rules unless the migration can prove the remote object is managed and still present. It is safer for the first post-upgrade policy refresh/reconciliation to classify state deliberately.

### 14.4 No Git installation migration

Upgrade/install scripts must not add Git as a runtime prerequisite.

---

## 15. Implementation phases

### Phase 1 — matcher dependency and compatibility spike

- add `github.com/git-pkgs/gitignore`;
- integrate `gitignore.New("")` in a focused filter prototype;
- add representative Git-pattern unit tests;
- add optional `git check-ignore` differential tests;
- prove filter operation succeeds with Git absent from `PATH`;
- add rclone filter/deletion integration tests needed to choose the suppression-safe reconciliation composition.

Exit criterion:

> Git-style matching behavior and rclone suppression strategy are proven by tests before persistent behavior changes are enabled.

### Phase 2 — filter API and ordered rules

- add `exclude_gitignore`;
- add ordered policy storage;
- update `filter.Engine` for compiled Git matcher plus legacy rules;
- pass `isDir` through all filter callers;
- normalize paths consistently;
- update profile/exclude listing.

Exit criterion:

> Git-compatible patterns work across scans without changing existing structured-rule behavior.

### Phase 3 — atomic policy generations and debounce

- add `ApplyExcludeChanges` transactional state API;
- advance one monotonic input generation for an effective transaction;
- persist policy-refresh intent atomically;
- reuse durable debounce;
- reload profile/filter state after worker ownership;
- add active-run race regression tests.

Exit criterion:

> Multiple edits cannot be swallowed by an old run and coalesce into one follow-up policy opportunity.

### Phase 4 — suppression-aware manifest

- migrate manifest rows to active/suppressed state;
- replace blanket manifest replacement with state-aware merge;
- classify newly excluded managed paths as suppressed;
- reactivate suppressed paths when policy permits them again;
- ensure ordinary local deletion remains distinguishable from suppression.

Exit criterion:

> The system retains durable knowledge of remote managed objects awaiting prune.

### Phase 5 — suppression-safe reconciliation

- implement the rclone command composition selected in Phase 1;
- remove the behavior that ordinary exclusion automatically deletes suppressed objects;
- preserve current copy/check, source-stability, remote-ownership, and delete-budget safety invariants;
- prove scheduled reconciliation leaves suppressed remote content intact.

Exit criterion:

> Active mirror convergence works while suppressed objects remain protected.

### Phase 6 — durable explicit prune

- add prune intent/state;
- add preview/dry-run;
- enforce persistent and one-shot delete budgets;
- run prune through worker ownership;
- clear suppressed rows only after successful/confirmed remote deletion;
- add status output and optional wait behavior.

Exit criterion:

> A user can make many exclude edits, inspect the final cleanup set once, authorize once, and converge safely.

### Phase 7 — CLI polish and documentation

- add batch edit support if not already included;
- document Git pattern examples;
- document that Git is not a runtime dependency;
- document suppress-vs-prune lifecycle;
- document max-delete approval flow.

---

## 16. Test plan

### 16.1 Gitignore semantics

Cover at least:

```text
foo/
/foo/
**/foo/
*.tmp
foo/*
a/**/b
abc/**
file?.md
[a-z].txt
\!literal
\#literal
*.tmp + !important.tmp ordering
```

Test files and directories separately.

### 16.2 Legacy compatibility

Verify unchanged behavior for:

```text
exclude_path_prefix
exclude_dir_name
exclude_filename
exclude_extension
```

### 16.3 Runtime independence from Git

- run matcher/filter tests with Git unavailable from `PATH`;
- confirm normal filtering does not spawn a Git process;
- optional oracle tests skip when Git is absent.

### 16.4 Generation and concurrency

Regression cases:

```text
run N starts with policy A
policy B is committed while N is active
policy transaction advances generation to N+1 or later
run N succeeds
newer debt remains
worker later runs against policy B
```

Also test:

- duplicate add -> no generation change;
- remove missing rule -> no generation change;
- batch of three effective edits -> one generation increment;
- several separate edits inside debounce -> one eligible follow-up run after quiet period.

### 16.5 Suppression lifecycle

Test:

```text
active mirrored path
 -> add matching exclude
 -> suppressed
 -> scheduled reconcile
 -> remote still exists
 -> remove exclude before prune
 -> active again
```

And:

```text
active mirrored path
 -> add matching exclude
 -> suppressed
 -> prune
 -> remote removed
 -> manifest row finalized/removed
```

### 16.6 Delete budget

Test:

- prune below persistent budget succeeds;
- prune above persistent budget starts no deletion and reports approval required;
- sufficient one-shot override succeeds;
- insufficient one-shot override still blocks;
- override does not persist;
- partial/crashed prune retries idempotently;
- missing remote candidate is treated as already converged.

### 16.7 Ordinary reconciliation protection

Test that:

- active local deletion retains existing delete-budget behavior;
- suppressed paths are not deleted by scheduled/full reconciliation;
- policy-excluded local files are not uploaded;
- remote drift handling remains consistent with the chosen authoritative semantics;
- explicit prune deletes only suppressed managed paths, not unrelated remote extras.

---

## 17. Expected file-level changes

Primary files likely affected:

```text
go.mod
go.sum

internal/filter/filter.go
internal/filter/filter_test.go

internal/state/excludes.go
internal/state/manifest.go
internal/state/migrate.go
internal/state/migrate_test.go
internal/state/syncstate.go
internal/state/events.go

internal/cli/profile.go
internal/cli/profilestatus.go
internal/cli/reconcile.go
internal/cli/worker.go

internal/sync/scan.go
internal/sync/reconcile.go
internal/sync/service.go
```

New focused files are preferable where prune state or policy mutation logic would otherwise make existing files too broad, for example:

```text
internal/state/prune.go
internal/state/policy.go
internal/cli/prune.go
```

Exact placement should follow existing package responsibilities.

---

## 18. Acceptance criteria

The implementation is complete when all of the following are true:

1. users can add Git-style patterns without installing Git;
2. `*`, `**`, directory-only, root-anchored, and ordered negation behavior matches Git for the supported pattern set;
3. existing structured exclude rules continue to work;
4. policy edits are atomic with generation advancement;
5. a policy edit during an active run cannot be cleared by that stale run;
6. several consecutive edits coalesce instead of causing several destructive full synchronizations;
7. an added exclude stops future synchronization of matching content promptly;
8. already mirrored matching content becomes suppressed instead of being immediately deleted;
9. scheduled/ordinary reconciliation preserves suppressed remote objects;
10. removing an exclude before prune reactivates matching local content without forced delete/re-upload;
11. explicit prune previews the final suppressed deletion set;
12. prune honors `max_delete` and supports one-shot authorization;
13. exceeding the prune budget is reported as approval required rather than terminal sync failure;
14. prune is retry-safe after interruption;
15. status clearly distinguishes healthy synchronization from pending suppressed cleanup;
16. no runtime code path depends on the `git` executable;
17. all new examples and tests use synthetic repository-safe data.

---

## 19. Resulting user model

After this change, users should be able to reason about excludes as follows:

```text
1. Edit ignore policy freely.

   '*.tmp'
   '**/.cache/'
   '**/generated/'
   '!important.tmp'

2. knowledge-sync immediately stops treating newly excluded paths as active sync content.

3. Several edits settle into one coalesced policy refresh rather than several destructive full syncs.

4. Previously mirrored objects that are now excluded remain safely retained as suppressed managed objects.

5. Inspect cleanup once after the final policy is ready.

6. If the deletion count is acceptable, prune once.

7. If it exceeds max_delete, explicitly authorize the required one-shot budget and prune.
```

The important separation is:

```text
exclude policy change
    -> eligibility decision
    -> no implicit deletion authorization

prune
    -> explicit destructive intent
    -> preview
    -> delete-budget gate
    -> worker-owned deletion
```

This preserves familiar Git-style pattern semantics, avoids a custom wildcard engine, prevents repeated destructive reconciliation while policy is being edited, and keeps the existing deletion safety boundary meaningful.