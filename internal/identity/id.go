// Package identity creates human-readable, collision-resistant product IDs.
package identity

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"time"
)

const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// New returns an ID with a sortable millisecond timestamp and 50 bits of
// cryptographic randomness. Provider-native IDs are deliberately never used.
func New(prefix string) (string, error) {
	return newWith(prefix, time.Now().UTC(), rand.Reader)
}

func newWith(prefix string, now time.Time, source io.Reader) (string, error) {
	if prefix == "" {
		return "", fmt.Errorf("ID prefix is required")
	}
	var entropy [8]byte
	if _, err := io.ReadFull(source, entropy[:]); err != nil {
		return "", fmt.Errorf("read ID entropy: %w", err)
	}
	return fmt.Sprintf("%s-%s-%s", prefix, encode(uint64(now.UnixMilli()), 10), encode(binary.BigEndian.Uint64(entropy[:]), 10)), nil
}

func encode(value uint64, width int) string {
	result := make([]byte, width)
	for i := width - 1; i >= 0; i-- {
		result[i] = alphabet[value&31]
		value >>= 5
	}
	return string(result)
}
