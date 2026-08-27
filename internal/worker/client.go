package worker

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"time"

	"github.com/yty-build/acp-gateway/internal/acp"
	"github.com/yty-build/acp-gateway/internal/sessionlog"
)

type SocketClient struct {
	Path string
	// Timeout bounds only establishment of the local worker connection. Command
	// completion is governed by the caller context and worker operation bounds;
	// it must not become ambiguous because of an implicit client-side timer.
	Timeout time.Duration
}

func NewSocketClient(path string) *SocketClient {
	return &SocketClient{Path: path, Timeout: 3 * time.Second}
}

func (c *SocketClient) Send(ctx context.Context, command Command) (Message, error) {
	connection, err := c.dial(ctx)
	if err != nil {
		return Message{}, err
	}
	defer connection.Close()
	stopContextWatch := closeConnectionWhenDone(ctx, connection)
	defer stopContextWatch()
	if err := json.NewEncoder(connection).Encode(command); err != nil {
		if ctx.Err() != nil {
			return Message{}, ctx.Err()
		}
		return Message{}, fmt.Errorf("send worker command: %w", err)
	}
	var message Message
	if err := json.NewDecoder(connection).Decode(&message); err != nil {
		if ctx.Err() != nil {
			return Message{}, ctx.Err()
		}
		return Message{}, fmt.Errorf("read worker response: %w", err)
	}
	if message.Type == "error" {
		switch message.ErrorCode {
		case "session_busy":
			return message, &BusyError{ActiveRequestID: message.ActiveRequestID}
		case "session_starting":
			return message, &NotReadyError{}
		case "session_channel_failed":
			return message, &ChannelUnavailableError{}
		case "provider_options_unavailable":
			return message, &acp.ProviderOptionsError{}
		case "provider_configuration_failed":
			return message, &acp.ProviderConfigurationError{}
		case "permission_response_rejected":
			return message, &acp.ProviderPermissionRejectedError{}
		}
		return message, fmt.Errorf("worker: %s", message.Message)
	}
	return message, nil
}

// Admit performs the private two-step worker-bootstrap handshake. Receiving
// admission_ready proves the Session Channel is addressable before the client
// sends the acknowledgement that permits provider Start/Resume to begin.
// Ordinary ping is observational and must never release provider startup.
func (c *SocketClient) Admit(ctx context.Context, sessionID string) (Message, error) {
	if sessionID == "" {
		return Message{}, fmt.Errorf("worker admission Session ID is required")
	}
	connection, err := c.dial(ctx)
	if err != nil {
		return Message{}, err
	}
	defer connection.Close()
	stopContextWatch := closeConnectionWhenDone(ctx, connection)
	defer stopContextWatch()
	encoder := json.NewEncoder(connection)
	decoder := json.NewDecoder(connection)
	if err := encoder.Encode(Command{Type: "_admit", SessionID: sessionID}); err != nil {
		if ctx.Err() != nil {
			return Message{}, ctx.Err()
		}
		return Message{}, fmt.Errorf("send worker admission probe: %w", err)
	}
	var message Message
	if err := decoder.Decode(&message); err != nil {
		if ctx.Err() != nil {
			return Message{}, ctx.Err()
		}
		return Message{}, fmt.Errorf("read worker admission response: %w", err)
	}
	if message.Type == "error" {
		return message, fmt.Errorf("worker: %s", message.Message)
	}
	if message.Type != "admission_ready" || message.SessionID != sessionID || message.ProtocolVersion != ProtocolVersion || !messageHasCapability(message, CapabilitySessionChannel) {
		return Message{}, fmt.Errorf("unexpected worker admission response %q", message.Type)
	}
	if err := encoder.Encode(Command{Type: "_admit_ack", SessionID: sessionID}); err != nil {
		if ctx.Err() != nil {
			return Message{}, ctx.Err()
		}
		return Message{}, fmt.Errorf("acknowledge worker admission: %w", err)
	}
	return message, nil
}

func (c *SocketClient) Subscribe(ctx context.Context, from uint64, consume func(Message) error) error {
	return c.stream(ctx, Command{Type: "subscribe", From: from}, false, consume)
}

func (c *SocketClient) Tail(ctx context.Context, from uint64, consume func(Message) error) error {
	return c.stream(ctx, Command{Type: "tail", From: from}, true, consume)
}

func (c *SocketClient) stream(ctx context.Context, command Command, stopAtEnd bool, consume func(Message) error) error {
	connection, err := c.dial(ctx)
	if err != nil {
		return err
	}
	defer connection.Close()
	if err := json.NewEncoder(connection).Encode(command); err != nil {
		return fmt.Errorf("send worker stream command: %w", err)
	}
	watchDone := make(chan struct{})
	defer close(watchDone)
	go func() {
		select {
		case <-ctx.Done():
			_ = connection.Close()
		case <-watchDone:
		}
	}()
	scanner := bufio.NewScanner(connection)
	scanner.Buffer(make([]byte, 64*1024), 20<<20)
	for scanner.Scan() {
		var message Message
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
			return fmt.Errorf("decode worker stream response: %w", err)
		}
		if message.Type == "error" {
			if message.ErrorCode == "replay_cursor_gap" {
				return &sessionlog.CursorGapError{Cursor: command.From, Last: message.Seq}
			}
			return fmt.Errorf("worker: %s", message.Message)
		}
		if stopAtEnd && message.Type == "end" {
			return nil
		}
		if err := consume(message); err != nil {
			return err
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read worker stream: %w", err)
	}
	return nil
}

func closeConnectionWhenDone(ctx context.Context, connection net.Conn) func() {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = connection.Close()
		case <-done:
		}
	}()
	return func() { close(done) }
}

func (c *SocketClient) Ping(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return fmt.Errorf("worker ping Session ID is required")
	}
	message, err := c.Send(ctx, Command{Type: "ping", SessionID: sessionID})
	if err != nil {
		return err
	}
	if message.Type != "pong" || message.SessionID != sessionID || message.ProtocolVersion < ProtocolVersion || !messageHasCapability(message, CapabilitySessionChannel) {
		return fmt.Errorf("unexpected worker ping response %q", message.Type)
	}
	return nil
}

func messageHasCapability(message Message, capability string) bool {
	for _, current := range message.Capabilities {
		if current == capability {
			return true
		}
	}
	return false
}

func (c *SocketClient) dial(ctx context.Context) (net.Conn, error) {
	if c.Path == "" {
		return nil, fmt.Errorf("worker socket path is required")
	}
	dialer := net.Dialer{Timeout: c.Timeout}
	connection, err := dialer.DialContext(ctx, "unix", c.Path)
	if err != nil {
		return nil, fmt.Errorf("connect worker socket %q: %w", c.Path, err)
	}
	return connection, nil
}
