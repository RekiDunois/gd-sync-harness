#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: scripts/publication-audit.sh [--current|--history|--all] [--denylist FILE]

Audits tracked Git content before changing a repository from private to public.
The script prints locations (paths/commit SHAs), not matching secret values.

Modes:
  --current   scan the current tracked tree only
  --history   scan all reachable refs/history only
  --all       scan both (default)

Optional denylist:
  --denylist FILE
      FILE must live outside the repository. Each non-empty, non-comment line
      is treated as an exact private identifier to locate without printing it.
EOF
}

mode="all"
denylist="${PUBLICATION_DENYLIST_FILE:-}"
while (($#)); do
  case "$1" in
    --current) mode="current" ;;
    --history) mode="history" ;;
    --all) mode="all" ;;
    --denylist)
      shift
      [[ $# -gt 0 ]] || { echo "--denylist requires a file" >&2; exit 2; }
      denylist="$1"
      ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
  shift
done

root="$(git rev-parse --show-toplevel)"
cd "$root"

if [[ -n "$denylist" ]]; then
  denylist="$(cd "$(dirname "$denylist")" && pwd)/$(basename "$denylist")"
  [[ -f "$denylist" ]] || { echo "denylist not found: $denylist" >&2; exit 2; }
  case "$denylist" in
    "$root"/*)
      echo "denylist must be outside the repository so private identifiers are never committed" >&2
      exit 2
      ;;
  esac
fi

# High-confidence credential value shapes. These deliberately avoid generic
# words such as "password" or "token" because project documentation discusses
# those concepts legitimately.
secret_value_re='(AIza[0-9A-Za-z_-]{35}|gh[pousr]_[0-9A-Za-z]{20,}|github_pat_[0-9A-Za-z_]{20,}|AKIA[0-9A-Z]{16}|1//[0-9A-Za-z_-]{20,}|-----BEGIN (RSA |EC |OPENSSH )?PRIVATE KEY-----)'
privacy_re='(/Users/[^/[:space:]]+|/home/[^/[:space:]]+|[A-Za-z0-9._%+-]+@(gmail\.com|icloud\.com|outlook\.com|hotmail\.com))'
credential_file_re='(^|/)(rclone\.conf|credentials\.json|token\.json|client_secret_[^/]+\.json|[^/]*service-account[^/]*\.json|application_default_credentials\.json|[^/]+\.(pem|key|p12|pfx|jks|keystore))$'
privacy_exclude_paths=(':(exclude)AGENTS.md' ':(exclude)AGENT.md' ':(exclude)scripts/publication-audit.sh')

fail=0
warn=0

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

report_current() {
  echo "== current tracked tree =="

  : >"$tmp/credential-files"
  while IFS= read -r f; do
    if printf '%s\n' "$f" | grep -Eq "$credential_file_re"; then
      printf '%s\n' "$f" >>"$tmp/credential-files"
    fi
  done < <(git ls-files)
  if [[ -s "$tmp/credential-files" ]]; then
    echo "BLOCK: credential-like tracked filenames:"
    sed 's/^/  /' "$tmp/credential-files"
    fail=1
  else
    echo "OK: no credential-like tracked filenames"
  fi

  git grep -Il -E "$secret_value_re" -- . >"$tmp/current-secret" 2>/dev/null || true
  if [[ -s "$tmp/current-secret" ]]; then
    echo "BLOCK: high-confidence credential value shape found in tracked files:"
    sed 's/^/  /' "$tmp/current-secret"
    fail=1
  else
    echo "OK: no high-confidence credential value shapes in tracked files"
  fi

  git grep -Il -E "$privacy_re" -- . "${privacy_exclude_paths[@]}" >"$tmp/current-privacy" 2>/dev/null || true
  if [[ -s "$tmp/current-privacy" ]]; then
    echo "WARN: personal path/email shape found in tracked files; review these paths:"
    sed 's/^/  /' "$tmp/current-privacy"
    warn=1
  else
    echo "OK: no obvious personal path/email shapes in tracked files"
  fi

  if [[ -n "$denylist" ]]; then
    : >"$tmp/current-deny"
    while IFS= read -r needle || [[ -n "$needle" ]]; do
      [[ -n "$needle" ]] || continue
      [[ "$needle" == \#* ]] && continue
      git grep -Il -F -- "$needle" -- . >>"$tmp/current-deny" 2>/dev/null || true
    done <"$denylist"
    sort -u "$tmp/current-deny" -o "$tmp/current-deny"
    if [[ -s "$tmp/current-deny" ]]; then
      echo "BLOCK: denylisted private identifier remains in current tracked files:"
      sed 's/^/  /' "$tmp/current-deny"
      fail=1
    else
      echo "OK: no denylisted identifiers in current tracked files"
    fi
  fi
}

report_history() {
  echo "== all reachable Git history/refs =="
  : >"$tmp/history-secret"
  : >"$tmp/history-privacy"
  : >"$tmp/history-deny"
  : >"$tmp/message-secret"
  : >"$tmp/message-privacy"
  : >"$tmp/message-deny"

  while IFS= read -r rev; do
    while IFS= read -r path; do
      [[ -n "$path" ]] && printf '%s:%s\n' "$rev" "$path" >>"$tmp/history-secret"
    done < <(git grep -Il -E "$secret_value_re" "$rev" -- . 2>/dev/null || true)

    while IFS= read -r path; do
      [[ -n "$path" ]] && printf '%s:%s\n' "$rev" "$path" >>"$tmp/history-privacy"
    done < <(git grep -Il -E "$privacy_re" "$rev" -- . "${privacy_exclude_paths[@]}" 2>/dev/null || true)

    msg="$(git show -s --format='%B' "$rev")"
    if printf '%s\n' "$msg" | grep -Eq "$secret_value_re"; then
      printf '%s\n' "$rev" >>"$tmp/message-secret"
    fi
    if printf '%s\n' "$msg" | grep -Eq "$privacy_re"; then
      printf '%s\n' "$rev" >>"$tmp/message-privacy"
    fi

    if [[ -n "$denylist" ]]; then
      while IFS= read -r needle || [[ -n "$needle" ]]; do
        [[ -n "$needle" ]] || continue
        [[ "$needle" == \#* ]] && continue
        while IFS= read -r path; do
          [[ -n "$path" ]] && printf '%s:%s\n' "$rev" "$path" >>"$tmp/history-deny"
        done < <(git grep -Il -F -- "$needle" "$rev" -- . 2>/dev/null || true)
        if printf '%s\n' "$msg" | grep -Fq -- "$needle"; then
          printf '%s\n' "$rev" >>"$tmp/message-deny"
        fi
      done <"$denylist"
    fi
  done < <(git rev-list --all)

  for f in "$tmp"/history-* "$tmp"/message-*; do
    [[ -f "$f" ]] && sort -u "$f" -o "$f"
  done

  if [[ -s "$tmp/history-secret" || -s "$tmp/message-secret" ]]; then
    echo "BLOCK: high-confidence credential shape exists in reachable history."
    [[ -s "$tmp/history-secret" ]] && { echo "  file objects:"; sed 's/^/    /' "$tmp/history-secret"; }
    [[ -s "$tmp/message-secret" ]] && { echo "  commit messages:"; sed 's/^/    /' "$tmp/message-secret"; }
    fail=1
  else
    echo "OK: no high-confidence credential value shapes in reachable history"
  fi

  if [[ -s "$tmp/history-privacy" || -s "$tmp/message-privacy" ]]; then
    echo "WARN: personal path/email shape exists in reachable history."
    [[ -s "$tmp/history-privacy" ]] && { echo "  file objects:"; sed 's/^/    /' "$tmp/history-privacy"; }
    [[ -s "$tmp/message-privacy" ]] && { echo "  commit messages:"; sed 's/^/    /' "$tmp/message-privacy"; }
    warn=1
  else
    echo "OK: no obvious personal path/email shapes in reachable history"
  fi

  if [[ -n "$denylist" ]]; then
    if [[ -s "$tmp/history-deny" || -s "$tmp/message-deny" ]]; then
      echo "WARN: denylisted private identifiers exist in preserved history."
      echo "      This repository policy forbids history rewriting solely for publication,"
      echo "      so these findings require an explicit disclosure/acceptance decision."
      [[ -s "$tmp/history-deny" ]] && { echo "  file objects:"; sed 's/^/    /' "$tmp/history-deny"; }
      [[ -s "$tmp/message-deny" ]] && { echo "  commit messages:"; sed 's/^/    /' "$tmp/message-deny"; }
      warn=1
    else
      echo "OK: no denylisted identifiers in reachable history"
    fi
  fi

  : >"$tmp/non-noreply-authors"
  while IFS=$'\t' read -r rev email; do
    [[ "$email" == *@users.noreply.github.com ]] && continue
    [[ "$email" == noreply@github.com ]] && continue
    printf '%s\n' "$rev" >>"$tmp/non-noreply-authors"
  done < <(git log --all --format='%H%x09%ae')
  sort -u "$tmp/non-noreply-authors" -o "$tmp/non-noreply-authors"
  if [[ -s "$tmp/non-noreply-authors" ]]; then
    echo "WARN: commits with non-noreply author email metadata exist (SHAs only):"
    sed 's/^/  /' "$tmp/non-noreply-authors"
    warn=1
  fi
}

case "$mode" in
  current) report_current ;;
  history) report_history ;;
  all) report_current; echo; report_history ;;
esac

echo
if (( fail )); then
  echo "PUBLICATION AUDIT: BLOCKED"
  exit 1
fi
if (( warn )); then
  echo "PUBLICATION AUDIT: NO CREDENTIAL BLOCKERS FOUND, BUT MANUAL PRIVACY REVIEW REQUIRED"
  exit 3
fi
echo "PUBLICATION AUDIT: PASS"
