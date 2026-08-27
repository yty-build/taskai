package worker

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSocketClientTimeoutBoundsDialNotCommandCompletion(t *testing.T) {
	listener, socket := listenWorkerClientTestSocket(t)
	received := make(chan struct{})
	release := make(chan struct{})
	serverDone := make(chan error, 1)
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		defer connection.Close()
		var command Command
		if err := json.NewDecoder(connection).Decode(&command); err != nil {
			serverDone <- err
			return
		}
		close(received)
		<-release
		serverDone <- json.NewEncoder(connection).Encode(Message{Type: "status", Status: "waiting"})
	}()

	client := &SocketClient{Path: socket, Timeout: 20 * time.Millisecond}
	type result struct {
		message Message
		err     error
	}
	resultReady := make(chan result, 1)
	go func() {
		message, err := client.Send(context.Background(), Command{Type: "status"})
		resultReady <- result{message: message, err: err}
	}()
	<-received
	select {
	case outcome := <-resultReady:
		t.Fatalf("command inherited dial timeout: %#v, %v", outcome.message, outcome.err)
	case <-time.After(3 * client.Timeout):
	}
	close(release)
	select {
	case outcome := <-resultReady:
		if outcome.err != nil || outcome.message.Type != "status" {
			t.Fatalf("command result = %#v, %v", outcome.message, outcome.err)
		}
	case <-time.After(time.Second):
		t.Fatal("command did not complete after server response")
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

func TestSocketClientCancellationInterruptsCommandRead(t *testing.T) {
	listener, socket := listenWorkerClientTestSocket(t)
	received := make(chan struct{})
	serverDone := make(chan struct{})
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			close(serverDone)
			return
		}
		defer connection.Close()
		var command Command
		if json.NewDecoder(connection).Decode(&command) == nil {
			close(received)
			var ignored [1]byte
			_, _ = connection.Read(ignored[:])
		}
		close(serverDone)
	}()

	ctx, cancel := context.WithCancel(context.Background())
	resultReady := make(chan error, 1)
	go func() {
		_, err := (&SocketClient{Path: socket, Timeout: time.Second}).Send(ctx, Command{Type: "status"})
		resultReady <- err
	}()
	<-received
	cancel()
	select {
	case err := <-resultReady:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled command error = %T %v", err, err)
		}
	case <-time.After(time.Second):
		t.Fatal("context cancellation did not interrupt command read")
	}
	select {
	case <-serverDone:
	case <-time.After(time.Second):
		t.Fatal("client cancellation did not close its worker connection")
	}
}

func listenWorkerClientTestSocket(t *testing.T) (net.Listener, string) {
	t.Helper()
	root, err := os.MkdirTemp("", "acp-client-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	socket := filepath.Join(root, "worker.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return listener, socket
}
