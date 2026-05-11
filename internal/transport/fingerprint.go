package transport

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// Fingerprint returns the stable per-host hash sent in client.hello, used by
// the server to detect token replay across machines. SPEC-CLI1 §11 T2.
//
// The hash is sha256(machine-id + ":" + primary-mac) and prefixed
// "sha256:" to give the server a self-describing format if we ever rotate
// the algorithm. This is **best-effort detection, not anti-replay**: a
// root attacker can falsify both inputs. The value's purpose is to flag
// movement of an agent_token to a different machine so an operator can
// react via revoke.
func Fingerprint() (string, error) {
	mid, err := machineID()
	if err != nil {
		return "", fmt.Errorf("fingerprint: machine-id: %w", err)
	}
	mac, err := primaryMAC()
	if err != nil {
		return "", fmt.Errorf("fingerprint: mac: %w", err)
	}
	h := sha256.Sum256([]byte(mid + ":" + mac))
	return "sha256:" + hex.EncodeToString(h[:]), nil
}

// fingerprintWith is the test seam: takes pre-resolved inputs and produces
// the same hash. Lets tests assert stability without poking the host.
func fingerprintWith(machineID, mac string) string {
	h := sha256.Sum256([]byte(machineID + ":" + mac))
	return "sha256:" + hex.EncodeToString(h[:])
}

// machineID reads the OS-specific stable host identifier.
//
//	Linux:   /etc/machine-id (systemd) or /var/lib/dbus/machine-id (legacy).
//	Darwin:  ioreg IOPlatformUUID.
//	Windows: registry MachineGuid via PowerShell (avoids pulling in
//	         golang.org/x/sys/windows/registry just for this).
func machineID() (string, error) {
	switch runtime.GOOS {
	case "linux":
		for _, p := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
			if b, err := os.ReadFile(p); err == nil {
				return strings.TrimSpace(string(b)), nil
			}
		}
		return "", fmt.Errorf("machine-id: no readable source")
	case "darwin":
		// `ioreg -rd1 -c IOPlatformExpertDevice` prints a property line like
		//   "IOPlatformUUID" = "A1B2C3D4-...".
		// We grep for it without spawning awk; standard library is enough.
		out, err := exec.Command("ioreg", "-rd1", "-c", "IOPlatformExpertDevice").Output()
		if err != nil {
			return "", fmt.Errorf("ioreg: %w", err)
		}
		const key = `"IOPlatformUUID"`
		for _, line := range bytes.Split(out, []byte("\n")) {
			i := bytes.Index(line, []byte(key))
			if i < 0 {
				continue
			}
			rest := line[i+len(key):]
			// Find the value between the quotes after the `=` sign.
			j := bytes.IndexByte(rest, '"')
			if j < 0 {
				continue
			}
			rest = rest[j+1:]
			k := bytes.IndexByte(rest, '"')
			if k < 0 {
				continue
			}
			return string(rest[:k]), nil
		}
		return "", fmt.Errorf("IOPlatformUUID not found in ioreg output")
	case "windows":
		out, err := exec.Command(
			"powershell.exe", "-NoProfile", "-Command",
			`(Get-ItemProperty 'HKLM:\SOFTWARE\Microsoft\Cryptography' -Name MachineGuid).MachineGuid`,
		).Output()
		if err != nil {
			return "", fmt.Errorf("powershell MachineGuid: %w", err)
		}
		return strings.TrimSpace(string(out)), nil
	default:
		return "", fmt.Errorf("unsupported OS for machine-id: %s", runtime.GOOS)
	}
}

// primaryMAC returns the MAC address of the first non-loopback,
// non-virtual interface that is UP and has a hardware address. When no
// interface qualifies (e.g. fully sandboxed CI), returns "no-mac" so the
// hash stays deterministic per host — the divergence-detection signal
// degrades to "machine-id only", which is still strictly better than
// failing to enroll.
func primaryMAC() (string, error) {
	ifs, err := net.Interfaces()
	if err != nil {
		return "", err
	}
	return pickPrimaryMAC(ifs), nil
}

func pickPrimaryMAC(ifs []net.Interface) string {
	for _, i := range ifs {
		if i.Flags&net.FlagLoopback != 0 {
			continue
		}
		if i.Flags&net.FlagUp == 0 {
			continue
		}
		mac := i.HardwareAddr.String()
		if mac == "" {
			continue
		}
		if isVirtualIface(i.Name) {
			continue
		}
		return mac
	}
	return "no-mac"
}

// isVirtualIface filters out common virtual / tunnel interfaces that have
// random MACs and would defeat the stability goal. Best-effort heuristic.
func isVirtualIface(name string) bool {
	prefixes := []string{"docker", "br-", "veth", "tun", "tap", "utun", "awdl", "llw", "tailscale", "wg"}
	low := strings.ToLower(name)
	for _, p := range prefixes {
		if strings.HasPrefix(low, p) {
			return true
		}
	}
	return false
}
