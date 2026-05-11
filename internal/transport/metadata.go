package transport

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// AgentMetadata is the non-secret bookkeeping written next to the token at
// enroll/login time. It exists so `surfbot-cli status` can report meaningful
// info without an extra REST round-trip (the spec lists a `/cli/whoami`
// endpoint that does not exist in PR2.api at the time of writing — until it
// lands, status falls back to this file).
type AgentMetadata struct {
	AgentID     string `json:"agent_id"`
	OrgID       string `json:"org_id"`
	WSURL       string `json:"ws_url"`
	Hostname    string `json:"hostname"`
	TokenPrefix string `json:"token_prefix"`
	EnrolledAt  string `json:"enrolled_at"`
	Method      string `json:"method"`
}

// SaveMetadata writes the metadata file atomically (perms 0600 — even though
// it contains no secret, mirroring the token's perms keeps the pair tidy).
func SaveMetadata(dir string, m *AgentMetadata) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("metadata: mkdir: %w", err)
	}
	path := filepath.Join(dir, MetadataFileName)
	tmp, err := os.CreateTemp(dir, ".agent.json.*")
	if err != nil {
		return fmt.Errorf("metadata: tempfile: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("metadata: encode: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("metadata: sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("metadata: close: %w", err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		cleanup()
		return fmt.Errorf("metadata: chmod: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return fmt.Errorf("metadata: rename: %w", err)
	}
	return nil
}

// LoadMetadata returns (nil, nil) when the file does not exist.
func LoadMetadata(dir string) (*AgentMetadata, error) {
	path := filepath.Join(dir, MetadataFileName)
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("metadata: read: %w", err)
	}
	var m AgentMetadata
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("metadata: decode: %w", err)
	}
	return &m, nil
}

// DeleteMetadata removes the metadata file. Missing is not an error.
func DeleteMetadata(dir string) error {
	path := filepath.Join(dir, MetadataFileName)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("metadata: remove: %w", err)
	}
	return nil
}
