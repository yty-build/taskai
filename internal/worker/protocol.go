// Package worker implements the one detached runtime primitive owned by each
// active Session.
package worker

import (
	"encoding/json"
	"time"

	"github.com/yty-build/acp-gateway/internal/sessionlog"
)

type Command struct {
	Type      string `json:"type"`
	From      uint64 `json:"from,omitempty"`
	RequestID string `json:"request_id,omitempty"`
	Message   string `json:"message,omitempty"`
}

type Message struct {
	Type            string          `json:"type"`
	State           string          `json:"state,omitempty"`
	Status          string          `json:"status,omitempty"`
	RequestID       string          `json:"request_id,omitempty"`
	ActiveRequestID string          `json:"active_request_id,omitempty"`
	Seq             uint64          `json:"seq,omitempty"`
	Timestamp       time.Time       `json:"timestamp,omitempty"`
	LineType        string          `json:"line_type,omitempty"`
	Queued          int             `json:"queued,omitempty"`
	NativeLease     bool            `json:"native_lease,omitempty"`
	Data            json.RawMessage `json:"data,omitempty"`
	Message         string          `json:"message,omitempty"`
}

func lineMessage(line sessionlog.Line) Message {
	return Message{Type: "line", Seq: line.Seq, Timestamp: line.Timestamp, LineType: line.Type, RequestID: line.RequestID, Data: append(json.RawMessage(nil), line.Data...)}
}
