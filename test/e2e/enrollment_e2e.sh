#!/usr/bin/env bash
# E2E test for surfbot-cli enrollment + lifecycle.
#
# Implements the 12-step Definition-of-Done checklist from SPEC-CLI1
# §15 (websocket-enrollment). Designed to run on a fresh VM
# (linux/amd64 or arm64) with a laptop + browser nearby for the
# manual confirmation steps.
#
# Pre-conditions:
#   - surfbot-cli binary in PATH, or its absolute path in $BINARY
#     (default: ./bin/surfbot-cli).
#   - api-staging.surfbot.io reachable from the VM.
#   - You are logged into https://app-staging.surfbot.io with a
#     session you can drive in the browser when prompted.
#   - sudo available (steps 6-8 need iptables).
#
# Optional env:
#   BINARY        path to the cli binary
#   API           https base URL of the api (default api-staging)
#   WEB           https base URL of the web app (default app-staging)
#   SKIP_NETWORK  set to 1 to skip iptables-based network-cut step
#                 (useful for laptop dev where you don't want to
#                 nuke connectivity)
#   SKIP_REVOKE   set to 1 to skip the revoke step (step 9)
#   SKIP_BAD      set to 1 to skip the bad-enroll step (step 10)
#   CONFIG_DIR    override the on-disk config dir
#                 (default: ~/.config/surfbot)
#
# Outputs results to stdout with [PASS] / [FAIL] / [MANUAL] / [SKIP]
# markers; exits non-zero on any FAIL. MANUAL prompts pause for the
# operator to do something in the web UI.

set -uo pipefail

BINARY="${BINARY:-./bin/surfbot-cli}"
API="${API:-https://api-staging.surfbot.io}"
WEB="${WEB:-https://app-staging.surfbot.io}"
CONFIG_DIR="${CONFIG_DIR:-$HOME/.config/surfbot}"
TOKEN_FILE="$CONFIG_DIR/agent.token"

RESULT=0

red()    { printf '\033[31m%s\033[0m\n' "$*"; }
green()  { printf '\033[32m%s\033[0m\n' "$*"; }
yellow() { printf '\033[33m%s\033[0m\n' "$*"; }

pass() { green "[PASS] $*"; }
fail() { red   "[FAIL] $*"; RESULT=1; }
skip() { yellow "[SKIP] $*"; }
manual() { yellow "[MANUAL] $*"; }
hdr()  { printf '\n=== Step %s ===\n' "$*"; }

confirm() {
  # confirm "prompt" — wait for the operator to acknowledge.
  printf '%s\nPress Enter when done (or Ctrl-C to abort)... ' "$*"
  read -r _ < /dev/tty || true
}

# ── Step 1: build + binary runnable ──────────────────────────────────────────
hdr 1
if [[ ! -x "$BINARY" ]]; then
  fail "binary not found at $BINARY (set BINARY=/abs/path or build first)"
  exit 1
fi
if ! "$BINARY" version >/dev/null 2>&1; then
  fail "binary at $BINARY did not run cleanly: $($BINARY version 2>&1 | head -3)"
  exit 1
fi
pass "binary runnable: $($BINARY version | head -1)"

# ── Steps 2-5: device-code login (manual approval in browser) ────────────────
hdr 2-5
manual "In another terminal on this machine, run:"
echo "         $BINARY login"
manual "When prompted, open the URL it prints in your laptop browser and confirm the code at $WEB/cli/activate"
manual "Wait for the cli to print '✓ Authorized as <hostname>'"
confirm "Once login reports success, come back here."

if [[ ! -f "$TOKEN_FILE" ]]; then
  fail "no token file at $TOKEN_FILE after login (steps 2-5 broke)"
  exit 1
fi
pass "token persisted at $TOKEN_FILE"

# ── Step 6: status reports online ────────────────────────────────────────────
hdr 6
if "$BINARY" status 2>&1 | grep -Eq 'status:[[:space:]]+online'; then
  pass "status online"
else
  echo "--- status output ---"
  "$BINARY" status 2>&1 || true
  echo "---------------------"
  fail "status did not report online"
fi

# ── Steps 7-8: network cut + reconnect (iptables, requires sudo) ─────────────
hdr 7-8
if [[ "${SKIP_NETWORK:-0}" == "1" ]]; then
  skip "network-cut step (SKIP_NETWORK=1)"
else
  API_HOST="$(printf '%s\n' "$API" | sed -E 's#^https?://##; s#/.*$##')"
  if ! command -v iptables >/dev/null 2>&1; then
    skip "iptables not installed; cannot simulate network cut"
  else
    manual "Will block egress to $API_HOST for ~130s, then restore."
    if sudo iptables -A OUTPUT -d "$API_HOST" -j DROP 2>/dev/null; then
      sleep 130
      sudo iptables -D OUTPUT -d "$API_HOST" -j DROP || \
        fail "could not remove iptables rule — restore manually with: sudo iptables -D OUTPUT -d $API_HOST -j DROP"
      # Give the cli a few seconds to reconnect.
      sleep 8
      if "$BINARY" status 2>&1 | grep -Eq 'status:[[:space:]]+online'; then
        pass "reconnect after network cut"
      else
        fail "did not reconnect within 8s after network restore"
      fi
    else
      skip "iptables block failed (need sudo + ipv4 host); cannot run network-cut test"
    fi
  fi
fi

# ── Step 9: revoke via web → cli auto-exits + token purged ───────────────────
hdr 9
if [[ "${SKIP_REVOKE:-0}" == "1" ]]; then
  skip "revoke step (SKIP_REVOKE=1)"
else
  manual "In $WEB/agents, find this agent and click revoke."
  confirm "Click revoke, then come back here."
  # Give the WS up to 8s to surface close 4401 + purge the token.
  for _ in 1 2 3 4 5 6 7 8; do
    if [[ ! -f "$TOKEN_FILE" ]]; then break; fi
    sleep 1
  done
  if [[ -f "$TOKEN_FILE" ]]; then
    fail "token file still present at $TOKEN_FILE 8s after revoke"
  else
    pass "token purged after revoke"
  fi
fi

# ── Step 10: bad enroll → clean 401 ──────────────────────────────────────────
hdr 10
if [[ "${SKIP_BAD:-0}" == "1" ]]; then
  skip "bad-enroll step (SKIP_BAD=1)"
else
  out=$("$BINARY" enroll srfb_enr_invalid_token_xxx 2>&1 || true)
  if printf '%s' "$out" | grep -q "401"; then
    pass "bad enroll rejected with 401"
  else
    fail "bad enroll did not surface 401; output was: $out"
  fi
fi

# ── Steps 11-12: paste-token enroll (manual web step to generate token) ──────
hdr 11-12
manual "In $WEB/agents → 'Add agent', generate a fresh enrollment token."
printf 'Paste the enrollment token here (or empty to skip): '
read -r TOKEN < /dev/tty || TOKEN=""
if [[ -z "$TOKEN" ]]; then
  skip "paste-token enroll (no token supplied)"
else
  if "$BINARY" enroll "$TOKEN"; then
    pass "paste-token enroll succeeded"
  else
    fail "paste-token enroll failed"
  fi
fi

# ── Audit log check (server-side; out of e2e binary scope) ───────────────────
hdr audit
manual "Server-side: verify entries in agent_audit_log table (or /agents/<id>/audit when implemented) include: enrolled, connected, disconnected, connected, revoked."

# ── Summary ──────────────────────────────────────────────────────────────────
echo
if [[ "$RESULT" == "0" ]]; then
  green "── e2e: all automated steps passed (RESULT=0) ──"
else
  red   "── e2e: at least one step failed (RESULT=$RESULT) ──"
fi
exit "$RESULT"
