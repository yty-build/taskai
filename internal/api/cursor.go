package api

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

const (
	maxCursorLength   = 8192
	maxCursorSessions = 512
	exactZeroCursor   = "exact:0"
)

type sessionStreamCursor struct {
	Position uint64
	Fresh    bool
}

type taskCursorEnvelope struct {
	Version            int               `json:"v"`
	MembershipBoundary bool              `json:"membership_boundary"`
	Sessions           map[string]uint64 `json:"sessions"`
}

func sessionCursor(request *http.Request) (sessionStreamCursor, error) {
	value, err := cursorValue(request)
	if err != nil {
		return sessionStreamCursor{}, err
	}
	if value == "" {
		return sessionStreamCursor{Fresh: true}, nil
	}
	// A fresh viewer and an exact viewer positioned before sequence one both
	// have numeric position zero. The ready event uses this transport-only token
	// so EventSource can reconnect after ready-but-before-first-frame without
	// silently selecting a newer bounded tail. Caller-supplied numeric zero keeps
	// its documented fresh-view meaning.
	if value == exactZeroCursor {
		return sessionStreamCursor{}, nil
	}
	position, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return sessionStreamCursor{}, fmt.Errorf("invalid Session cursor %q", value)
	}
	return sessionStreamCursor{Position: position, Fresh: position == 0}, nil
}

func sessionCursorID(position uint64) string {
	if position == 0 {
		return exactZeroCursor
	}
	return strconv.FormatUint(position, 10)
}

func taskCursor(request *http.Request) (map[string]uint64, error) {
	cursor, _, err := taskCursorWithMembership(request)
	return cursor, err
}

func taskCursorWithMembership(request *http.Request) (map[string]uint64, bool, error) {
	value, err := cursorValue(request)
	if err != nil || value == "" {
		return make(map[string]uint64), false, err
	}
	if len(value) > maxCursorLength {
		return nil, false, fmt.Errorf("Task cursor is too long")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return nil, false, fmt.Errorf("invalid Task cursor encoding")
	}
	// Raw position maps were emitted before aggregate cursors carried an
	// explicit Task-membership boundary. Continue accepting them, but do not
	// infer exact-zero semantics for Sessions absent from an ambiguous legacy
	// map.
	var legacy map[string]uint64
	if err := json.Unmarshal(decoded, &legacy); err == nil && legacy != nil {
		if err := validateTaskCursorPositions(legacy); err != nil {
			return nil, false, err
		}
		return legacy, false, nil
	}
	var envelope taskCursorEnvelope
	if err := json.Unmarshal(decoded, &envelope); err != nil || envelope.Version != 1 ||
		!envelope.MembershipBoundary || envelope.Sessions == nil {
		return nil, false, fmt.Errorf("invalid Task cursor payload")
	}
	if err := validateTaskCursorPositions(envelope.Sessions); err != nil {
		return nil, false, err
	}
	return envelope.Sessions, true, nil
}

func encodeTaskCursor(cursor map[string]uint64) (string, error) {
	if cursor == nil {
		cursor = make(map[string]uint64)
	}
	if err := validateTaskCursorPositions(cursor); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(taskCursorEnvelope{
		Version: 1, MembershipBoundary: true, Sessions: cursor,
	})
	if err != nil {
		return "", err
	}
	result := base64.RawURLEncoding.EncodeToString(encoded)
	if len(result) > maxCursorLength {
		return "", fmt.Errorf("Task cursor is too long")
	}
	return result, nil
}

func validateTaskCursorPositions(cursor map[string]uint64) error {
	if len(cursor) > maxCursorSessions {
		return fmt.Errorf("Task cursor contains too many Sessions")
	}
	for sessionID := range cursor {
		if strings.TrimSpace(sessionID) == "" || len(sessionID) > 200 {
			return fmt.Errorf("Task cursor contains an invalid Session ID")
		}
	}
	return nil
}

func cursorValue(request *http.Request) (string, error) {
	query := request.URL.Query().Get("cursor")
	seed := request.URL.Query().Get("seed")
	header := request.Header.Get("Last-Event-ID")
	if query != "" && seed != "" {
		return "", fmt.Errorf("cursor and seed cannot both be supplied")
	}
	if query != "" && header != "" && query != header {
		return "", fmt.Errorf("cursor and Last-Event-ID disagree")
	}
	if header != "" {
		return header, nil
	}
	if query != "" {
		return query, nil
	}
	return seed, nil
}
