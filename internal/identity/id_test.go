package identity

import (
	"bytes"
	"testing"
	"time"
)

func TestNewWithIsReadableAndStable(t *testing.T) {
	now := time.Date(2026, 8, 14, 12, 0, 0, 123_000_000, time.FixedZone("local", -7*60*60))
	id, err := newWith("T", now, bytes.NewReader([]byte{1, 2, 3, 4, 5, 6, 7, 8}))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := id, "T-01M00TADZV-G30G2GC1R8"; got != want {
		t.Fatalf("ID = %q, want %q", got, want)
	}
}

func TestNewWithRequiresPrefix(t *testing.T) {
	if _, err := newWith("", time.Now(), bytes.NewReader(make([]byte, 8))); err == nil {
		t.Fatal("newWith() error = nil")
	}
}
