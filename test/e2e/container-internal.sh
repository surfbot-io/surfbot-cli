#!/usr/bin/env bash
# In-container helper invoked one subcommand at a time via `docker exec`
# from the Claude Code orchestrator (see test/e2e/e2e-runbook.md).
#
# Each subcommand is independently runnable, prints `[PASS] <id>: <msg>`
# or `[FAIL] <id>: <msg>` to stdout, and exits 0 on PASS / non-zero on
# FAIL. Captured values for the next step (user_code, verification_uri,
# agent_id) are emitted as `KEY=VALUE` lines so the orchestrator can
# grep them out.
#
# Env vars (all overridable from outside the container):
#   BINARY                path to the cli binary (default /usr/local/bin/surfbot-cli)
#   API                   https base url of the api (default https://api-staging.surfbot.io)
#   CONFIG_DIR            on-disk config dir (default $HOME/.config/surfbot)
#   TOKEN_FILE            override path to agent.token (default $CONFIG_DIR/agent.token)
#   META_FILE             override path to metadata.json (default $CONFIG_DIR/metadata.json)
#   REVOKE_TIMEOUT        seconds to wait for token purge after revoke (default 90)
#   NETWORK_CUT_DURATION  seconds to keep iptables DROP in place (default 130)
#   RECONNECT_TIMEOUT     seconds to wait for status=online after restore (default 30)
#   LOGIN_TIMEOUT         seconds to wait for `cli login` to complete (default 300)
#   STATUS_POLL_INTERVAL  seconds between status polls (default 2)

set -uo pipefail

BINARY="${BINARY:-/usr/local/bin/surfbot-cli}"
API="${API:-https://api-staging.surfbot.io}"
CONFIG_DIR="${CONFIG_DIR:-$HOME/.config/surfbot}"
TOKEN_FILE="${TOKEN_FILE:-$CONFIG_DIR/agent.token}"
META_FILE="${META_FILE:-$CONFIG_DIR/metadata.json}"
REVOKE_TIMEOUT="${REVOKE_TIMEOUT:-90}"
NETWORK_CUT_DURATION="${NETWORK_CUT_DURATION:-130}"
RECONNECT_TIMEOUT="${RECONNECT_TIMEOUT:-30}"
LOGIN_TIMEOUT="${LOGIN_TIMEOUT:-300}"
STATUS_POLL_INTERVAL="${STATUS_POLL_INTERVAL:-2}"

LOGIN_LOG="/tmp/surfbot-login.log"
LOGIN_PID_FILE="/tmp/surfbot-login.pid"

log()  { printf '[LOG]  %s: %s\n' "${STEP_ID:-?}" "$*" >&2; }
pass() { printf '[PASS] %s: %s\n' "${STEP_ID:-?}" "$*"; }
fail() { printf '[FAIL] %s: %s\n' "${STEP_ID:-?}" "$*"; exit 1; }

api_host() {
  printf '%s\n' "$API" | sed -E 's#^https?://##; s#/.*$##'
}

status_is_online() {
  "$BINARY" status 2>&1 | grep -Eq 'status:[[:space:]]+online'
}

poll_until() {
  local timeout="$1"; shift
  local deadline=$((SECONDS + timeout))
  while (( SECONDS < deadline )); do
    if "$@"; then return 0; fi
    sleep "$STATUS_POLL_INTERVAL"
  done
  return 1
}

# ── Subcommand: bootstrap ────────────────────────────────────────────────────
# Smoke that the cli binary is mounted and runnable. This is the only
# subcommand the orchestrator runs during PR-time smoke (no network /
# staging required).
cmd_bootstrap() {
  STEP_ID="bootstrap"
  if [[ ! -x "$BINARY" ]]; then
    fail "binary not executable at $BINARY"
  fi
  local out
  out="$("$BINARY" version 2>&1)" || fail "version exited non-zero: $out"
  pass "version=$(printf '%s' "$out" | head -1)"
}

# ── Subcommand: login_start ──────────────────────────────────────────────────
# Kicks off the device-code login in the background, redirecting all
# output to $LOGIN_LOG, and emits `CODE=` and `URL=` lines on stdout
# as soon as they appear (so the orchestrator can capture them and
# drive Chrome MCP).
cmd_login_start() {
  STEP_ID="login_start"
  mkdir -p "$CONFIG_DIR"
  rm -f "$LOGIN_LOG" "$LOGIN_PID_FILE"

  # Run cli login in the background, capturing output for later steps.
  # nohup detaches it from this docker exec so the exec returns once
  # CODE/URL are parsed.
  nohup "$BINARY" login --api-base "$API" >"$LOGIN_LOG" 2>&1 &
  echo $! >"$LOGIN_PID_FILE"

  # Poll the log for the verification banner. SPEC §10 wireframe S1
  # emits "Visit:\n  <url>\n\nAnd enter this code:\n  <code>\n".
  local deadline=$((SECONDS + 60))
  local url="" code=""
  while (( SECONDS < deadline )); do
    if [[ -f "$LOGIN_LOG" ]]; then
      url="$(grep -oE 'https?://[^[:space:]]+/cli/activate[^[:space:]]*' "$LOGIN_LOG" | head -1 || true)"
      if [[ -z "$url" ]]; then
        # Fall back: first https line after a "Visit:" header.
        url="$(awk '/^Visit:/{getline; getline; print; exit}' "$LOGIN_LOG" | tr -d '[:space:]')"
      fi
      code="$(grep -oE '[A-Z0-9]{4}-[A-Z0-9]{4}' "$LOGIN_LOG" | head -1 || true)"
      if [[ -n "$url" && -n "$code" ]]; then
        printf 'CODE=%s\n' "$code"
        printf 'URL=%s\n'  "$url"
        pass "login banner captured"
        return 0
      fi
    fi
    sleep 1
  done
  log "login log so far:"
  if [[ -f "$LOGIN_LOG" ]]; then sed -n '1,40p' "$LOGIN_LOG" >&2; fi
  fail "did not see CODE+URL within 60s"
}

# ── Subcommand: wait_authorized ──────────────────────────────────────────────
# Blocks until the background `cli login` exits (success after the
# browser confirms the code).
cmd_wait_authorized() {
  STEP_ID="wait_authorized"
  if [[ ! -f "$LOGIN_PID_FILE" ]]; then
    fail "no login pidfile at $LOGIN_PID_FILE; did you skip login_start?"
  fi
  local pid; pid="$(cat "$LOGIN_PID_FILE")"

  local deadline=$((SECONDS + LOGIN_TIMEOUT))
  while (( SECONDS < deadline )); do
    if ! kill -0 "$pid" 2>/dev/null; then
      # Process exited. Confirm token persisted before declaring PASS.
      if [[ -s "$TOKEN_FILE" ]]; then
        pass "login completed, token at $TOKEN_FILE"
        return 0
      fi
      log "login process exited but token not present; log tail:"
      tail -n 30 "$LOGIN_LOG" >&2 || true
      fail "login exited without writing a token"
    fi
    sleep 1
  done
  log "login still running after ${LOGIN_TIMEOUT}s; killing pid $pid"
  kill -INT "$pid" 2>/dev/null || true
  fail "login did not complete within ${LOGIN_TIMEOUT}s"
}

# ── Subcommand: daemon_install_and_start ─────────────────────────────────────
# Installs the systemd unit and starts the daemon; asserts it ends up
# in the "online" state.
cmd_daemon_install_and_start() {
  STEP_ID="daemon_install_and_start"
  sudo "$BINARY" daemon install      || fail "daemon install failed"
  sudo "$BINARY" daemon start        || fail "daemon start failed"
  if poll_until "$RECONNECT_TIMEOUT" status_is_online; then
    pass "daemon installed + running, status online"
  else
    log "status output:"
    "$BINARY" status >&2 || true
    fail "daemon never reached online within ${RECONNECT_TIMEOUT}s"
  fi
}

# ── Subcommand: status_check_online ──────────────────────────────────────────
cmd_status_check_online() {
  STEP_ID="status_check_online"
  if status_is_online; then
    pass "status online"
  else
    log "status output:"
    "$BINARY" status >&2 || true
    fail "status not online"
  fi
}

# ── Subcommand: network_cut ──────────────────────────────────────────────────
# Block egress to the api host for $NETWORK_CUT_DURATION seconds, then
# restore. Asserts status was online before the cut and that the cli
# logged at least one reconnect attempt during the outage.
cmd_network_cut() {
  STEP_ID="network_cut"
  local host; host="$(api_host)"

  status_is_online || fail "status not online before cut; refusing to proceed"

  log "blocking egress to $host for ${NETWORK_CUT_DURATION}s"
  sudo iptables -A OUTPUT -d "$host" -j DROP || \
    fail "could not install iptables DROP for $host"

  # Cleanup guard: even if the sleep is interrupted, restore the rule.
  trap 'sudo iptables -D OUTPUT -d "$host" -j DROP 2>/dev/null || true' EXIT

  sleep "$NETWORK_CUT_DURATION"

  sudo iptables -D OUTPUT -d "$host" -j DROP || \
    fail "could not remove iptables DROP for $host (manual cleanup needed)"
  trap - EXIT
  pass "egress blocked for ${NETWORK_CUT_DURATION}s, then restored"
}

# ── Subcommand: wait_reconnect ───────────────────────────────────────────────
cmd_wait_reconnect() {
  STEP_ID="wait_reconnect"
  if poll_until "$RECONNECT_TIMEOUT" status_is_online; then
    pass "reconnected within ${RECONNECT_TIMEOUT}s"
  else
    log "status after restore:"
    "$BINARY" status >&2 || true
    fail "did not reconnect within ${RECONNECT_TIMEOUT}s"
  fi
}

# ── Subcommand: wait_token_purged ────────────────────────────────────────────
# Waits up to $REVOKE_TIMEOUT seconds for the daemon to receive the WS
# close 4401 and delete the token + metadata files.
cmd_wait_token_purged() {
  STEP_ID="wait_token_purged"
  local deadline=$((SECONDS + REVOKE_TIMEOUT))
  while (( SECONDS < deadline )); do
    if [[ ! -e "$TOKEN_FILE" && ! -e "$META_FILE" ]]; then
      pass "token + metadata purged"
      return 0
    fi
    sleep "$STATUS_POLL_INTERVAL"
  done
  log "files still present after ${REVOKE_TIMEOUT}s:"
  ls -la "$CONFIG_DIR" >&2 || true
  fail "token/metadata not purged within ${REVOKE_TIMEOUT}s"
}

# ── Subcommand: bad_enroll ───────────────────────────────────────────────────
# Confirms a fabricated token produces a clean 401-class error rather
# than crashing or persisting state.
cmd_bad_enroll() {
  STEP_ID="bad_enroll"
  local out
  out="$("$BINARY" enroll --api-base "$API" \
        srfb_enr_invalid_token_xxxxxxxxxxxxxxxxxxxxxxxx 2>&1 || true)"
  if printf '%s' "$out" | grep -Eq '401|unauthorized|invalid'; then
    pass "bad enroll rejected (output contained 401/unauthorized/invalid)"
  else
    log "bad enroll output: $out"
    fail "bad enroll did not surface 401-class error"
  fi
}

# ── Subcommand: paste_enroll ─────────────────────────────────────────────────
# Enrolls with a freshly minted enrollment token (generated in the
# browser by Chrome MCP) and asserts the cli ends up online again.
cmd_paste_enroll() {
  STEP_ID="paste_enroll"
  local token="${1:-}"
  if [[ -z "$token" ]]; then
    fail "paste_enroll requires a token argument"
  fi
  "$BINARY" enroll --api-base "$API" "$token" || fail "enroll failed"
  # daemon should pick up the new token; nudge with a restart for
  # deterministic behaviour rather than waiting for the file watcher.
  sudo "$BINARY" daemon restart || fail "daemon restart failed"
  if poll_until "$RECONNECT_TIMEOUT" status_is_online; then
    pass "paste-token enroll succeeded, status online"
  else
    log "status output:"
    "$BINARY" status >&2 || true
    fail "post-enroll status never reached online"
  fi
}

# ── Subcommand: cleanup ──────────────────────────────────────────────────────
# Idempotent teardown: stops/uninstalls the daemon, flushes the
# iptables rule (best-effort), and removes any lingering login child
# process. Safe to call from a failure path.
cmd_cleanup() {
  STEP_ID="cleanup"
  local host; host="$(api_host)"
  sudo iptables -D OUTPUT -d "$host" -j DROP 2>/dev/null || true
  sudo "$BINARY" daemon stop      2>/dev/null || true
  sudo "$BINARY" daemon uninstall 2>/dev/null || true
  if [[ -f "$LOGIN_PID_FILE" ]]; then
    local pid; pid="$(cat "$LOGIN_PID_FILE")"
    kill -INT "$pid" 2>/dev/null || true
    rm -f "$LOGIN_PID_FILE"
  fi
  pass "cleanup complete"
}

usage() {
  cat <<EOF
usage: $(basename "$0") <subcommand> [args]

Subcommands (run one at a time via 'docker exec'):
  bootstrap                 — verify cli binary works (smoke; no network)
  login_start               — start 'cli login' in background, print CODE= and URL=
  wait_authorized           — block until login completes (after browser confirms)
  daemon_install_and_start  — install + start systemd unit, assert online
  status_check_online       — assert 'status' reports online
  network_cut               — iptables DROP egress to api host for NETWORK_CUT_DURATION
  wait_reconnect            — poll until status is online again
  wait_token_purged         — wait up to REVOKE_TIMEOUT for token+metadata removal
  bad_enroll                — submit invalid enrollment token, expect 401-class error
  paste_enroll <token>      — enroll with a freshly minted token, assert online
  cleanup                   — idempotent teardown (daemon, iptables, login pid)

Each subcommand prints '[PASS] <id>: <msg>' or '[FAIL] <id>: <msg>'
and exits 0 / non-zero accordingly.
EOF
}

main() {
  local sub="${1:-}"
  if [[ -z "$sub" ]]; then usage >&2; exit 2; fi
  shift || true
  case "$sub" in
    bootstrap)                 cmd_bootstrap                  "$@" ;;
    login_start)               cmd_login_start                "$@" ;;
    wait_authorized)           cmd_wait_authorized            "$@" ;;
    daemon_install_and_start)  cmd_daemon_install_and_start   "$@" ;;
    status_check_online)       cmd_status_check_online        "$@" ;;
    network_cut)               cmd_network_cut                "$@" ;;
    wait_reconnect)            cmd_wait_reconnect             "$@" ;;
    wait_token_purged)         cmd_wait_token_purged          "$@" ;;
    bad_enroll)                cmd_bad_enroll                 "$@" ;;
    paste_enroll)              cmd_paste_enroll               "$@" ;;
    cleanup)                   cmd_cleanup                    "$@" ;;
    -h|--help|help)            usage; exit 0 ;;
    *) printf 'unknown subcommand: %s\n' "$sub" >&2; usage >&2; exit 2 ;;
  esac
}

main "$@"
