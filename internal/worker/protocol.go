// Package worker implements the one detached runtime primitive owned by each
// active Session.
package worker

import (
	"encoding/json"
	"time"

	"github.com/yty-build/acp-gateway/internal/acp"
	"github.com/yty-build/acp-gateway/internal/sessionlog"
)

const (
	// ProtocolVersion is incremented when a new client-visible worker contract
	// must be detected before an operation can be performed safely.
	ProtocolVersion = 7

	// CapabilitySessionChannel is the complete v7 worker contract: one live
	// provider connection, one active turn, commands in, and ordered frames out.
	CapabilitySessionChannel = "session-channel-v1"
)

const (
	FrameConnection          = "connection"
	FrameClientCommand       = "client_command"
	FrameProviderEvent       = "provider_event"
	FrameProviderResult      = "provider_result"
	FramePermissionRequested = "permission_requested"
	FramePermissionResolved  = "permission_resolved"
	FramePermissionAbandoned = "permission_abandoned"
	FrameWorkerError         = "worker_error"
)

// BusyError is returned before a concurrent prompt is journaled or sent. It
// deliberately contains only gateway identifiers and a fixed message.
type BusyError struct {
	SessionID       string
	ActiveRequestID string
}

// NotReadyError means the Session Channel is already attachable, but its one
// provider Port has not finished Start/Resume yet. Clients may keep streaming
// connection frames and retry the command after `connection: connected`.
type NotReadyError struct {
	SessionID string
}

// ChannelUnavailableError means the retained provider Port can no longer be
// trusted or used. It is intentionally detail-free because bridge/protocol
// errors may contain provider-owned identifiers or local paths.
type ChannelUnavailableError struct{}

func (e *ChannelUnavailableError) Error() string {
	return "Session provider connection is unavailable; wait for this worker to stop, then resume the Session"
}

func (e *NotReadyError) Error() string {
	if e == nil || e.SessionID == "" {
		return "Session provider connection is still starting"
	}
	return "Session " + e.SessionID + " provider connection is still starting"
}

func (e *BusyError) Error() string {
	if e == nil || e.SessionID == "" {
		return "Session already has an active provider turn"
	}
	return "Session " + e.SessionID + " already has an active provider turn"
}

type Command struct {
	Type            string `json:"type"`
	SessionID       string `json:"session_id,omitempty"`
	From            uint64 `json:"from,omitempty"`
	RequestID       string `json:"request_id,omitempty"`
	Message         string `json:"message,omitempty"`
	Model           string `json:"model,omitempty"`
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	PermissionID    string `json:"permission_id,omitempty"`
	OptionID        string `json:"option_id,omitempty"`
	Cancelled       bool   `json:"cancelled,omitempty"`
}

type PermissionOption struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
}

type PendingPermission struct {
	ID        string             `json:"id"`
	RequestID string             `json:"request_id"`
	Title     string             `json:"title,omitempty"`
	Kind      string             `json:"kind,omitempty"`
	Options   []PermissionOption `json:"options"`
}

type Message struct {
	Type            string                     `json:"type"`
	SessionID       string                     `json:"session_id,omitempty"`
	ProtocolVersion int                        `json:"protocol_version,omitempty"`
	Capabilities    []string                   `json:"capabilities,omitempty"`
	State           string                     `json:"state,omitempty"`
	Status          string                     `json:"status,omitempty"`
	RequestID       string                     `json:"request_id,omitempty"`
	ActiveRequestID string                     `json:"active_request_id,omitempty"`
	Seq             uint64                     `json:"seq,omitempty"`
	Timestamp       time.Time                  `json:"timestamp,omitempty"`
	LineType        string                     `json:"line_type,omitempty"`
	Permissions     []PendingPermission        `json:"permissions,omitempty"`
	ProviderOptions *acp.ProviderOptionCatalog `json:"provider_options,omitempty"`
	Data            json.RawMessage            `json:"data,omitempty"`
	ErrorCode       string                     `json:"error_code,omitempty"`
	Message         string                     `json:"message,omitempty"`
}

func lineMessage(line sessionlog.Line) Message {
	return Message{Type: "line", Seq: line.Seq, Timestamp: line.Timestamp, LineType: line.Type, RequestID: line.RequestID, Data: append(json.RawMessage(nil), line.Data...)}
}
