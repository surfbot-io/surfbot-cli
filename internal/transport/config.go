package transport

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Config holds the optional user-overrideable settings loaded from
// $XDG_CONFIG_HOME/surfbot/config.yaml. SPEC-CLI1 §10.
//
// The format is a deliberately tiny line-oriented "key: value" — we ship
// without a YAML dependency in v1 because both fields are flat strings.
// (Switch to a real parser when a third field with non-trivial typing
// arrives.)
type Config struct {
	WSURL    string
	LogLevel string
}

// LoadDefault reads the per-user config file if present and applies sane
// defaults otherwise. Missing file is not an error.
func LoadDefault() (*Config, error) {
	dir, err := DefaultConfigDir()
	if err != nil {
		return nil, err
	}
	return LoadFromDir(dir)
}

// LoadFromDir is the test seam: read config.yaml from an explicit directory.
func LoadFromDir(dir string) (*Config, error) {
	c := &Config{LogLevel: "info"}
	path := filepath.Join(dir, "config.yaml")
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return c, nil
		}
		return nil, fmt.Errorf("config: read: %w", err)
	}
	for i, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			return nil, fmt.Errorf("config: line %d: missing colon", i+1)
		}
		val = strings.TrimSpace(val)
		val = strings.Trim(val, `"'`)
		switch strings.TrimSpace(key) {
		case "ws_url":
			c.WSURL = val
		case "log_level":
			c.LogLevel = val
		}
	}
	return c, nil
}

// ConfigPath returns the absolute path to the config file (without checking
// existence). Used by `status` to print the active config location.
func ConfigPath() string {
	dir, err := DefaultConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "config.yaml")
}
