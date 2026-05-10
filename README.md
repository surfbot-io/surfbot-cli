# surfbot-cli

> Cloud-bridge endpoint for Surfbot — no embedded scanners.

[![CI](https://github.com/surfbot-io/surfbot-cli/actions/workflows/ci.yml/badge.svg)](https://github.com/surfbot-io/surfbot-cli/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

`surfbot-cli` is the on-host endpoint for the Surfbot cloud platform. It
maintains a persistent WebSocket connection to `api.surfbot.io` and
executes whatever the cloud dispatches inside a local sandbox. Detection
logic, scheduling, and findings storage live in the cloud.

## Tombstone — supersedes `surfbot-agent`

This binary supersedes [`surfbot-agent`](https://github.com/surfbot-io/surfbot-agent)
(archived 2026-05-10). The agent shipped scanners, scheduler, and a local
SQLite findings store inside the binary; that architecture is gone.

The rationale and the new boundary live in **ADR-004 — surfbot-cli
cloud-bridge** in the (private) `surfbot-strategy` repo. Short version:
shipping detection logic to every host turned every release into a
fleet-wide upgrade and made the security surface unmanageable. The cloud
owns logic and state; the endpoint owns presence and execution.

## Status

**Bootstrap — PR0 of Sprint CLI1.** Only `surfbot-cli version` is wired.
The runtime (WebSocket transport, device-code enrollment, daemon) ships
across PR1..PR6.

| PR  | Scope                                              |
| --- | -------------------------------------------------- |
| PR0 | Repo scaffold, CI, release pipeline (this commit). |
| PR1 | WebSocket client + reconnect/backoff.              |
| PR2 | Device-code enrollment flow.                       |
| PR3 | Sandbox executor for cloud-dispatched jobs.        |
| PR4 | Heartbeat, capability advertisement, status RPCs.  |
| PR5 | `login` / `enroll` / `status` / `logout` commands. |
| PR6 | Daemon install/uninstall (systemd, launchd, SCM).  |

## Quick start

```sh
surfbot-cli version
```

`login`, `enroll`, `status`, and `logout` arrive in PR5. Daemon
install/uninstall arrives in PR6.

## Build from source

Requires Go 1.25+.

```sh
git clone https://github.com/surfbot-io/surfbot-cli.git
cd surfbot-cli
make build
./bin/surfbot-cli version
```

`make test` runs the unit tests. `make lint` runs `golangci-lint`.

## Architecture & design docs

- **ADR-004 — surfbot-cli cloud-bridge** — boundary, threat model,
  rollout. Lives in the private `surfbot-strategy` repo.
- **SPEC-CLI1 — WebSocket transport + device-code enrollment** —
  protocol, state machine, command surface (§10), PR plan (§13). Lives
  in the private `surfbot-strategy` repo.

The strategy repo is internal; if you need access for audit or
integration work, contact `security@surfbot.io`.

## License

MIT — see [LICENSE](LICENSE).

## Security

Report vulnerabilities to `security@surfbot.io`. Please do not open
public issues for security reports.
