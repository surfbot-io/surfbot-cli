package transport

import (
	"encoding/json"
	"time"

	"github.com/oklog/ulid/v2"
)

// EnvelopeVersion is the cli-side mirror of the server-side constant in
// surfbot-api/internal/cli/envelope.go. SPEC-CLI1 §6.2.
const EnvelopeVersion = 1

// EnvelopeType enumerates the message types we send & recognize. Unknown
// types from the server are ignored with a warn log (forward-compat).
type EnvelopeType string

const (
	TypeClientHello    EnvelopeType = "client.hello"
	TypeServerHello    EnvelopeType = "server.hello"
	TypeHeartbeat      EnvelopeType = "heartbeat"
	TypeHeartbeatAck   EnvelopeType = "heartbeat.ack"
	TypeServerShutdown EnvelopeType = "server.shutdown"
)

// Envelope wraps every TEXT frame on the surfbot.cli.v1 subprotocol.
type Envelope struct {
	V       int             `json:"v"`
	ID      string          `json:"id"`
	TS      time.Time       `json:"ts"`
	Type    EnvelopeType    `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// ClientHelloPayload is the first frame the cli writes after the handshake.
type ClientHelloPayload struct {
	AgentID      string   `json:"agent_id"`
	Version      string   `json:"version"`
	BuildCommit  string   `json:"build_commit"`
	OS           string   `json:"os"`
	Arch         string   `json:"arch"`
	Kernel       string   `json:"kernel"`
	Hostname     string   `json:"hostname"`
	Fingerprint  string   `json:"fingerprint"`
	Capabilities []string `json:"capabilities"`
}

// ServerHelloPayload mirrors the API-side type. SPEC-CLI1 §6.3.2.
type ServerHelloPayload struct {
	SessionID                string   `json:"session_id"`
	ServerVersion            string   `json:"server_version"`
	HeartbeatIntervalSeconds int      `json:"heartbeat_interval_seconds"`
	Capabilities             []string `json:"capabilities"`
	Warnings                 []string `json:"warnings"`
}

// HeartbeatPayload mirrors the API-side type. SPEC-CLI1 §6.3.3.
type HeartbeatPayload struct {
	UptimeSeconds int       `json:"uptime_seconds"`
	LoadAvg       []float64 `json:"load_avg"`
	MemUsedMB     int       `json:"mem_used_mb"`
	MemTotalMB    int       `json:"mem_total_mb"`
	DiskFreeGB    float64   `json:"disk_free_gb"`
}

// ServerShutdownPayload is parsed when type=server.shutdown and triggers
// accelerated reconnect (50ms instead of the normal 1s).
type ServerShutdownPayload struct {
	Reason                   string `json:"reason"`
	EstimatedDowntimeSeconds int    `json:"estimated_downtime_seconds"`
}

// NewEnvelope builds an outbound envelope with a fresh ULID id + RFC3339
// timestamp, ready to be written to the WebSocket.
func NewEnvelope(t EnvelopeType, payload any) ([]byte, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	e := Envelope{
		V:       EnvelopeVersion,
		ID:      ulid.Make().String(),
		TS:      time.Now().UTC(),
		Type:    t,
		Payload: raw,
	}
	return json.Marshal(e)
}
