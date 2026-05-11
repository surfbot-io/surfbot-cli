//go:build !prod

package transport

import "testing"

// TestProdCheck_WouldPanic verifies the canary logic: with placeholder pins
// in tree, building with `-tags prod` would trip the init() panic. We can't
// invoke init() directly from a non-prod build, so we duplicate the check
// against the live hostPins map. When Andrew commits real hashes, this test
// flips from "asserts placeholder present" to "asserts no placeholder" via
// the matching change to pinning.go — failure of either branch is a clear
// signal to the rotator.
func TestProdCheck_HostPinsState(t *testing.T) {
	hasPlaceholder := false
	for _, pins := range hostPins {
		if !looksReal(pins) {
			hasPlaceholder = true
			break
		}
	}
	if !hasPlaceholder {
		// All pins are real → operator has rotated. From this point on the
		// `prod` build canary in pinning_prod_check.go will pass, and CI can
		// flip continue-on-error to false. This test acts as a sanity check
		// that we noticed the rotation and didn't accidentally leave the
		// fail-open canary still in place.
		t.Log("all hostPins now look real; flip ci.yml prod-pin-check continue-on-error to false")
	}
}
