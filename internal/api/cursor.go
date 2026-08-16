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
)

func sessionCursor(request *http.Request) (uint64, error) {
	value, err := cursorValue(request)
	if err != nil || value == "" {
		return 0, err
	}
	position, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid Session cursor %q", value)
	}
	return position, nil
}

func taskCursor(request *http.Request) (map[string]uint64, error) {
	value, err := cursorValue(request)
	if err != nil || value == "" {
		return make(map[string]uint64), err
	}
	if len(value) > maxCursorLength {
		return nil, fmt.Errorf("Task cursor is too long")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return nil, fmt.Errorf("invalid Task cursor encoding")
	}
	var result map[string]uint64
	if err := json.Unmarshal(decoded, &result); err != nil || result == nil {
		return nil, fmt.Errorf("invalid Task cursor payload")
	}
	if len(result) > maxCursorSessions {
		return nil, fmt.Errorf("Task cursor contains too many Sessions")
	}
	for sessionID := range result {
		if strings.TrimSpace(sessionID) == "" || len(sessionID) > 200 {
			return nil, fmt.Errorf("Task cursor contains an invalid Session ID")
		}
	}
	return result, nil
}

func encodeTaskCursor(cursor map[string]uint64) (string, error) {
	if len(cursor) > maxCursorSessions {
		return "", fmt.Errorf("Task cursor contains too many Sessions")
	}
	for sessionID := range cursor {
		if strings.TrimSpace(sessionID) == "" || len(sessionID) > 200 {
			return "", fmt.Errorf("Task cursor contains an invalid Session ID")
		}
	}
	encoded, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	result := base64.RawURLEncoding.EncodeToString(encoded)
	if len(result) > maxCursorLength {
		return "", fmt.Errorf("Task cursor is too long")
	}
	return result, nil
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
