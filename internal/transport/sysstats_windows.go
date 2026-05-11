//go:build windows

package transport

import "os"

// diskRoot returns the volume hosting the current working directory on
// Windows. gopsutil's disk.Usage on Windows wants a drive letter ("C:\\"),
// not a UNC path; we fall back to %SystemDrive% if the cwd lookup fails.
func diskRoot() string {
	if cwd, err := os.Getwd(); err == nil && len(cwd) >= 2 && cwd[1] == ':' {
		return cwd[:3]
	}
	if sd := os.Getenv("SystemDrive"); sd != "" {
		return sd + "\\"
	}
	return "C:\\"
}
