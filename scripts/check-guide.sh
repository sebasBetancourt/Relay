#!/usr/bin/env bash
# Follows CONTRIBUTING.md on a clean machine, so the guide cannot go stale.
#
# It runs the commands in the file itself, not a copy of them: each code block
# after a "guide:run" comment runs in order, and one after "guide:serve" runs
# in the background until the API answers /readyz.
#
# Only for CI or a throwaway clone: the guide replaces .env, and this removes
# the Compose volumes when it ends.
set -euo pipefail

cd "$(dirname "$0")/.."

if [ -z "${CI:-}" ]; then
  echo "check-guide: refusing to run without CI=true: it replaces .env and deletes the Compose volumes" >&2
  exit 2
fi

work="$(mktemp -d)"
server=""
cleanup() {
  if [ -n "$server" ]; then
    kill -- "-$server" 2>/dev/null || true
  fi
  docker compose down -v >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT

# Writes each marked block to its own file, named NN-run.sh or NN-serve.sh.
awk -v dir="$work" '
  /^<!-- guide:(run|serve) -->$/ { mode = ($2 == "guide:run") ? "run" : "serve"; next }
  mode != "" && /^```bash$/      { n++; file = sprintf("%s/%02d-%s.sh", dir, n, mode); mode = ""; next }
  file != "" && /^```$/          { close(file); file = ""; next }
  file != ""                     { print > file }
' CONTRIBUTING.md

blocks=("$work"/*.sh)
if [ ! -e "${blocks[0]}" ]; then
  echo "check-guide: no guide blocks found in CONTRIBUTING.md" >&2
  exit 1
fi

for block in "${blocks[@]}"; do
  echo "::group::$(basename "$block")"
  cat "$block"
  case "$block" in
    *-run.sh)
      bash -euo pipefail "$block"
      ;;
    *-serve.sh)
      # setsid gives it a process group: go run starts a child that must die too.
      setsid bash -euo pipefail "$block" &
      server=$!
      for _ in $(seq 1 120); do
        if curl -sf http://localhost:8080/readyz >/dev/null; then
          break
        fi
        if ! kill -0 "$server" 2>/dev/null; then
          echo "check-guide: the API exited before it was ready" >&2
          exit 1
        fi
        sleep 1
      done
      curl -sf http://localhost:8080/readyz >/dev/null
      kill -- "-$server"
      server=""
      ;;
  esac
  echo "::endgroup::"
done

echo "check-guide: followed ${#blocks[@]} blocks of CONTRIBUTING.md"
