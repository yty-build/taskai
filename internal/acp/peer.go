package acp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"sync"
	"sync/atomic"
)

const (
	MethodInitialize    = "initialize"
	MethodAuthenticate  = "authenticate"
	MethodSessionNew    = "session/new"
	MethodSessionResume = "session/resume"
	MethodSessionPrompt = "session/prompt"
	MethodSessionCancel = "session/cancel"
	MethodSessionUpdate = "session/update"
	MethodCancelRequest = "$/cancelRequest"
	MaxInboundRequests  = 64
)

// Handler is the complete public inbound JSON-RPC surface. Implementations may
// inspect ACP envelope mechanics, but opaque content remains in Message.Raw,
// Params, and Result.
type Handler interface {
	HandleRequest(context.Context, Message) (json.RawMessage, *ErrorObject)
	HandleNotification(context.Context, Message) error
}

type HandlerFuncs struct {
	Request      func(context.Context, Message) (json.RawMessage, *ErrorObject)
	Notification func(context.Context, Message) error
}

func (h HandlerFuncs) HandleRequest(ctx context.Context, m Message) (json.RawMessage, *ErrorObject) {
	if h.Request == nil {
		return nil, &ErrorObject{Code: -32601, Message: "method not supported"}
	}
	return h.Request(ctx, m)
}

func (h HandlerFuncs) HandleNotification(ctx context.Context, m Message) error {
	if h.Notification == nil {
		return nil
	}
	return h.Notification(ctx, m)
}

type callResult struct {
	message Message
	err     error
}

// Peer is a symmetric, minimal JSON-RPC endpoint. Client and Agent below give
// role-specific names without adding a second protocol implementation.
type Peer struct {
	decoder *Decoder
	encoder *Encoder
	handler Handler

	seq atomic.Uint64

	mu       sync.Mutex
	pending  map[string]chan callResult
	inbound  map[string]context.CancelFunc
	started  bool
	terminal error

	done     chan struct{}
	doneOnce sync.Once
	closeIO  func()
}

func NewPeer(r io.Reader, w io.Writer, handler Handler) *Peer {
	p := &Peer{
		decoder: NewDecoder(r),
		encoder: NewEncoder(w),
		handler: handler,
		pending: make(map[string]chan callResult),
		inbound: make(map[string]context.CancelFunc),
		done:    make(chan struct{}),
	}
	p.closeIO = func() {
		if closer, ok := r.(io.Closer); ok {
			_ = closer.Close()
		}
		if closer, ok := w.(io.Closer); ok {
			_ = closer.Close()
		}
	}
	return p
}

// Start begins the single sequential raw reader. Notifications are handled on
// that reader goroutine, so a slow handler applies transport backpressure and
// cannot be overtaken by a later notification.
func (p *Peer) Start(ctx context.Context) error {
	p.mu.Lock()
	if p.started {
		p.mu.Unlock()
		return nil
	}
	p.started = true
	p.mu.Unlock()

	go p.readLoop(ctx)
	go func() {
		select {
		case <-ctx.Done():
			p.fail(ctx.Err())
			p.closeIO()
		case <-p.done:
		}
	}()
	return nil
}

// Call sends one request and waits for its exact correlated response.
func (p *Peer) Call(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	if method == "" || (len(params) != 0 && !json.Valid(params)) {
		return nil, ErrMalformedFrame
	}
	id := p.seq.Add(1)
	idRaw := json.RawMessage(strconv.FormatUint(id, 10))
	key, _ := correlationKey(idRaw)
	result := make(chan callResult, 1)

	p.mu.Lock()
	if !p.started {
		p.mu.Unlock()
		return nil, ErrEndpointNotReady
	}
	if p.terminal != nil {
		err := p.terminal
		p.mu.Unlock()
		return nil, err
	}
	p.pending[key] = result
	p.mu.Unlock()

	if err := p.encoder.Encode(Request(idRaw, method, params)); err != nil {
		p.removePending(key)
		p.fail(err)
		return nil, err
	}

	select {
	case reply := <-result:
		if reply.err != nil {
			return nil, reply.err
		}
		if reply.message.Error != nil {
			return nil, &RemoteError{Code: reply.message.Error.Code}
		}
		return cloneRaw(reply.message.Result), nil
	case <-ctx.Done():
		p.removePending(key)
		return nil, ctx.Err()
	case <-p.done:
		p.removePending(key)
		return nil, p.Err()
	}
}

// Notify sends one notification with opaque params.
func (p *Peer) Notify(method string, params json.RawMessage) error {
	if method == "" || (len(params) != 0 && !json.Valid(params)) {
		return ErrMalformedFrame
	}
	p.mu.Lock()
	ready := p.started && p.terminal == nil
	p.mu.Unlock()
	if !ready {
		return ErrEndpointNotReady
	}
	if err := p.encoder.Encode(Notification(method, params)); err != nil {
		p.fail(err)
		return err
	}
	return nil
}

// Wait blocks until the endpoint terminates.
func (p *Peer) Wait() error {
	<-p.done
	return p.Err()
}

// Done closes when the endpoint has terminated. It lets transport owners
// release and reap the process behind a Peer without starting a second waiter.
func (p *Peer) Done() <-chan struct{} {
	if p == nil {
		done := make(chan struct{})
		close(done)
		return done
	}
	return p.done
}

// Close ends this endpoint and unblocks its transport.
func (p *Peer) Close() error {
	p.fail(ErrTransportClosed)
	p.closeIO()
	return nil
}

func (p *Peer) Err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.terminal == nil {
		return nil
	}
	return p.terminal
}

func (p *Peer) readLoop(ctx context.Context) {
	for {
		message, err := p.decoder.Decode()
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = ErrTransportClosed
			}
			p.fail(err)
			return
		}
		switch message.Kind() {
		case KindResponse:
			if err := p.deliverResponse(message); err != nil {
				p.fail(err)
				return
			}
		case KindNotification:
			if message.Method == MethodCancelRequest {
				p.cancelInbound(message.Params)
			}
			if p.handler != nil {
				if err := p.handler.HandleNotification(ctx, message); err != nil {
					p.fail(&HandlerError{})
					return
				}
			}
		case KindRequest:
			if err := p.startInbound(ctx, message); err != nil {
				p.fail(err)
				return
			}
		default:
			p.fail(ErrMalformedFrame)
			return
		}
	}
}

func (p *Peer) startInbound(parent context.Context, message Message) error {
	key, err := correlationKey(message.ID)
	if err != nil {
		return ErrMalformedFrame
	}
	ctx, cancel := context.WithCancel(parent)
	p.mu.Lock()
	if p.terminal != nil {
		p.mu.Unlock()
		cancel()
		return ErrTransportClosed
	}
	if _, duplicate := p.inbound[key]; duplicate || len(p.inbound) >= MaxInboundRequests {
		p.mu.Unlock()
		cancel()
		return ErrMalformedFrame
	}
	p.inbound[key] = cancel
	p.mu.Unlock()

	go func() {
		defer func() {
			cancel()
			p.mu.Lock()
			delete(p.inbound, key)
			p.mu.Unlock()
		}()
		if p.handler == nil {
			if err := p.encoder.Encode(ErrorResponse(message.ID, &ErrorObject{Code: -32601, Message: "method not supported"})); err != nil {
				p.fail(err)
			}
			return
		}
		result, rpcErr := p.handler.HandleRequest(ctx, message)
		if rpcErr != nil {
			if err := p.encoder.Encode(ErrorResponse(message.ID, rpcErr)); err != nil {
				p.fail(err)
			}
			return
		}
		if len(result) == 0 {
			result = json.RawMessage("null")
		}
		if err := p.encoder.Encode(Response(message.ID, result)); err != nil {
			p.fail(err)
		}
	}()
	return nil
}

func (p *Peer) cancelInbound(params json.RawMessage) {
	var payload struct {
		ID json.RawMessage `json:"id"`
	}
	if json.Unmarshal(params, &payload) != nil {
		return
	}
	key, err := correlationKey(payload.ID)
	if err != nil {
		return
	}
	p.mu.Lock()
	cancel := p.inbound[key]
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (p *Peer) deliverResponse(message Message) error {
	key, _ := correlationKey(message.ID)
	p.mu.Lock()
	ch, ok := p.pending[key]
	if ok {
		delete(p.pending, key)
	}
	p.mu.Unlock()
	if !ok {
		return ErrUnexpectedReply
	}
	ch <- callResult{message: message}
	return nil
}

func (p *Peer) removePending(key string) {
	p.mu.Lock()
	delete(p.pending, key)
	p.mu.Unlock()
}

func (p *Peer) fail(err error) {
	if err == nil {
		err = ErrTransportClosed
	}
	p.doneOnce.Do(func() {
		p.mu.Lock()
		p.terminal = err
		pending := p.pending
		p.pending = make(map[string]chan callResult)
		for _, cancel := range p.inbound {
			cancel()
		}
		p.inbound = make(map[string]context.CancelFunc)
		p.mu.Unlock()
		for _, ch := range pending {
			ch <- callResult{err: err}
		}
		close(p.done)
	})
}

// RemoteError carries only the public JSON-RPC code and message. Error.Data is
// intentionally not placed in diagnostics.
type RemoteError struct {
	Code int
}

func (e *RemoteError) Error() string {
	if e == nil {
		return "acp: remote error"
	}
	return "acp: remote error " + strconv.Itoa(e.Code)
}

// HandlerError intentionally omits opaque request and response content.
type HandlerError struct{}

func (*HandlerError) Error() string { return "acp: inbound handler failed" }

// Client names the ACP-client role of a symmetric endpoint.
type Client struct{ *Peer }

func NewClient(r io.Reader, w io.Writer, handler Handler) *Client {
	return &Client{Peer: NewPeer(r, w, handler)}
}

// Agent names the ACP-agent role of a symmetric endpoint.
type Agent struct{ *Peer }

func NewAgent(r io.Reader, w io.Writer, handler Handler) *Agent {
	return &Agent{Peer: NewPeer(r, w, handler)}
}
