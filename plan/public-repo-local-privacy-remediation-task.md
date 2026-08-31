# Public repository privacy remediation — local agent task

## Goal

Prepare the existing `RekiDunois/gd-sync-harness` repository for public visibility **without rewriting Git history and without losing existing commits or pull-request records**.

The GitHub-side hardening branch already handles current-tree changes that can be applied safely without local Git. This task covers checks that require a complete local clone / Git object database.

## Non-negotiable constraints

1. Do **not** use `git filter-repo`, BFG, `git replace`, history rebase, orphan branches, force-push rewrites, or any operation that changes existing commit SHAs.
2. Do **not** delete/recreate the GitHub repository.
3. Preserve all existing branches, commit ancestry, PR records, review comments, and Actions history.
4. Never commit real credentials, private identifiers, or a denylist containing those identifiers.
5. Drive Canary uses a dedicated test Google account and GitHub Actions secrets. Treat the secret itself as safe-by-design, but still review logs/configuration for accidental secret-derived output.

## 1. Run the repository audit

From the repository root:

```bash
chmod +x scripts/publication-audit.sh
scripts/publication-audit.sh --all
```

For project-specific identifiers that generic regexes cannot recognize, create a denylist **outside the repository**, for example:

```bash
cat > /tmp/gd-sync-private-identifiers.txt <<'EOF'
# One exact private identifier per line.
# Add real profile IDs, real rclone remote aliases, private Drive folder names,
# personal filesystem path fragments, account labels, hostnames, etc.
EOF

scripts/publication-audit.sh --all --denylist /tmp/gd-sync-private-identifiers.txt
```

Do not paste the denylist into issues, PRs, commit messages, CI output, or agent reports.

## 2. Run dedicated secret scanners locally

Install/run at least one well-maintained scanner against both the current tree and history. Prefer `gitleaks`; `trufflehog` is a useful second opinion.

Example with gitleaks:

```bash
gitleaks git --redact --no-banner --exit-code 1 .
```

If the installed gitleaks version uses older CLI syntax, adapt the command but keep these requirements:

- scan full Git history, not only the working tree;
- redact secret values from output;
- save only sanitized findings;
- do not add broad allowlists merely to make the scan green.

Any actual credential found in history must be considered compromised and rotated even though history is intentionally preserved.

## 3. Inspect all refs and unreachable-but-present objects

Because the repository will become public in place, verify all existing refs, not only `master`:

```bash
git for-each-ref --format='%(refname) %(objectname)'
git fsck --full --no-reflogs --unreachable
```

Review local branches, remote-tracking branches, tags, stash refs, notes, and other custom refs. Remove obsolete local-only refs before pushing only when doing so does not alter GitHub PR/history requirements.

For unreachable objects, determine whether they exist only in the local clone or are reachable from a GitHub ref. Do not assume local garbage collection changes what GitHub retains.

## 4. Review preserved historical disclosure

History is intentionally not rewritten. Therefore old commits can continue to expose non-secret environment metadata such as former profile names, rclone remote aliases, operational examples, incident narratives, or author email metadata.

Classify every historical finding into one of these buckets:

- **Credential/secret** — rotate/revoke immediately; public release is blocked until resolved.
- **Sensitive personal identifier** — decide whether disclosure is acceptable. If not acceptable, the constraint of preserving SHAs conflicts with publication and must be escalated rather than silently rewritten.
- **Low-risk historical implementation detail** — document/accept explicitly.
- **Synthetic/test data** — no action if clearly non-personal and non-production.

Produce a short sanitized report containing commit SHAs and categories only. Do not reproduce the sensitive values in that report.

## 5. Verify current tree contains only synthetic documentation examples

Search the current branch after merging the GitHub hardening PR. At minimum inspect all Markdown, YAML, JSON, shell, Go tests/fixtures, and sample configs.

Use the external denylist for exact private names:

```bash
scripts/publication-audit.sh --current --denylist /tmp/gd-sync-private-identifiers.txt
```

Any current-tree hit must be replaced with obviously synthetic values such as:

- `notes-main`
- `example-profile`
- `example-drive-primary`
- `~/ExampleVault/Main`
- `Knowledge Mirror/Notes`
- clearly synthetic counts/quota values

Do not sanitize code identifiers that are part of the public product/API unless they encode a real private deployment value.

## 6. Drive Canary review

The Canary may remain enabled because it uses a dedicated test Google account and secret-backed rclone config, provided all of the following hold:

- the Google account contains no personal/user production data;
- the configured Drive root is dedicated to CI;
- remote/folder/account identifiers are generic test identifiers;
- fork PRs cannot access the secret;
- logs do not print the decoded config, access/refresh tokens, OAuth client secret, account email, or secret-derived identifiers that should remain private;
- failure diagnostics are safe to expose publicly.

Inspect recent failed/successful runs manually on GitHub before changing visibility. If any old log contains an actual credential, rotate/revoke it before publication. If it only contains test-account operational metadata, decide explicitly whether that metadata is acceptable to publish.

## 7. Commit author email decision

Existing commits contain non-noreply author metadata. Rewriting this would change SHAs, so do not rewrite it under the current constraint.

Before publication, explicitly decide whether the already-recorded author email is acceptable as public metadata. For future commits, configure a GitHub noreply address if desired:

```bash
git config user.email '<github-noreply-address>'
```

Do not fabricate an address; copy the exact noreply address from GitHub account email settings.

## 8. Final publication gate

Do not change repository visibility until all of these are true:

- current-tree audit passes with the external denylist;
- full-history secret scanner reports no live credentials;
- any historical credential ever found has been rotated/revoked;
- historical non-secret disclosures have been reviewed and accepted;
- current docs/examples are synthetic;
- all credential/config patterns are covered by `.gitignore`;
- Drive Canary public-log exposure has been reviewed;
- author email exposure has been consciously accepted or otherwise resolved without rewriting history.

After that, make the visibility change through GitHub settings/API while retaining the same repository, branches, commits, and PR records.
