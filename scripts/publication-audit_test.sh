#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
AUDIT_SOURCE=$SCRIPT_DIR/publication-audit.sh
repo=
output=
status=0

fail() {
  printf '%s\n' "FAIL: $*" >&2
  exit 1
}

new_repo() {
  if [ -n "$repo" ]; then
    rm -rf "$repo"
  fi
  repo=$(mktemp -d "${TMPDIR:-/tmp}/publication-audit-test.XXXXXX")
  mkdir -p "$repo/scripts"
  cp "$AUDIT_SOURCE" "$repo/scripts/publication-audit.sh"
  chmod 755 "$repo/scripts/publication-audit.sh"
  git -C "$repo" init -q
  git -C "$repo" config user.name "Synthetic Test"
  git -C "$repo" config user.email "synthetic@example.invalid"
  printf '%s\n' 'safe fixture' >"$repo/README.md"
  git -C "$repo" add README.md scripts/publication-audit.sh
  git -C "$repo" commit -qm "Add synthetic audit fixture"
}

cleanup() {
  if [ -n "$repo" ]; then
    rm -rf "$repo"
  fi
}
trap cleanup EXIT HUP INT TERM

run_audit() {
  set +e
  output=$(cd "$repo" && "$repo/scripts/publication-audit.sh" "$@" 2>&1)
  status=$?
  set -e
}

fail_if_status_not() {
  [ "$status" -eq "$1" ] || fail "expected exit $1, got $status\n$output"
}

expect_output() {
  printf '%s\n' "$output" | grep -Fq -- "$1" || fail "missing output $1\n$output"
}

expect_not_output() {
  if printf '%s\n' "$output" | grep -Fq -- "$1"; then
    fail "unexpected output $1\n$output"
  fi
}

new_repo
run_audit --current
fail_if_status_not 0
expect_output "PUBLICATION AUDIT: PASS"

new_repo
printf '%s\n' 'synthetic credential fixture' >"$repo/credentials.json"
git -C "$repo" add credentials.json
git -C "$repo" commit -qm "Add filename fixture"
run_audit --current
fail_if_status_not 1
expect_output "BLOCK: credential-like tracked filenames"

new_repo
secret=AIza
i=0
while [ "$i" -lt 35 ]; do
  secret=${secret}A
  i=$((i + 1))
done
printf '%s\n' "$secret" >"$repo/history.txt"
git -C "$repo" add history.txt
git -C "$repo" commit -qm "Add history fixture"
rm "$repo/history.txt"
git -C "$repo" add -u
git -C "$repo" commit -qm "Remove history fixture"
run_audit --history
fail_if_status_not 1
expect_output "BLOCK: high-confidence credential shape exists in reachable history"
expect_not_output "$secret"

new_repo
marker=synthetic-private-marker
printf '%s\n' "$marker" >"$repo/marker.txt"
git -C "$repo" add marker.txt
git -C "$repo" commit -qm "Add denylist fixture"
denylist=$(mktemp "${TMPDIR:-/tmp}/publication-denylist.XXXXXX")
printf '%s\n' "$marker" >"$denylist"
run_audit --current --denylist "$denylist"
rm -f "$denylist"
fail_if_status_not 1
expect_output "BLOCK: denylisted private identifier remains in current tracked files"
expect_output "marker.txt"
expect_not_output "$marker"

new_repo
email=someone@
email=${email}gmail.com
printf '%s\n' "$email" >"$repo/review.txt"
git -C "$repo" add review.txt
git -C "$repo" commit -qm "Add privacy warning fixture"
run_audit --current
fail_if_status_not 3
expect_output "PUBLICATION AUDIT: NO CREDENTIAL BLOCKERS FOUND, BUT MANUAL PRIVACY REVIEW REQUIRED"
expect_not_output "$email"

printf '%s\n' 'publication audit regression tests: PASS'
