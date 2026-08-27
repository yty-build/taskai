package acp

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestUnknownNotificationRoundTripsByteForByte(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"jsonrpc":"2.0", "method":"session/update", "params":{"future":{"z":[3,2,1]},"kind":"not-normalized"}}`)
	decoder := NewDecoder(bytes.NewReader(append(append([]byte(nil), raw...), '\n')))
	message, err := decoder.Decode()
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if !bytes.Equal(message.Raw, raw) {
		t.Fatalf("raw bytes changed")
	}

	var encoded bytes.Buffer
	if err := NewEncoder(&encoded).Encode(message); err != nil {
		t.Fatalf("Encode() error = %v", err)
	}
	want := append(append([]byte(nil), raw...), '\n')
	if !bytes.Equal(encoded.Bytes(), want) {
		t.Fatalf("round trip changed bytes\ngot:  %q\nwant: %q", encoded.Bytes(), want)
	}
}

func TestFrameBoundsAreStrict(t *testing.T) {
	t.Parallel()
	prefix := `{"jsonrpc":"2.0","method":"session/update","params":{"opaque":"`
	suffix := `"}}`
	exact := []byte(prefix + strings.Repeat("x", MaxFrameBytes-len(prefix)-len(suffix)) + suffix)
	if len(exact) != MaxFrameBytes {
		t.Fatalf("fixture size = %d", len(exact))
	}
	if _, err := ParseMessage(exact); err != nil {
		t.Fatalf("exact-limit ParseMessage() error = %v", err)
	}
	if _, err := NewDecoder(bytes.NewReader(append(exact, '\n'))).Decode(); err != nil {
		t.Fatalf("exact-limit Decode() error = %v", err)
	}

	oversized := append(append([]byte(nil), exact...), ' ')
	if _, err := ParseMessage(oversized); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("oversized ParseMessage() error = %v, want ErrFrameTooLarge", err)
	}
	if _, err := NewDecoder(bytes.NewReader(append(oversized, '\n'))).Decode(); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("oversized Decode() error = %v, want ErrFrameTooLarge", err)
	}
}

func TestMalformedTrafficFailsWithoutEchoingContent(t *testing.T) {
	t.Parallel()
	canary := "PRIVATE-CONTENT-CANARY"
	cases := [][]byte{
		[]byte(`not-json-` + canary),
		[]byte(`[{"jsonrpc":"2.0","method":"session/update"}]`),
		[]byte(`{"jsonrpc":"1.0","method":"session/update","params":{"x":"` + canary + `"}}`),
		[]byte(`{"jsonrpc":"2.0","id":{},"method":"session/prompt"}`),
		[]byte(`{"jsonrpc":"2.0","id":1,"result":{},"error":{"code":1,"message":"x"}}`),
	}
	for _, raw := range cases {
		_, err := ParseMessage(raw)
		if !errors.Is(err, ErrMalformedFrame) {
			t.Errorf("ParseMessage(%q) error = %v, want ErrMalformedFrame", raw, err)
		}
		if err != nil && strings.Contains(err.Error(), canary) {
			t.Errorf("error leaked content canary: %v", err)
		}
	}
}

func TestOpaqueParamsRemainRawMessage(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"jsonrpc":"2.0","method":"session/update","params": { "unknown" : [true, null] }}`)
	message, err := ParseMessage(raw)
	if err != nil {
		t.Fatal(err)
	}
	want := json.RawMessage(`{ "unknown" : [true, null] }`)
	if !bytes.Equal(message.Params, want) {
		t.Fatalf("params = %q, want %q", message.Params, want)
	}
}

func TestEncoderLoopsUntilShortWritesComplete(t *testing.T) {
	t.Parallel()
	writer := &shortWriter{maximum: 3}
	message := Notification("session/update", json.RawMessage(`{"opaque":"value"}`))
	want, err := message.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, '\n')
	if err := NewEncoder(writer).Encode(message); err != nil {
		t.Fatalf("Encode() error = %v", err)
	}
	if !bytes.Equal(writer.Bytes(), want) {
		t.Fatalf("short-write result = %q, want %q", writer.Bytes(), want)
	}
	if writer.calls < 2 {
		t.Fatalf("writer calls = %d, want multiple short writes", writer.calls)
	}
}

func TestEncoderRejectsZeroLengthWrite(t *testing.T) {
	t.Parallel()
	writer := &shortWriter{zero: true}
	err := NewEncoder(writer).Encode(Notification("session/update", json.RawMessage(`{}`)))
	if !errors.Is(err, ErrTransportClosed) {
		t.Fatalf("Encode() error = %v, want ErrTransportClosed", err)
	}
}

type shortWriter struct {
	bytes.Buffer
	maximum int
	zero    bool
	calls   int
}

func (w *shortWriter) Write(value []byte) (int, error) {
	w.calls++
	if w.zero {
		return 0, nil
	}
	maximum := w.maximum
	if maximum <= 0 || maximum > len(value) {
		maximum = len(value)
	}
	return w.Buffer.Write(value[:maximum])
}
