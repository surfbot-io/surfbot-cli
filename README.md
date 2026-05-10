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
