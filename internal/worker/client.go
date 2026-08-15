package worker

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"time"
)

type SocketClient struct {
	Path    string
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
	if deadline, ok := ctx.Deadline(); ok {
		_ = connection.SetDeadline(deadline)
	} else if c.Timeout > 0 {
		_ = connection.SetDeadline(time.Now().Add(c.Timeout))
	}
	if err := json.NewEncoder(connection).Encode(command); err != nil {
		return Message{}, fmt.Errorf("send worker command: %w", err)
	}
	var message Message
	if err := json.NewDecoder(connection).Decode(&message); err != nil {
		return Message{}, fmt.Errorf("read worker response: %w", err)
	}
	if message.Type == "error" {
		return message, fmt.Errorf("worker: %s", message.Message)
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

func (c *SocketClient) Ping(ctx context.Context) error {
	message, err := c.Send(ctx, Command{Type: "ping"})
	if err != nil {
		return err
	}
	if message.Type != "pong" {
		return fmt.Errorf("unexpected worker ping response %q", message.Type)
	}
	return nil
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
