package acp

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestClientCorrelatesReorderedResponses(t *testing.T) {
	t.Parallel()
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() {
		_ = clientConn.Close()
		_ = serverConn.Close()
	})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	client := NewClient(clientConn, clientConn, nil)
	if err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}

	serverDone := make(chan error, 1)
	go func() {
		decoder := NewDecoder(serverConn)
		encoder := NewEncoder(serverConn)
		first, err := decoder.Decode()
		if err != nil {
			serverDone <- err
			return
		}
		second, err := decoder.Decode()
		if err != nil {
			serverDone <- err
			return
		}
		for _, request := range []Message{second, first} {
			result, _ := json.Marshal(map[string]string{"method": request.Method})
			if err := encoder.Encode(Response(request.ID, result)); err != nil {
				serverDone <- err
				return
			}
		}
		serverDone <- nil
	}()

	type outcome struct {
		method string
		got    string
		err    error
	}
	results := make(chan outcome, 2)
	for _, method := range []string{"first", "second"} {
		method := method
		go func() {
			raw, err := client.Call(ctx, method, json.RawMessage(`{"opaque":"same"}`))
			if err != nil {
				results <- outcome{method: method, err: err}
				return
			}
			var value map[string]string
			err = json.Unmarshal(raw, &value)
			results <- outcome{method: method, got: value["method"], err: err}
		}()
	}
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatalf("Call(%q) error = %v", result.method, result.err)
		}
		if result.got != result.method {
			t.Fatalf("Call(%q) correlated result from %q", result.method, result.got)
		}
	}
	if err := <-serverDone; err != nil {
		t.Fatalf("server error = %v", err)
	}
}

func TestNotificationsAreHandledInWireOrderWithBackpressure(t *testing.T) {
	t.Parallel()
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() {
		_ = clientConn.Close()
		_ = serverConn.Close()
	})

	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondHandled := make(chan struct{})
	var (
		mu    sync.Mutex
		order []int
	)
	handler := HandlerFuncs{Notification: func(_ context.Context, message Message) error {
		var params struct {
			Sequence int `json:"sequence"`
		}
		if err := json.Unmarshal(message.Params, &params); err != nil {
			return err
		}
		if params.Sequence == 1 {
			close(firstEntered)
			<-releaseFirst
		}
		mu.Lock()
		order = append(order, params.Sequence)
		mu.Unlock()
		if params.Sequence == 2 {
			close(secondHandled)
		}
		return nil
	}}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	client := NewClient(clientConn, clientConn, handler)
	_ = client.Start(ctx)
	writeDone := make(chan error, 1)
	go func() {
		encoder := NewEncoder(serverConn)
		if err := encoder.Encode(Notification(MethodSessionUpdate, json.RawMessage(`{"sequence":1,"future":{"x":true}}`))); err != nil {
			writeDone <- err
			return
		}
		writeDone <- encoder.Encode(Notification(MethodSessionUpdate, json.RawMessage(`{"sequence":2,"future":{"x":false}}`)))
	}()

	<-firstEntered
	select {
	case <-secondHandled:
		t.Fatal("second notification overtook blocked first notification")
	default:
	}
	close(releaseFirst)
	<-secondHandled
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	got := append([]int(nil), order...)
	mu.Unlock()
	if !reflect.DeepEqual(got, []int{1, 2}) {
		t.Fatalf("notification order = %v", got)
	}
}

func TestAgentRequestCancellation(t *testing.T) {
	t.Parallel()
	clientConn, agentConn := net.Pipe()
	t.Cleanup(func() {
		_ = clientConn.Close()
		_ = agentConn.Close()
	})

	requestStarted := make(chan struct{})
	agent := NewAgent(agentConn, agentConn, HandlerFuncs{Request: func(ctx context.Context, message Message) (json.RawMessage, *ErrorObject) {
		if message.Method != MethodSessionPrompt {
			return nil, &ErrorObject{Code: -32601, Message: "unsupported"}
		}
		close(requestStarted)
		<-ctx.Done()
		return nil, &ErrorObject{Code: -32800, Message: "cancelled-private-canary"}
	}})
	client := NewClient(clientConn, clientConn, nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	_ = agent.Start(ctx)
	_ = client.Start(ctx)

	callDone := make(chan error, 1)
	go func() {
		_, err := client.Call(ctx, MethodSessionPrompt, json.RawMessage(`{"sessionId":"owned"}`))
		callDone <- err
	}()
	<-requestStarted
	if err := client.Notify(MethodCancelRequest, json.RawMessage(`{"id":1}`)); err != nil {
		t.Fatalf("cancel notification error = %v", err)
	}
	err := <-callDone
	var remote *RemoteError
	if !errors.As(err, &remote) || remote.Code != -32800 {
		t.Fatalf("Call() error = %v, want remote cancellation", err)
	}
	if strings.Contains(err.Error(), "private-canary") {
		t.Fatalf("error exposed remote content: %v", err)
	}
}

func TestPeerRejectsDuplicateLiveInboundRequestID(t *testing.T) {
	t.Parallel()
	remote, local := net.Pipe()
	t.Cleanup(func() {
		_ = remote.Close()
		_ = local.Close()
	})
	entered := make(chan struct{})
	var once sync.Once
	peer := NewAgent(local, local, HandlerFuncs{Request: func(ctx context.Context, _ Message) (json.RawMessage, *ErrorObject) {
		once.Do(func() { close(entered) })
		<-ctx.Done()
		return nil, &ErrorObject{Code: -32800, Message: "cancelled"}
	}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := peer.Start(ctx); err != nil {
		t.Fatal(err)
	}
	encoder := NewEncoder(remote)
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- encoder.Encode(Request(json.RawMessage(`7`), MethodSessionPrompt, json.RawMessage(`{}`)))
	}()
	<-entered
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := encoder.Encode(Request(json.RawMessage(`7`), MethodSessionPrompt, json.RawMessage(`{}`))); err != nil {
		t.Fatal(err)
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- peer.Wait() }()
	select {
	case err := <-waitDone:
		if !errors.Is(err, ErrMalformedFrame) {
			t.Fatalf("duplicate inbound terminal = %v, want malformed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("duplicate inbound request did not terminate peer")
	}
}
