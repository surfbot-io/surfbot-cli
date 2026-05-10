// Package main is the entry point for the surfbot-cli binary.
//
// surfbot-cli is the cloud-bridge endpoint for Surfbot. Per ADR-004 and
// SPEC-CLI1 it supersedes surfbot-agent (archived 2026-05-10): it is no
// longer a vulnerability scanner but a thin endpoint that maintains a
// persistent WebSocket connection to api.surfbot.io and executes whatever
// the cloud dispatches inside a sandbox.
package main

import "github.com/surfbot-io/surfbot-cli/internal/cli"

var (
	version   = "dev"
	commit    = "none"
	buildDate = "unknown"
)

func main() {
	cli.Version = version
	cli.Commit = commit
	cli.BuildDate = buildDate
	cli.Execute()
}
