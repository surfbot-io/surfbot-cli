package transport

import (
	"context"
	"time"

	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/host"
	"github.com/shirou/gopsutil/v3/load"
	"github.com/shirou/gopsutil/v3/mem"
)

// Sample returns the current host vitals shaped to fit a heartbeat payload.
// Each underlying probe is best-effort: a single failing source degrades to
// a zero value rather than dropping the heartbeat (the WS handler treats
// zero values as "data unavailable" and still records the heartbeat tick).
func Sample(ctx context.Context) HeartbeatPayload {
	p := HeartbeatPayload{LoadAvg: []float64{0, 0, 0}}

	if up, err := host.UptimeWithContext(ctx); err == nil {
		p.UptimeSeconds = int(up)
	} else if startup := processStartFallback(); !startup.IsZero() {
		p.UptimeSeconds = int(time.Since(startup).Seconds())
	}

	if l, err := load.AvgWithContext(ctx); err == nil && l != nil {
		p.LoadAvg = []float64{l.Load1, l.Load5, l.Load15}
	}

	if vm, err := mem.VirtualMemoryWithContext(ctx); err == nil && vm != nil {
		p.MemUsedMB = int(vm.Used / 1024 / 1024)
		p.MemTotalMB = int(vm.Total / 1024 / 1024)
	}

	// Disk free on the process working directory's volume. gopsutil walks
	// the mounts list; we sample "/" on Unix and the system drive on Win,
	// which matches what an SRE expects from `df` on the box.
	root := diskRoot()
	if u, err := disk.UsageWithContext(ctx, root); err == nil && u != nil {
		p.DiskFreeGB = float64(u.Free) / (1024 * 1024 * 1024)
	}
	return p
}

// processStartFallback is used when host.Uptime fails (e.g. unprivileged
// container without /proc/uptime visibility). We don't ship the import for
// process uptime in the hot path; this just returns zero in that case and
// the heartbeat reports uptime=0 — server treats it as "unknown".
func processStartFallback() time.Time { return time.Time{} }
