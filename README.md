<p align="center">
  <a href="https://surfbot.io">
    <picture>
      <source media="(prefers-color-scheme: dark)" srcset=".github/assets/logo-dark.svg">
      <img src=".github/assets/logo.svg" alt="Surfbot" width="280">
    </picture>
  </a>
</p>

<p align="center">
  <strong>Endpoint binary for the Surfbot platform.</strong>
  <br>
  The reasoning layer that closes the loop between detection and response.
</p>

<p align="center">
  <a href="https://github.com/surfbot-io/surfbot-cli/actions/workflows/ci.yml"><img src="https://github.com/surfbot-io/surfbot-cli/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-MIT-blue.svg" alt="License: MIT"></a>
  <a href="https://surfbot.io"><img src="https://img.shields.io/badge/surfbot.io-website-05308C.svg" alt="surfbot.io"></a>
</p>

---

`surfbot-cli` is the on-host binary that connects your infrastructure to the
Surfbot platform. Once enrolled, it maintains a persistent, mutually
authenticated channel to Surfbot's control plane, advertises the host's
capabilities, and executes the workflows the platform dispatches inside a
local sandbox. Detection logic, scheduling, and findings storage live in
the platform — the binary stays small, sandboxed, and easy to audit.

## Install

### Build from source

Requires [Go 1.25](https://go.dev/dl/) or later.

```sh
git clone https://github.com/surfbot-io/surfbot-cli.git
cd surfbot-cli
make build
./bin/surfbot-cli version
```

The binary cross-compiles cleanly to **Linux**, **macOS**, and **Windows**
on `amd64` and `arm64`. Each combination is verified on every push by the
CI pipeline.

## Usage

```sh
surfbot-cli --help
```

For platform setup, host enrollment, and operator guidance, see the
[Surfbot documentation](https://surfbot.io).

## Running as a service

`surfbot-cli` can register itself with the OS service manager so it
reconnects automatically after reboots, network flaps, or daemon restarts.

```sh
# Install (registers systemd / launchd / Windows service entry — requires root):
sudo surfbot-cli daemon install

# Enroll this machine (one-time):
surfbot-cli login                           # device-code flow; or:
surfbot-cli enroll <token>                  # paste-token flow

# Start the service (or restart, if it was already running):
sudo surfbot-cli daemon start
sudo surfbot-cli daemon restart             # use this after login/enroll

# Inspect:
surfbot-cli daemon status                   # installed? running?
surfbot-cli status                          # what does the agent see from the cloud?

# Remove:
sudo surfbot-cli daemon stop
sudo surfbot-cli daemon uninstall
```

Logs are routed through the platform's standard service logging backend
(never raw stdout/stderr, which is unreliable under systemd / launchd):

| Platform | Where to look                                                       |
| -------- | ------------------------------------------------------------------- |
| Linux    | `journalctl -u surfbot-cli -f`                                      |
| macOS    | `log stream --predicate 'process == "surfbot-cli"'`                 |
| Windows  | Event Viewer → Windows Logs → Application (source: `surfbot-cli`)   |

When the cloud revokes the agent (close `4401`) or rejects its token
(`4403`), the daemon purges the on-disk token + metadata and exits; the
operator re-enrolls with `surfbot-cli login` and starts the service again.

## Verification

End-to-end verification follows the 12-step Definition of Done from
SPEC-CLI1 §15. The script lives at `test/e2e/enrollment_e2e.sh` and is
designed for a fresh VM (Hetzner / EC2 / a local Linux VM).

```sh
export BINARY=./bin/surfbot-cli             # or absolute path
./test/e2e/enrollment_e2e.sh
```

The script combines automated checks (`[PASS]` / `[FAIL]`) with three
manual confirmation steps that require a browser session against
`https://app-staging.surfbot.io` (login confirmation, revoke from the
dashboard, fresh enrollment token generation). Individual steps can be
skipped with `SKIP_NETWORK=1`, `SKIP_REVOKE=1`, `SKIP_BAD=1` env vars.
Exit code is `0` iff every non-skipped automated check passed.

## Development

| Target           | Description                              |
| ---------------- | ---------------------------------------- |
| `make build`     | Produce the binary at `./bin/surfbot-cli`. |
| `make test`      | Run the test suite with `-race`.         |
| `make lint`      | Run `golangci-lint`.                     |
| `make tidy`      | Run `go mod tidy`.                       |
| `make clean`     | Remove build artifacts.                  |

Run targets from the repository root.

## Security

Please report vulnerabilities privately to
[security@surfbot.io](mailto:security@surfbot.io). Do not disclose
security issues through public GitHub issues.

## License

`surfbot-cli` is released under the [MIT License](LICENSE) — © 2026 Surfbot.
