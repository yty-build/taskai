package runtime

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/yty-build/acp-gateway/internal/session"
	"github.com/yty-build/acp-gateway/internal/settings"
	"github.com/yty-build/acp-gateway/internal/task"
)

const (
	ipcProtocolVersion      = 1
	ipcRoleEvents           = "events"
	ipcRoleAction           = "action"
	ipcMethodSnapshot       = "snapshot"
	ipcMethodAddTask        = "add_task"
	ipcMethodAddManager     = "add_manager"
	ipcMethodSetEnabled     = "set_enabled"
	ipcMethodAuthenticate   = "authenticate"
	ipcMethodRecheck        = "recheck"
	ipcMethodAttach         = "attach"
	ipcMethodDetach         = "detach"
	ipcMethodSend           = "send"
	ipcMethodResume         = "resume"
	ipcMethodStop           = "stop"
	ipcMethodNext           = "next"
	ipcRequestIDWidth       = 16
	maxIPCActionConnections = 64
	maxIPCClientActions     = 8
)

type localEndpoint interface {
	Accept() (net.Conn, error)
	Interrupt() error
	Close() error
}

// IPCServer exposes one Supervisor-compatible backend over a restrictive
// local socket. It is deliberately a framed protocol rather than HTTP,
// net/rpc, or a generic message broker.
type IPCServer struct {
	backend  ClientAcceptor
	endpoint localEndpoint

	mu      sync.Mutex
	serving bool
	closed  bool
	ctx     context.Context
	cancel  context.CancelFunc
	clients map[string]*ipcServerClient
	conns   map[net.Conn]struct{}

	connectionSlots chan struct{}
	actionSlots     chan struct{}
	wg              sync.WaitGroup
	closeOnce       sync.Once
	closeErr        error
}

type ipcServerClient struct {
	id        string
	port      ClientPort
	event     net.Conn
	actionMu  sync.Mutex
	closeOnce sync.Once
}

func (client *ipcServerClient) close() {
	client.closeOnce.Do(func() {
		if client.port != nil {
			_ = client.port.Close()
		}
		if client.event != nil {
			_ = client.event.Close()
		}
	})
}

// NewIPCServer acquires and binds the local endpoint. Serve performs the
// blocking accept loop. Holding the endpoint before Serve lets a worker
// publish readiness without a connect/retry race.
func NewIPCServer(stateDir string, backend ClientAcceptor) (*IPCServer, error) {
	if backend == nil {
		return nil, ErrInvalidRequest
	}
	endpoint, err := listenLocalEndpoint(stateDir)
	if err != nil {
		return nil, err
	}
	return &IPCServer{
		backend: backend, endpoint: endpoint,
		clients: make(map[string]*ipcServerClient), conns: make(map[net.Conn]struct{}),
		connectionSlots: make(chan struct{}, MaxSubscriberCount+maxIPCActionConnections),
		actionSlots:     make(chan struct{}, maxIPCActionConnections),
	}, nil
}

// Serve blocks until ctx is cancelled, Close is called, or the endpoint
// fails. It may be called exactly once.
func (server *IPCServer) Serve(ctx context.Context) error {
	if server == nil || ctx == nil || ctx.Err() != nil {
		return ErrInvalidRequest
	}
	server.mu.Lock()
	if server.closed {
		server.mu.Unlock()
		return ErrClosed
	}
	if server.serving {
		server.mu.Unlock()
		return ErrConflict
	}
	server.serving = true
	server.ctx, server.cancel = context.WithCancel(ctx)
	serveCtx := server.ctx
	server.mu.Unlock()

	// Interrupt closes only the listener. The ownership lock remains held until
	// Close so the app can stop its Supervisor before admitting a successor.
	stopAccept := context.AfterFunc(serveCtx, func() { _ = server.endpoint.Interrupt() })
	defer stopAccept()
	for {
		connection, err := server.endpoint.Accept()
		if err != nil {
			server.mu.Lock()
			closed := server.closed
			server.mu.Unlock()
			if closed {
				return nil
			}
			if serveCtx.Err() != nil {
				return ErrCancelled
			}
			return ErrTransport
		}
		select {
		case server.connectionSlots <- struct{}{}:
		default:
			_ = connection.Close()
			continue
		}
		server.mu.Lock()
		if server.closed {
			server.mu.Unlock()
			<-server.connectionSlots
			_ = connection.Close()
			return nil
		}
		server.conns[connection] = struct{}{}
		server.wg.Add(1)
		server.mu.Unlock()
		go server.serveConnection(connection)
	}
}

func (server *IPCServer) serveConnection(connection net.Conn) {
	defer func() {
		_ = connection.Close()
		server.mu.Lock()
		delete(server.conns, connection)
		server.mu.Unlock()
		<-server.connectionSlots
		server.wg.Done()
	}()
	if err := verifyLocalPeer(connection); err != nil {
		return
	}
	var hello wireHello
	if readWireFrame(connection, &hello) != nil || validateHello(hello) != nil {
		_ = writeWireFrame(connection, wireHelloResponse{Protocol: ipcProtocolVersion, Error: CodeInvalidRequest})
		return
	}
	switch hello.Role {
	case ipcRoleEvents:
		server.serveEventConnection(connection, hello.ClientID)
	case ipcRoleAction:
		select {
		case server.actionSlots <- struct{}{}:
			defer func() { <-server.actionSlots }()
		default:
			_ = writeWireFrame(connection, wireHelloResponse{Protocol: ipcProtocolVersion, Error: CodeBusy})
			return
		}
		client := server.lookupClient(hello.ClientID)
		if client == nil {
			_ = writeWireFrame(connection, wireHelloResponse{Protocol: ipcProtocolVersion, Error: CodeUnavailable})
			return
		}
		if writeWireFrame(connection, wireHelloResponse{Protocol: ipcProtocolVersion}) != nil {
			return
		}
		server.serveActionConnection(connection, client)
	}
}

func (server *IPCServer) serveEventConnection(connection net.Conn, clientID string) {
	server.mu.Lock()
	if server.closed || server.clients[clientID] != nil {
		server.mu.Unlock()
		_ = writeWireFrame(connection, wireHelloResponse{Protocol: ipcProtocolVersion, Error: CodeConflict})
		return
	}
	server.mu.Unlock()
	port, err := server.backend.OpenClient(clientID)
	if err != nil || port == nil {
		if err == nil {
			err = ErrTransport
		}
		_ = writeWireFrame(connection, wireHelloResponse{Protocol: ipcProtocolVersion, Error: codeOf(err)})
		return
	}
	client := &ipcServerClient{id: clientID, port: port, event: connection}
	server.mu.Lock()
	if server.closed || server.clients[clientID] != nil {
		server.mu.Unlock()
		client.close()
		_ = writeWireFrame(connection, wireHelloResponse{Protocol: ipcProtocolVersion, Error: CodeConflict})
		return
	}
	server.clients[clientID] = client
	server.mu.Unlock()
	defer server.removeClient(clientID, client)
	if writeWireFrame(connection, wireHelloResponse{Protocol: ipcProtocolVersion}) != nil {
		return
	}

	server.mu.Lock()
	serveCtx := server.ctx
	server.mu.Unlock()
	if serveCtx == nil {
		return
	}
	connectionCtx, cancelConnection := context.WithCancel(serveCtx)
	type eventRequest struct {
		id     string
		cursor Cursor
	}
	requests := make(chan eventRequest, 1)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		defer cancelConnection()
		var expected uint64 = 1
		for {
			var request wireRequest
			if readWireFrame(connection, &request) != nil {
				clear(request.Params)
				return
			}
			requestNumber, requestErr := parseRequestID(request.ID)
			if requestErr != nil || requestNumber != expected || request.Protocol != ipcProtocolVersion || request.Method != ipcMethodNext {
				clear(request.Params)
				return
			}
			expected++
			var params wireNextParams
			if decodeWireParams(request.Params, &params) != nil {
				clear(request.Params)
				return
			}
			clear(request.Params)
			select {
			case requests <- eventRequest{id: request.ID, cursor: params.Cursor}:
			case <-connectionCtx.Done():
				return
			}
		}
	}()
	defer func() {
		cancelConnection()
		_ = connection.Close()
		<-readerDone
	}()

	for {
		select {
		case request := <-requests:
			event, nextErr := client.port.Next(connectionCtx, request.cursor)
			if connectionCtx.Err() != nil {
				return
			}
			response := wireResponse{Protocol: ipcProtocolVersion, ID: request.id}
			if nextErr != nil {
				response.Error = codeOf(nextErr)
			} else {
				response.Result, nextErr = marshalWireResult(wireNextResult{Event: event})
				if nextErr != nil {
					response.Error = codeOf(nextErr)
				}
			}
			if writeWireFrame(connection, response) != nil {
				clear(response.Result)
				return
			}
			clear(response.Result)
		case <-connectionCtx.Done():
			return
		}
	}
}

func (server *IPCServer) serveActionConnection(connection net.Conn, client *ipcServerClient) {
	var request wireRequest
	if readWireFrame(connection, &request) != nil || validateWireRequest(request) != nil || request.Method == ipcMethodNext {
		_ = writeWireFrame(connection, wireResponse{
			Protocol: ipcProtocolVersion, ID: request.ID, Error: CodeInvalidRequest,
		})
		return
	}
	client.actionMu.Lock()
	result, callErr, closeClient := server.dispatchAction(client, request.Method, request.Params)
	clear(request.Params)
	client.actionMu.Unlock()
	response := wireResponse{Protocol: ipcProtocolVersion, ID: request.ID, Result: result}
	if callErr != nil {
		clear(result)
		response.Result = nil
		response.Error = codeOf(callErr)
	}
	_ = writeWireFrame(connection, response)
	clear(response.Result)
	if closeClient {
		server.removeClient(client.id, client)
	}
}

func (server *IPCServer) dispatchAction(client *ipcServerClient, method string, raw json.RawMessage) (json.RawMessage, error, bool) {
	server.mu.Lock()
	ctx := server.ctx
	server.mu.Unlock()
	if ctx == nil || ctx.Err() != nil {
		return nil, ErrClosed, false
	}
	port := client.port
	switch method {
	case ipcMethodSnapshot:
		if decodeWireParams(raw, &wireNoParams{}) != nil {
			return nil, ErrInvalidRequest, false
		}
		value, err := port.Snapshot(ctx)
		return marshalActionResult(wireSnapshotResult{Snapshot: value}, err)
	case ipcMethodAddTask:
		var params wireAddTaskParams
		if decodeWireParams(raw, &params) != nil {
			return nil, ErrInvalidRequest, false
		}
		id, err := port.AddTask(ctx, params.Title)
		return marshalActionResult(wireTaskIDResult{TaskID: id}, err)
	case ipcMethodAddManager:
		var params wireAddManagerParams
		if decodeWireParams(raw, &params) != nil {
			return nil, ErrInvalidRequest, false
		}
		id, err := port.AddManager(ctx, params.TaskID, params.ProviderID, params.Label)
		return marshalActionResult(wireSessionIDResult{SessionID: id}, err)
	case ipcMethodSetEnabled:
		var params wireProviderEnabledParams
		if decodeWireParams(raw, &params) != nil {
			return nil, ErrInvalidRequest, false
		}
		return marshalActionResult(wireNoResult{}, port.SetEnabled(ctx, params.ProviderID, params.Enabled))
	case ipcMethodAuthenticate:
		var params wireProviderParams
		if decodeWireParams(raw, &params) != nil {
			return nil, ErrInvalidRequest, false
		}
		return marshalActionResult(wireNoResult{}, port.Authenticate(ctx, params.ProviderID))
	case ipcMethodRecheck:
		var params wireProviderParams
		if decodeWireParams(raw, &params) != nil {
			return nil, ErrInvalidRequest, false
		}
		return marshalActionResult(wireNoResult{}, port.Recheck(ctx, params.ProviderID))
	case ipcMethodAttach:
		var params wireTaskParams
		if decodeWireParams(raw, &params) != nil {
			return nil, ErrInvalidRequest, false
		}
		return marshalActionResult(wireNoResult{}, port.Attach(ctx, params.TaskID))
	case ipcMethodDetach:
		var params wireTaskParams
		if decodeWireParams(raw, &params) != nil {
			return nil, ErrInvalidRequest, false
		}
		return marshalActionResult(wireNoResult{}, port.Detach(ctx, params.TaskID))
	case ipcMethodSend:
		var params wireSendParams
		if decodeWireParams(raw, &params) != nil {
			return nil, ErrInvalidRequest, false
		}
		return marshalActionResult(wireNoResult{}, port.Send(ctx, params.SessionID, params.Prompt))
	case ipcMethodResume:
		var params wireSessionParams
		if decodeWireParams(raw, &params) != nil {
			return nil, ErrInvalidRequest, false
		}
		return marshalActionResult(wireNoResult{}, port.Resume(ctx, params.SessionID))
	case ipcMethodStop:
		var params wireSessionParams
		if decodeWireParams(raw, &params) != nil {
			return nil, ErrInvalidRequest, false
		}
		return marshalActionResult(wireNoResult{}, port.Stop(ctx, params.SessionID))
	default:
		return nil, ErrInvalidRequest, false
	}
}

func marshalActionResult(value any, err error) (json.RawMessage, error, bool) {
	if err != nil {
		return nil, err, false
	}
	result, encodeErr := marshalWireResult(value)
	return result, encodeErr, false
}

func (server *IPCServer) lookupClient(id string) *ipcServerClient {
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.closed {
		return nil
	}
	return server.clients[id]
}

func (server *IPCServer) removeClient(id string, expected *ipcServerClient) {
	server.mu.Lock()
	if server.clients[id] == expected {
		delete(server.clients, id)
	}
	server.mu.Unlock()
	expected.close()
}

// Close stops accepting connections, closes all viewer ports, and removes
// only the exact socket and lock owned by this server.
func (server *IPCServer) Close() error {
	if server == nil {
		return nil
	}
	server.closeOnce.Do(func() {
		server.mu.Lock()
		server.closed = true
		if server.cancel != nil {
			server.cancel()
		}
		connections := make([]net.Conn, 0, len(server.conns))
		for connection := range server.conns {
			connections = append(connections, connection)
		}
		clients := make([]*ipcServerClient, 0, len(server.clients))
		for _, client := range server.clients {
			clients = append(clients, client)
		}
		clear(server.clients)
		server.mu.Unlock()
		endpointErr := server.endpoint.Close()
		for _, connection := range connections {
			_ = connection.Close()
		}
		for _, client := range clients {
			client.close()
		}
		server.wg.Wait()
		server.closeErr = endpointErr
	})
	return server.closeErr
}

// IPCClient is the viewer-side implementation of ClientPort. Its event
// connection remains open for the entire client lifetime; each bounded action
// uses a separate one-request connection.
type IPCClient struct {
	stateDir string
	id       string
	event    net.Conn

	mu            sync.Mutex
	closed        bool
	eventBroken   bool
	eventRequest  uint64
	eventMu       sync.Mutex
	actionRequest atomic.Uint64
	actionSlots   chan struct{}
	closeOnce     sync.Once
	closeErr      error
}

var _ ClientPort = (*IPCClient)(nil)

// Dial registers one viewer and establishes its single persistent event
// connection. It never starts or probes a provider.
func Dial(ctx context.Context, stateDir string) (ClientPort, error) {
	if ctx == nil || ctx.Err() != nil {
		return nil, ErrInvalidRequest
	}
	clientID, err := newIPCClientID()
	if err != nil {
		return nil, ErrCapacity
	}
	connection, err := dialLocalEndpoint(ctx, stateDir)
	if err != nil {
		return nil, err
	}
	client := &IPCClient{
		stateDir: stateDir, id: clientID, event: connection,
		actionSlots: make(chan struct{}, maxIPCClientActions),
	}
	if err := client.sendHello(ctx, connection, ipcRoleEvents); err != nil {
		_ = connection.Close()
		return nil, err
	}
	return client, nil
}

func newIPCClientID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func (client *IPCClient) Snapshot(ctx context.Context) (Snapshot, error) {
	var result wireSnapshotResult
	err := client.action(ctx, ipcMethodSnapshot, wireNoParams{}, &result)
	return result.Snapshot, err
}

func (client *IPCClient) AddTask(ctx context.Context, title string) (task.ID, error) {
	var result wireTaskIDResult
	err := client.action(ctx, ipcMethodAddTask, wireAddTaskParams{Title: title}, &result)
	return result.TaskID, err
}

func (client *IPCClient) AddManager(ctx context.Context, taskID task.ID, providerID settings.ProviderID, label string) (session.ID, error) {
	var result wireSessionIDResult
	err := client.action(ctx, ipcMethodAddManager, wireAddManagerParams{
		TaskID: taskID, ProviderID: providerID, Label: label,
	}, &result)
	return result.SessionID, err
}

func (client *IPCClient) SetEnabled(ctx context.Context, providerID settings.ProviderID, enabled bool) error {
	return client.action(ctx, ipcMethodSetEnabled, wireProviderEnabledParams{ProviderID: providerID, Enabled: enabled}, &wireNoResult{})
}

func (client *IPCClient) Authenticate(ctx context.Context, providerID settings.ProviderID) error {
	return client.action(ctx, ipcMethodAuthenticate, wireProviderParams{ProviderID: providerID}, &wireNoResult{})
}

func (client *IPCClient) Recheck(ctx context.Context, providerID settings.ProviderID) error {
	return client.action(ctx, ipcMethodRecheck, wireProviderParams{ProviderID: providerID}, &wireNoResult{})
}

func (client *IPCClient) Attach(ctx context.Context, taskID task.ID) error {
	return client.action(ctx, ipcMethodAttach, wireTaskParams{TaskID: taskID}, &wireNoResult{})
}

func (client *IPCClient) Detach(ctx context.Context, taskID task.ID) error {
	return client.action(ctx, ipcMethodDetach, wireTaskParams{TaskID: taskID}, &wireNoResult{})
}

func (client *IPCClient) Send(ctx context.Context, sessionID session.ID, prompt string) error {
	return client.action(ctx, ipcMethodSend, wireSendParams{SessionID: sessionID, Prompt: prompt}, &wireNoResult{})
}

func (client *IPCClient) Resume(ctx context.Context, sessionID session.ID) error {
	return client.action(ctx, ipcMethodResume, wireSessionParams{SessionID: sessionID}, &wireNoResult{})
}

func (client *IPCClient) Stop(ctx context.Context, sessionID session.ID) error {
	return client.action(ctx, ipcMethodStop, wireSessionParams{SessionID: sessionID}, &wireNoResult{})
}

func (client *IPCClient) Next(ctx context.Context, cursor Cursor) (Event, error) {
	if ctx == nil || ctx.Err() != nil {
		return Event{}, ErrCancelled
	}
	client.eventMu.Lock()
	defer client.eventMu.Unlock()
	client.mu.Lock()
	if client.closed || client.eventBroken || client.event == nil {
		client.mu.Unlock()
		return Event{}, ErrClosed
	}
	connection := client.event
	client.eventRequest++
	requestID := formatRequestID(client.eventRequest)
	client.mu.Unlock()

	params, err := marshalWireResult(wireNextParams{Cursor: cursor})
	if err != nil {
		return Event{}, err
	}
	request := wireRequest{Protocol: ipcProtocolVersion, ID: requestID, Method: ipcMethodNext, Params: params}
	if writeWireFrame(connection, request) != nil {
		clear(params)
		client.breakEvent()
		return Event{}, transportContextError(ctx)
	}
	clear(params)
	stop := context.AfterFunc(ctx, func() {
		client.breakEvent()
	})
	var response wireResponse
	err = readWireFrame(connection, &response)
	if !stop() && ctx.Err() != nil {
		return Event{}, ErrCancelled
	}
	if err != nil {
		client.breakEvent()
		return Event{}, transportContextError(ctx)
	}
	if validateWireResponse(response, requestID) != nil {
		clear(response.Result)
		client.breakEvent()
		return Event{}, ErrTransport
	}
	if response.Error != "" {
		clear(response.Result)
		return Event{}, codeError(response.Error)
	}
	var result wireNextResult
	if decodeWireResult(response.Result, &result) != nil {
		clear(response.Result)
		client.breakEvent()
		return Event{}, ErrTransport
	}
	clear(response.Result)
	return result.Event, nil
}

func (client *IPCClient) action(ctx context.Context, method string, params any, result any) error {
	if ctx == nil || ctx.Err() != nil {
		return ErrCancelled
	}
	select {
	case client.actionSlots <- struct{}{}:
		defer func() { <-client.actionSlots }()
	case <-ctx.Done():
		return ErrCancelled
	}
	client.mu.Lock()
	if client.closed {
		client.mu.Unlock()
		return ErrClosed
	}
	client.mu.Unlock()
	connection, err := dialLocalEndpoint(ctx, client.stateDir)
	if err != nil {
		return err
	}
	defer connection.Close()
	stop := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stop()
	if err := client.sendHello(ctx, connection, ipcRoleAction); err != nil {
		return err
	}
	requestNumber := client.actionRequest.Add(1)
	requestID := formatRequestID(requestNumber)
	raw, err := marshalWireResult(params)
	if err != nil {
		return err
	}
	request := wireRequest{Protocol: ipcProtocolVersion, ID: requestID, Method: method, Params: raw}
	if err := writeWireFrame(connection, request); err != nil {
		clear(raw)
		return transportContextError(ctx)
	}
	clear(raw)
	var response wireResponse
	if err := readWireFrame(connection, &response); err != nil {
		return transportContextError(ctx)
	}
	if validateWireResponse(response, requestID) != nil {
		clear(response.Result)
		return ErrTransport
	}
	if response.Error != "" {
		clear(response.Result)
		return codeError(response.Error)
	}
	if decodeWireResult(response.Result, result) != nil {
		clear(response.Result)
		return ErrTransport
	}
	clear(response.Result)
	return nil
}

func (client *IPCClient) sendHello(ctx context.Context, connection net.Conn, role string) error {
	stop := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stop()
	if err := writeWireFrame(connection, wireHello{Protocol: ipcProtocolVersion, Role: role, ClientID: client.id}); err != nil {
		return transportContextError(ctx)
	}
	var response wireHelloResponse
	if err := readWireFrame(connection, &response); err != nil {
		return transportContextError(ctx)
	}
	if response.Protocol != ipcProtocolVersion || !validWireCode(response.Error, true) {
		return ErrTransport
	}
	if response.Error != "" {
		return codeError(response.Error)
	}
	return nil
}

func (client *IPCClient) breakEvent() {
	client.mu.Lock()
	if !client.eventBroken {
		client.eventBroken = true
		if client.event != nil {
			_ = client.event.Close()
		}
	}
	client.mu.Unlock()
}

// Close unregisters the remote viewer, closes its persistent event
// connection, and never stops a provider runtime.
func (client *IPCClient) Close() error {
	if client == nil {
		return nil
	}
	client.closeOnce.Do(func() {
		client.mu.Lock()
		client.closed = true
		if client.event != nil {
			_ = client.event.Close()
		}
		client.mu.Unlock()
	})
	return nil
}

func transportContextError(ctx context.Context) error {
	if ctx != nil && ctx.Err() != nil {
		return ErrCancelled
	}
	return ErrTransport
}

type wireHello struct {
	Protocol int    `json:"protocol"`
	Role     string `json:"role"`
	ClientID string `json:"client_id"`
}

type wireHelloResponse struct {
	Protocol int  `json:"protocol"`
	Error    Code `json:"error,omitempty"`
}

type wireRequest struct {
	Protocol int             `json:"protocol"`
	ID       string          `json:"id"`
	Method   string          `json:"method"`
	Params   json.RawMessage `json:"params"`
}

type wireResponse struct {
	Protocol int             `json:"protocol"`
	ID       string          `json:"id"`
	Result   json.RawMessage `json:"result,omitempty"`
	Error    Code            `json:"error,omitempty"`
}

type wireNoParams struct{}
type wireNoResult struct{}
type wireSnapshotResult struct {
	Snapshot Snapshot `json:"snapshot"`
}
type wireTaskIDResult struct {
	TaskID task.ID `json:"task_id"`
}
type wireSessionIDResult struct {
	SessionID session.ID `json:"session_id"`
}
type wireAddTaskParams struct {
	Title string `json:"title"`
}
type wireAddManagerParams struct {
	TaskID     task.ID             `json:"task_id"`
	ProviderID settings.ProviderID `json:"provider_id"`
	Label      string              `json:"label"`
}
type wireProviderEnabledParams struct {
	ProviderID settings.ProviderID `json:"provider_id"`
	Enabled    bool                `json:"enabled"`
}
type wireProviderParams struct {
	ProviderID settings.ProviderID `json:"provider_id"`
}
type wireTaskParams struct {
	TaskID task.ID `json:"task_id"`
}
type wireSessionParams struct {
	SessionID session.ID `json:"session_id"`
}
type wireSendParams struct {
	SessionID session.ID `json:"session_id"`
	Prompt    string     `json:"prompt"`
}
type wireNextParams struct {
	Cursor Cursor `json:"cursor"`
}
type wireNextResult struct {
	Event Event `json:"event"`
}

func validateHello(value wireHello) error {
	if value.Protocol != ipcProtocolVersion || !validClientID(value.ClientID) ||
		(value.Role != ipcRoleEvents && value.Role != ipcRoleAction) {
		return ErrInvalidRequest
	}
	return nil
}

func validateWireRequest(value wireRequest) error {
	if value.Protocol != ipcProtocolVersion || value.Method == "" || len(value.Method) > 64 ||
		len(value.Params) == 0 || !wireObject(value.Params) {
		return ErrInvalidRequest
	}
	_, err := parseRequestID(value.ID)
	return err
}

func validateWireResponse(value wireResponse, requestID string) error {
	if value.Protocol != ipcProtocolVersion || value.ID != requestID || !validWireCode(value.Error, true) {
		return ErrTransport
	}
	if value.Error == "" {
		if len(value.Result) == 0 {
			return ErrTransport
		}
	} else if len(value.Result) != 0 {
		return ErrTransport
	}
	return nil
}

func validWireCode(code Code, allowEmpty bool) bool {
	return validRuntimeCode(code, allowEmpty)
}

func formatRequestID(value uint64) string { return fmt.Sprintf("%016x", value) }

func parseRequestID(value string) (uint64, error) {
	if len(value) != ipcRequestIDWidth || len(value) > MaxRequestIDBytes {
		return 0, ErrInvalidRequest
	}
	for _, current := range []byte(value) {
		if !((current >= '0' && current <= '9') || (current >= 'a' && current <= 'f')) {
			return 0, ErrInvalidRequest
		}
	}
	parsed, err := strconv.ParseUint(value, 16, 64)
	if err != nil || parsed == 0 {
		return 0, ErrInvalidRequest
	}
	return parsed, nil
}

func marshalWireResult(value any) (json.RawMessage, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, ErrTransport
	}
	if len(encoded) == 0 || len(encoded) > MaxWireMessageBytes || !wireObject(encoded) {
		clear(encoded)
		return nil, ErrCapacity
	}
	return encoded, nil
}

func decodeWireParams(raw json.RawMessage, target any) error {
	if len(raw) == 0 || len(raw) > MaxWireMessageBytes || !wireObject(raw) {
		return ErrInvalidRequest
	}
	return decodeWireStrict(raw, target)
}

func decodeWireResult(raw json.RawMessage, target any) error {
	if len(raw) == 0 || len(raw) > MaxWireMessageBytes || !wireObject(raw) {
		return ErrTransport
	}
	return decodeWireStrict(raw, target)
}

func wireObject(raw []byte) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) >= 2 && trimmed[0] == '{' && trimmed[len(trimmed)-1] == '}'
}

func decodeWireStrict(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return ErrInvalidRequest
	}
	return nil
}

func readWireFrame(reader io.Reader, target any) error {
	var header [4]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return ErrTransport
	}
	size := binary.BigEndian.Uint32(header[:])
	if size == 0 || uint64(size) > uint64(MaxWireMessageBytes) {
		return ErrCapacity
	}
	payload := make([]byte, int(size))
	defer clear(payload)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return ErrTransport
	}
	if err := decodeWireStrict(payload, target); err != nil {
		return ErrInvalidRequest
	}
	return nil
}

func writeWireFrame(writer io.Writer, value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return ErrTransport
	}
	defer clear(payload)
	if len(payload) == 0 || len(payload) > MaxWireMessageBytes {
		return ErrCapacity
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(payload)))
	if err := writeAll(writer, header[:]); err != nil {
		return ErrTransport
	}
	if err := writeAll(writer, payload); err != nil {
		return ErrTransport
	}
	return nil
}

func writeAll(writer io.Writer, value []byte) error {
	for len(value) != 0 {
		written, err := writer.Write(value)
		if err != nil {
			return err
		}
		if written <= 0 || written > len(value) {
			return io.ErrShortWrite
		}
		value = value[written:]
	}
	return nil
}
