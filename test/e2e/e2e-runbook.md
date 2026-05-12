# Sprint CLI1 — E2E Validation Runbook (SPEC §15)

> **Source of truth for the 12-step DoD.** This document is executed
> by Claude Code in a session that has both Docker and the Chrome MCP
> plugin available. Each step lists an explicit owner — `[CONTAINER]`
> (run via `docker exec`), `[CHROME]` (Chrome MCP browser action), or
> `[CLAUDE]` (orchestrator decision / aggregation) — its pre-condition,
> the exact command/action, and the PASS/FAIL criterion.
>
> Follow the steps in order. Do **not** improvise. If a step's
> precondition isn't met, stop and report — do not retry blindly.

---

## Operator prerequisites (humano antes de lanzar la sesión)

1. **Docker** funcionando en local (`docker ps` responde).
2. **Chrome MCP plugin** activo y conectado al browser.
3. **Sesión logueada** en `https://app-staging.surfbot.io` en el browser que Chrome MCP controla. Una vez por sesión de trabajo basta.
4. **Outbound** a `api-staging.surfbot.io` accesible desde la máquina.

Si Claude Code detecta que falta cualquiera de los cuatro, debe **parar y reportar** — no improvisar.

---

## Configurable env vars

These default to sane staging values but the orchestrator can override
them when invoking Claude Code:

| Variable                | Default                                   | Purpose                                       |
| ----------------------- | ----------------------------------------- | --------------------------------------------- |
| `IMAGE_TAG`             | `surfbot-e2e-img`                         | docker image tag built from `test/e2e/`       |
| `CONTAINER_NAME`        | `surfbot-e2e`                             | running container name                        |
| `API`                   | `https://api-staging.surfbot.io`          | api base; passed into the container           |
| `WEB`                   | `https://app-staging.surfbot.io`          | web app base; navigated by Chrome MCP         |
| `REVOKE_TIMEOUT`        | `90`                                      | seconds to wait for token purge after revoke  |
| `NETWORK_CUT_DURATION`  | `130`                                     | seconds the iptables DROP stays in place      |

---

## Pre-flight (Claude Code does this first)

- **[CLAUDE]** Verify Docker reachable: `docker ps`. If it errors, fail-fast with "Docker is not running."
- **[CLAUDE]** Verify Chrome MCP plugin is available in this session. If not, fail-fast with "Chrome MCP not connected — re-launch the session with the plugin enabled."
- **[CHROME]** Navigate to `${WEB}/agents`. Expect to land on the agents page (not `/login`). If redirected to `/login`, fail-fast with "Please log in to ${WEB} in Chrome before running this runbook."
- **[CLAUDE]** Cross-compile the cli for `linux/amd64` (the e2e container is amd64-only — see `test/e2e/Dockerfile`):
  ```bash
  GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
    go build -o bin/surfbot-cli-linux-amd64 ./cmd/surfbot-cli
  ```
  Confirm the binary exists. On Linux/amd64 hosts a plain `make build`
  is equivalent, but using a distinct output path keeps the operator's
  host build at `bin/surfbot-cli` untouched.
- **[CLAUDE]** Build the e2e image (one-shot, cached on rebuilds):
  ```bash
  docker build -t "${IMAGE_TAG}" test/e2e/
  ```
- **[CLAUDE]** Generate a short random suffix for this run, e.g. `RAND=$(openssl rand -hex 3)`. Use it for `display_name` (step 4) and the paste-token label (step 12) so abandoned agents are easy to spot in the dashboard.

If any pre-flight check fails, **stop**. Do not continue.

---

## Step 1 — Container up

**[CONTAINER]**
```bash
docker run --rm -d --name "${CONTAINER_NAME}" \
  --platform linux/amd64 \
  --privileged \
  -v /sys/fs/cgroup:/sys/fs/cgroup:rw \
  -v "$(pwd)/bin/surfbot-cli-linux-amd64:/usr/local/bin/surfbot-cli:ro" \
  -v "$(pwd)/test/e2e/container-internal.sh:/opt/e2e/internal.sh:ro" \
  -e "API=${API}" \
  -e "REVOKE_TIMEOUT=${REVOKE_TIMEOUT}" \
  -e "NETWORK_CUT_DURATION=${NETWORK_CUT_DURATION}" \
  "${IMAGE_TAG}"
```

**PASS** — `docker ps --filter "name=${CONTAINER_NAME}"` shows the container running. Give systemd 5–10s to come up before the next step.

---

## Step 2 — Cli binary works

**[CONTAINER]**
```bash
docker exec -u surfbot "${CONTAINER_NAME}" /opt/e2e/internal.sh bootstrap
```

**PASS** — stdout includes `[PASS] bootstrap: version=…`.

---

## Step 3 — Cli `login` → capture user_code

**[CONTAINER]** Start the device-code login. The subcommand backgrounds the cli, parses the activation banner, and prints `CODE=` and `URL=` lines for the orchestrator to capture.

```bash
docker exec -u surfbot "${CONTAINER_NAME}" /opt/e2e/internal.sh login_start
```

**[CLAUDE]** Parse `CODE=XXXX-XXXX` and `URL=https://…/cli/activate?…` from the stdout of the previous command. Store both for the next step.

**PASS** — both `CODE` and `URL` were captured and the subcommand printed `[PASS] login_start: …`.

---

## Step 4 — Browser: paste code + authorize

**[CHROME]** Navigate to the captured `URL` (it embeds the code via `?code=`).

**[CHROME]** Confirm the activation page shows the container's hostname and source IP. (Hostname will be the container's auto-generated ID — that is fine.)

**[CHROME]** Set the agent **display_name** to `e2e-test-${RAND}` so the agent is recognisable in the dashboard later.

**[CHROME]** Click **Authorize endpoint**.

**[CHROME]** Read the success page; capture the **agent_id** that's displayed (it appears near the success banner; if the UI doesn't show it on success, read it later from `/agents`).

**[CLAUDE]** Store `AGENT_ID` and `DISPLAY_NAME` for step 9.

**PASS** — success banner visible, `AGENT_ID` captured.

---

## Step 5 — Cli login completes inside container

**[CONTAINER]**
```bash
docker exec "${CONTAINER_NAME}" /opt/e2e/internal.sh wait_authorized
```

**PASS** — stdout includes `[PASS] wait_authorized: login completed, token at …`.

---

## Step 5b — Install + start the daemon

**[CONTAINER]**
```bash
docker exec -u surfbot "${CONTAINER_NAME}" /opt/e2e/internal.sh daemon_install_and_start
```

**PASS** — stdout includes `[PASS] daemon_install_and_start: …, status online`.

---

## Step 6 — Status check online

**[CONTAINER]**
```bash
docker exec -u surfbot "${CONTAINER_NAME}" /opt/e2e/internal.sh status_check_online
```

**PASS** — stdout includes `[PASS] status_check_online: status online`.

---

## Step 7 — Network cut (≈130s)

**[CONTAINER]** This subcommand blocks for `NETWORK_CUT_DURATION` seconds. Run it with a matching `docker exec` timeout, but never short-circuit it — the daemon must actually attempt reconnects during the outage for the test to be meaningful.

```bash
docker exec "${CONTAINER_NAME}" /opt/e2e/internal.sh network_cut
```

**PASS** — stdout includes `[PASS] network_cut: egress blocked for Ns, then restored`. The subcommand restores the iptables rule even if interrupted (trap on EXIT), but on FAIL log + flag for manual cleanup before continuing.

---

## Step 8 — Wait reconnect

**[CONTAINER]**
```bash
docker exec -u surfbot "${CONTAINER_NAME}" /opt/e2e/internal.sh wait_reconnect
```

**PASS** — stdout includes `[PASS] wait_reconnect: reconnected within Ns`.

---

## Step 9 — Browser: revoke from `/agents`

**[CHROME]** Navigate to `${WEB}/agents`.

**[CHROME]** Locate the row whose **id** matches `AGENT_ID` (or whose **display_name** matches `e2e-test-${RAND}` if the id isn't shown in the table). Click **Revoke**.

**[CHROME]** In the confirmation modal, type the **display_name** (`e2e-test-${RAND}`) and click **Revoke this agent**.

**[CHROME]** Assert the row now shows a **revoked** status (or the row is removed, depending on UI variant).

**PASS** — revoke confirmation succeeded; no error banner.

---

## Step 10 — Container: confirm token purge

**[CONTAINER]**
```bash
docker exec "${CONTAINER_NAME}" /opt/e2e/internal.sh wait_token_purged
```

**PASS** — stdout includes `[PASS] wait_token_purged: token + metadata purged`.

---

## Step 11 — Bad enroll → clean 401

**[CONTAINER]**
```bash
docker exec -u surfbot "${CONTAINER_NAME}" /opt/e2e/internal.sh bad_enroll
```

**PASS** — stdout includes `[PASS] bad_enroll: …` (the cli surfaced a 401/unauthorized/invalid error rather than crashing).

---

## Step 12 — Browser: generate enrollment token + paste-enroll

**[CHROME]** Navigate `${WEB}/agents` → click **Add agent**.

**[CHROME]** Switch to the **Generate enrollment token** tab.

**[CHROME]** Set:
- **label**: `e2e-paste-${RAND}`
- **max_uses**: `1`
- **expires**: `15 min`

Click **Generate**.

**[CHROME]** Capture the token from the reveal box (it appears once; cannot be re-read). Store as `ENROLL_TOKEN`.

**[CONTAINER]**
```bash
docker exec -u surfbot "${CONTAINER_NAME}" /opt/e2e/internal.sh paste_enroll "${ENROLL_TOKEN}"
```

**PASS** — stdout includes `[PASS] paste_enroll: …, status online`.

---

## Final cleanup

**[CONTAINER]** Idempotent teardown — safe even after a failure earlier in the run:
```bash
docker exec "${CONTAINER_NAME}" /opt/e2e/internal.sh cleanup
docker stop "${CONTAINER_NAME}"
```

**[CLAUDE]** Aggregate `[PASS]` / `[FAIL]` markers from every step into a final report:

```
Step 1  Container up                    [PASS|FAIL]
Step 2  Cli binary works                [PASS|FAIL]
Step 3  login_start                     [PASS|FAIL]
Step 4  Browser authorize               [PASS|FAIL]
Step 5  wait_authorized                 [PASS|FAIL]
Step 5b daemon_install_and_start        [PASS|FAIL]
Step 6  status_check_online             [PASS|FAIL]
Step 7  network_cut                     [PASS|FAIL]
Step 8  wait_reconnect                  [PASS|FAIL]
Step 9  Browser revoke                  [PASS|FAIL]
Step 10 wait_token_purged               [PASS|FAIL]
Step 11 bad_enroll                      [PASS|FAIL]
Step 12 paste_enroll                    [PASS|FAIL]
```

Exit non-zero if **any** step failed. Print a one-line summary at the
top, e.g. `RESULT: 13/13 PASS` or `RESULT: 11/13 PASS — see Step 7, 10`.

---

## Failure handling notes (for Claude Code)

- If pre-flight fails, **don't** start the container — there's nothing to clean up.
- If a CONTAINER step fails, run `cleanup` then `docker stop` before exiting. Leaving a `--privileged` container with an iptables DROP rule installed is the worst case; the `cleanup` subcommand and the `EXIT` trap in `network_cut` both guard against it, but a manual `sudo iptables -D OUTPUT -d api-staging.surfbot.io -j DROP` on the host is the escape hatch.
- If a CHROME step fails (element not found, modal didn't open), capture a screenshot before retrying — UI drift between SPEC §15 wireframes and live staging is a likely root cause and Andrew needs the screenshot to fix the brief.
- Do **not** retry a failed step silently. Report `[FAIL]` and continue with cleanup. The runbook is meant to surface real regressions, not paper over them.
