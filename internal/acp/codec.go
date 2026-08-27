package acp

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"sync"
)

// Decoder reads newline-delimited ACP JSON-RPC with a hard frame limit.
type Decoder struct {
	r *bufio.Reader
}

func NewDecoder(r io.Reader) *Decoder {
	return &Decoder{r: bufio.NewReaderSize(r, MaxFrameBytes+2)}
}

// Decode returns exactly one wire message. An EOF-terminated final JSON value
// is accepted; an empty terminal read returns io.EOF.
func (d *Decoder) Decode() (Message, error) {
	line, err := d.r.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		return Message{}, ErrFrameTooLarge
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return Message{}, ErrTransportClosed
	}
	if errors.Is(err, io.EOF) && len(line) == 0 {
		return Message{}, io.EOF
	}
	if len(line) != 0 && line[len(line)-1] == '\n' {
		line = line[:len(line)-1]
	}
	if len(line) != 0 && line[len(line)-1] == '\r' {
		line = line[:len(line)-1]
	}
	if len(line) > MaxFrameBytes {
		return Message{}, ErrFrameTooLarge
	}
	return ParseMessage(line)
}

// Encoder writes one bounded message per line. Writes are serialized so a
// peer can answer concurrent JSON-RPC requests without interleaving bytes.
type Encoder struct {
	w  io.Writer
	mu sync.Mutex
}

func NewEncoder(w io.Writer) *Encoder { return &Encoder{w: w} }

func (e *Encoder) Encode(m Message) error {
	b, err := m.Bytes()
	if err != nil {
		return err
	}
	if bytes.IndexByte(b, '\n') >= 0 || bytes.IndexByte(b, '\r') >= 0 {
		return ErrMalformedFrame
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	framed := append(b, '\n')
	for len(framed) != 0 {
		written, err := e.w.Write(framed)
		if written < 0 || written > len(framed) {
			return ErrTransportClosed
		}
		framed = framed[written:]
		if err != nil || written == 0 {
			return ErrTransportClosed
		}
	}
	return nil
}
