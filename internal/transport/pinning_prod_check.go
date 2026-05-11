//go:build prod

package transport

// Build-time canary that closes the placeholder-pin fail-open gate.
//
// HOTFIX H5 from the PR #5 post-merge review: looksReal() at runtime only
// loosens the validator when ALL pins for a host are real, so a partial
// rotation (real prod pin but staging still placeholder, or vice versa)
// silently kept the binary fail-open. CI runs `go build -tags prod ./...`
// before every release artifact; this init() panics if any host pin still
// contains the PLACEHOLDER sentinel, breaking the build before a binary
// ships to operators.
//
// To bypass for an emergency dev build, drop the `-tags prod` flag — but
// such a build MUST NOT be uploaded to a release surface (goreleaser,
// homebrew, GHCR). The CI workflow gates the upload step on this build
// succeeding.

func init() {
	for host, pins := range hostPins {
		if !looksReal(pins) {
			panic("transport/pinning_prod_check: host " + host +
				" still has placeholder SPKI pins — replace them before building with -tags prod")
		}
	}
}
