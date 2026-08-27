package acp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

const (
	// ProtocolVersion is the only ACP protocol generation implemented here.
	ProtocolVersion = 1
	// MaxFrameBytes is the strict maximum JSON payload size, excluding the
	// newline delimiter.
	MaxFrameBytes = 128 << 10
)

var (
	ErrFrameTooLarge    = errors.New("acp: frame exceeds 128 KiB")
	ErrMalformedFrame   = errors.New("acp: malformed JSON-RPC frame")
	ErrUnexpectedReply  = errors.New("acp: response has no pending request")
	ErrTransportClosed  = errors.New("acp: transport closed")
	ErrEndpointNotReady = errors.New("acp: endpoint is not running")
)

// Kind describes only the JSON-RPC envelope. It deliberately says nothing
// about the semantic meaning of ACP content.
type Kind uint8

const (
	KindInvalid Kind = iota
	KindRequest
	KindNotification
	KindResponse
)

// ErrorObject is the public JSON-RPC error shape. Data remains opaque.
type ErrorObject struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Message is a thin JSON-RPC envelope. Raw is the exact frame payload as read
// from the wire. Params, Result, Error.Data, and Raw are never projected into a
// normalized ACP event model.
type Message struct {
	Raw     json.RawMessage `json:"-"`
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *ErrorObject    `json:"error,omitempty"`
}

// ParseMessage validates a single bounded JSON-RPC 2.0 object while retaining
// an exact copy of its bytes.
func ParseMessage(raw []byte) (Message, error) {
	if len(raw) > MaxFrameBytes {
		return Message{}, ErrFrameTooLarge
	}
	if len(raw) == 0 || bytes.IndexByte(raw, '\n') >= 0 || bytes.IndexByte(raw, '\r') >= 0 {
		return Message{}, ErrMalformedFrame
	}

	var m Message
	if err := json.Unmarshal(raw, &m); err != nil {
		return Message{}, ErrMalformedFrame
	}
	m.Raw = append(json.RawMessage(nil), raw...)
	if err := m.validate(); err != nil {
		return Message{}, err
	}
	return m, nil
}

// Kind reports the request, notification, or response envelope kind.
func (m Message) Kind() Kind {
	switch {
	case m.Method != "" && len(m.ID) != 0:
		return KindRequest
	case m.Method != "" && len(m.ID) == 0:
		return KindNotification
	case m.Method == "" && len(m.ID) != 0:
		return KindResponse
	default:
		return KindInvalid
	}
}

func (m Message) validate() error {
	if m.JSONRPC != "2.0" {
		return ErrMalformedFrame
	}
	if len(m.ID) != 0 {
		if _, err := correlationKey(m.ID); err != nil {
			return ErrMalformedFrame
		}
	}
	if len(m.Params) != 0 && !json.Valid(m.Params) {
		return ErrMalformedFrame
	}
	if len(m.Result) != 0 && !json.Valid(m.Result) {
		return ErrMalformedFrame
	}
	if m.Error != nil && len(m.Error.Data) != 0 && !json.Valid(m.Error.Data) {
		return ErrMalformedFrame
	}

	switch m.Kind() {
	case KindRequest, KindNotification:
		if len(m.Result) != 0 || m.Error != nil {
			return ErrMalformedFrame
		}
	case KindResponse:
		if (len(m.Result) == 0) == (m.Error == nil) {
			return ErrMalformedFrame
		}
	default:
		return ErrMalformedFrame
	}
	return nil
}

// Bytes returns the exact received frame, or a compact encoding for a locally
// constructed message.
func (m Message) Bytes() ([]byte, error) {
	if len(m.Raw) != 0 {
		if _, err := ParseMessage(m.Raw); err != nil {
			return nil, err
		}
		return append([]byte(nil), m.Raw...), nil
	}
	if err := m.validate(); err != nil {
		return nil, err
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("acp: encode envelope: %w", err)
	}
	if len(b) > MaxFrameBytes {
		return nil, ErrFrameTooLarge
	}
	return b, nil
}

// Request constructs a JSON-RPC request with an opaque params value.
func Request(id json.RawMessage, method string, params json.RawMessage) Message {
	return Message{JSONRPC: "2.0", ID: cloneRaw(id), Method: method, Params: cloneRaw(params)}
}

// Notification constructs a JSON-RPC notification with opaque params.
func Notification(method string, params json.RawMessage) Message {
	return Message{JSONRPC: "2.0", Method: method, Params: cloneRaw(params)}
}

// Response constructs a successful response with an opaque result.
func Response(id, result json.RawMessage) Message {
	return Message{JSONRPC: "2.0", ID: cloneRaw(id), Result: cloneRaw(result)}
}

// ErrorResponse constructs a failed response.
func ErrorResponse(id json.RawMessage, rpcErr *ErrorObject) Message {
	return Message{JSONRPC: "2.0", ID: cloneRaw(id), Error: rpcErr}
}

func cloneRaw(v json.RawMessage) json.RawMessage {
	return append(json.RawMessage(nil), v...)
}

func correlationKey(id json.RawMessage) (string, error) {
	if len(id) == 0 || bytes.Equal(bytes.TrimSpace(id), []byte("null")) {
		return "", ErrMalformedFrame
	}
	dec := json.NewDecoder(bytes.NewReader(id))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return "", err
	}
	switch v := value.(type) {
	case string:
		return "s:" + v, nil
	case json.Number:
		if _, err := v.Int64(); err != nil {
			return "", err
		}
		return "n:" + v.String(), nil
	default:
		return "", ErrMalformedFrame
	}
}
