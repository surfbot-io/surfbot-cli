package transport

import (
	"net"
	"strings"
	"testing"
)

func TestFingerprintWith_Stable(t *testing.T) {
	a := fingerprintWith("abc123", "00:11:22:33:44:55")
	b := fingerprintWith("abc123", "00:11:22:33:44:55")
	if a != b {
		t.Fatalf("not stable: %q vs %q", a, b)
	}
	if !strings.HasPrefix(a, "sha256:") {
		t.Fatalf("missing prefix: %q", a)
	}
	if len(a) != len("sha256:")+64 {
		t.Fatalf("unexpected length: %d", len(a))
	}
}

func TestFingerprintWith_DivergesOnInput(t *testing.T) {
	a := fingerprintWith("host-a", "00:11:22:33:44:55")
	b := fingerprintWith("host-b", "00:11:22:33:44:55")
	if a == b {
		t.Fatalf("expected different fingerprints for different machine-ids")
	}
}

func TestPickPrimaryMAC_SkipsLoopback(t *testing.T) {
	ifs := []net.Interface{
		{Name: "lo", Flags: net.FlagUp | net.FlagLoopback, HardwareAddr: net.HardwareAddr{}},
		{Name: "eth0", Flags: net.FlagUp, HardwareAddr: net.HardwareAddr{0x00, 0x11, 0x22, 0x33, 0x44, 0x55}},
	}
	got := pickPrimaryMAC(ifs)
	if got != "00:11:22:33:44:55" {
		t.Fatalf("got %q want eth0 mac", got)
	}
}

func TestPickPrimaryMAC_SkipsVirtual(t *testing.T) {
	ifs := []net.Interface{
		{Name: "docker0", Flags: net.FlagUp, HardwareAddr: net.HardwareAddr{0x02, 0x42, 0xAC, 0x11, 0x00, 0x02}},
		{Name: "eth0", Flags: net.FlagUp, HardwareAddr: net.HardwareAddr{0x00, 0x11, 0x22, 0x33, 0x44, 0x55}},
	}
	got := pickPrimaryMAC(ifs)
	if got != "00:11:22:33:44:55" {
		t.Fatalf("got %q want eth0 mac (docker0 skipped)", got)
	}
}

func TestPickPrimaryMAC_NoCandidate(t *testing.T) {
	ifs := []net.Interface{
		{Name: "lo", Flags: net.FlagUp | net.FlagLoopback},
	}
	got := pickPrimaryMAC(ifs)
	if got != "no-mac" {
		t.Fatalf("got %q want no-mac fallback", got)
	}
}

func TestFingerprint_LiveHost(t *testing.T) {
	// Smoke: real Fingerprint should succeed on this host (test runs locally),
	// or fail with a clear error on stripped-down CI. We tolerate either, but
	// when it succeeds the format must match.
	fp, err := Fingerprint()
	if err != nil {
		t.Skipf("Fingerprint not supported on this host: %v", err)
	}
	if !strings.HasPrefix(fp, "sha256:") || len(fp) != len("sha256:")+64 {
		t.Fatalf("bad format: %q", fp)
	}
}
