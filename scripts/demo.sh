#!/usr/bin/env bash
# Relay in five minutes: create a tenant, connect Google and/or Outlook,
# list the accounts and read their events.
#
#   docker compose up -d --build   # or: go run ./cmd/api
#   ./scripts/demo.sh
#
# Environment:
#   RELAY_URL  API base URL (default http://localhost:8080)
#   RELAY_KEY  use an existing API key instead of creating a tenant
set -euo pipefail

RELAY_URL="${RELAY_URL:-http://localhost:8080}"
KEY_FILE="demo.key"

bold() { printf '\n\033[1m%s\033[0m\n' "$*"; }
info() { printf '  %s\n' "$*"; }
fail() { printf '\033[31merror:\033[0m %s\n' "$*" >&2; exit 1; }

for cmd in curl jq; do
  command -v "$cmd" >/dev/null || fail "$cmd is required"
done

bold "1. Is Relay up?"
curl -fsS "$RELAY_URL/readyz" >/dev/null || fail "Relay is not ready at $RELAY_URL (docker compose up -d --build)"
info "ready at $RELAY_URL"

bold "2. A tenant and its API key"
if [ -n "${RELAY_KEY:-}" ]; then
  info "using RELAY_KEY from the environment"
elif [ -s "$KEY_FILE" ]; then
  RELAY_KEY="$(cat "$KEY_FILE")"
  info "reusing the key in $KEY_FILE"
else
  # The key goes to stdout only; it is shown once and Relay keeps just its hash.
  if docker compose ps --status running api >/dev/null 2>&1 && [ -n "$(docker compose ps -q --status running api)" ]; then
    # </dev/null: `exec -T` would otherwise swallow the user's keyboard input.
    RELAY_KEY="$(docker compose exec -T api relayctl tenant create --name "Demo $(date +%H:%M:%S)" </dev/null 2>/dev/null)"
  else
    RELAY_KEY="$(go run ./cmd/relayctl tenant create --name "Demo $(date +%H:%M:%S)" </dev/null 2>/dev/null)"
  fi
  [ -n "$RELAY_KEY" ] || fail "could not create a tenant"
  umask 077 && printf '%s\n' "$RELAY_KEY" > "$KEY_FILE"
  info "created; key saved to $KEY_FILE (gitignored)"
fi
AUTH="Authorization: Bearer $RELAY_KEY"
curl -fsS -H "$AUTH" "$RELAY_URL/v1/me" | jq -r '"  tenant: \(.name) (\(.key_prefix)…)"'

bold "3. Connect accounts"
connected=0
for provider in google microsoft; do
  resp="$(curl -sS -H "$AUTH" -H 'Accept: application/json' "$RELAY_URL/v1/connect/$provider")"
  url="$(jq -r '.authorization_url // empty' <<<"$resp")"
  if [ -z "$url" ]; then
    info "$provider: not configured on this deployment ($(jq -r '.error' <<<"$resp")), skipping"
    continue
  fi
  printf '\n  Open this URL, sign in to %s and accept:\n\n  %s\n\n' "$provider" "$url"
  read -r -p "  Press Enter when the browser shows \"connected\" (or type s to skip) " answer || answer=s
  [ "$answer" = "s" ] || connected=$((connected + 1))
done
[ "$connected" -gt 0 ] || info "no account connected; existing ones (if any) are listed below"

bold "4. Connected accounts"
accounts="$(curl -fsS -H "$AUTH" "$RELAY_URL/v1/accounts")"
jq -r '.accounts[] | "  \(.provider)\t\(.status)\t\(.email // "-")\t\(.id)"' <<<"$accounts"
[ "$(jq '.accounts | length' <<<"$accounts")" -gt 0 ] || { info "none yet"; exit 0; }

bold "5. Next events (up to 5 per account, same shape for every provider)"
for id in $(jq -r '.accounts[] | select(.status == "active") | .id' <<<"$accounts"); do
  provider="$(jq -r --arg id "$id" '.accounts[] | select(.id == $id) | .provider' <<<"$accounts")"
  printf '\n  %s (%s)\n' "$provider" "$id"
  resp="$(curl -sS -H "$AUTH" "$RELAY_URL/v1/calendars/$id/events?max_results=5")"
  if jq -e '.events' >/dev/null 2>&1 <<<"$resp"; then
    jq -r '.events[] | "    \(.start)\t\(if .all_day then "all day" else (.timezone // "") end)\t\(.title)"' <<<"$resp"
    [ "$(jq '.events | length' <<<"$resp")" -gt 0 ] || info "  no events in the next 30 days"
  else
    info "  $(jq -r '.error' <<<"$resp")"
  fi
done

bold "Done"
info "API guide: docs/api.md · OpenAPI: api/openapi.yaml"
info "Disconnect an account: curl -X DELETE -H \"Authorization: Bearer <api key>\" $RELAY_URL/v1/accounts/<id>"
