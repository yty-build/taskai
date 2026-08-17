// Package worker implements the one detached runtime primitive owned by each
// active Session.
package worker

import (
	"encoding/json"
	"time"

	"github.com/yty-build/acp-gateway/internal/sessionlog"
)

const (
	// ProtocolVersion is incremented when a new client-visible worker contract
	// must be detected before an operation can be performed safely.
	ProtocolVersion = 3

	// CapabilityExclusiveNativeOwner means the worker durably prevents both a
	// second native process and concurrent ACP/native writes across detach and
	// worker restart, while retaining exact provider-conversation freshness.
	CapabilityExclusiveNativeOwner = "exclusive-native-owner-v2"

	// NativeOwnerRoleReservation is used by an outer tmux viewer before it has
	// started the private provider-owning process. Only this phase is eligible
	// for explicit missing-harness recovery.
	NativeOwnerRoleReservation = "reservation"

	// NativeOwnerRoleProvider marks the private process that can actually write
	// the provider conversation. An active provider owner is never recoverable
	// merely because a tmux Session cannot be found.
	NativeOwnerRoleProvider = "provider"

	// NativeOwnerRoleDirect is an outer foreground process that writes the
	// provider conversation without a tmux reservation.
	NativeOwnerRoleDirect = "direct"
)

type Command struct {
	Type             string `json:"type"`
	From             uint64 `json:"from,omitempty"`
	RequestID        string `json:"request_id,omitempty"`
	Message          string `json:"message,omitempty"`
	DisplayMessage   string `json:"display_message,omitempty"`
	PermissionID     string `json:"permission_id,omitempty"`
	OptionID         string `json:"option_id,omitempty"`
	Cancelled        bool   `json:"cancelled,omitempty"`
	OwnerToken       string `json:"owner_token,omitempty"`
	NativeOwnerRole  string `json:"native_owner_role,omitempty"`
	NativeGeneration string `json:"native_generation,omitempty"`
	NativeActivity   bool   `json:"native_activity,omitempty"`
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
	Type             string              `json:"type"`
	ProtocolVersion  int                 `json:"protocol_version,omitempty"`
	Capabilities     []string            `json:"capabilities,omitempty"`
	State            string              `json:"state,omitempty"`
	Status           string              `json:"status,omitempty"`
	RequestID        string              `json:"request_id,omitempty"`
	ActiveRequestID  string              `json:"active_request_id,omitempty"`
	Seq              uint64              `json:"seq,omitempty"`
	Timestamp        time.Time           `json:"timestamp,omitempty"`
	LineType         string              `json:"line_type,omitempty"`
	Queued           int                 `json:"queued,omitempty"`
	NativeOwned      bool                `json:"native_owned,omitempty"`
	NativeGeneration string              `json:"native_generation,omitempty"`
	Permissions      []PendingPermission `json:"permissions,omitempty"`
	Data             json.RawMessage     `json:"data,omitempty"`
	Message          string              `json:"message,omitempty"`
}

func lineMessage(line sessionlog.Line) Message {
	return Message{Type: "line", Seq: line.Seq, Timestamp: line.Timestamp, LineType: line.Type, RequestID: line.RequestID, Data: append(json.RawMessage(nil), line.Data...)}
}
