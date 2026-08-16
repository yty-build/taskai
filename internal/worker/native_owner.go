package worker

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const (
	nativeMarkerVersion        = 1
	nativeMarkerOwned          = "owned"
	nativeMarkerRefreshPending = "refresh_pending"
)

type nativeOwnerMarker struct {
	Version    int    `json:"version"`
	SessionID  string `json:"session_id"`
	State      string `json:"state"`
	OwnerToken string `json:"owner_token"`
}

// restoreNativeOwnership deliberately fails closed. A malformed, insecure,
// or unknown marker is never interpreted as an expired lease because the
// native provider may still be alive.
func (w *Worker) restoreNativeOwnership() error {
	path := w.NativeOwnerPath()
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect native provider ownership marker: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return fmt.Errorf("native provider ownership marker %q is not a private 0600 regular file; refusing ACP work until it is recovered explicitly", path)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read native provider ownership marker: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var marker nativeOwnerMarker
	if err := decoder.Decode(&marker); err != nil {
		return fmt.Errorf("decode native provider ownership marker %q: %w; refusing ACP work until it is recovered explicitly", path, err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return fmt.Errorf("decode native provider ownership marker %q: %w; refusing ACP work until it is recovered explicitly", path, err)
	}
	if marker.Version != nativeMarkerVersion || marker.SessionID != w.config.Session.ID {
		return fmt.Errorf("native provider ownership marker %q does not match Session %s or a supported version; refusing ACP work until it is recovered explicitly", path, w.config.Session.ID)
	}
	ownerToken, err := validateNativeOwnerToken(marker.OwnerToken)
	if err != nil {
		return fmt.Errorf("native provider ownership marker %q has an invalid owner token; refusing ACP work until it is recovered explicitly", path)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	switch marker.State {
	case nativeMarkerOwned:
		w.nativeOwnerToken = ownerToken
	case nativeMarkerRefreshPending:
		w.nativeRefreshBarrier = true
		w.refreshPending = true
		w.refreshQueueRemaining = 0
	default:
		return fmt.Errorf("native provider ownership marker %q has unknown state %q; refusing ACP work until it is recovered explicitly", path, marker.State)
	}
	return nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return err
	}
	return nil
}

func (w *Worker) persistNativeOwnership(state, ownerToken string) error {
	marker := nativeOwnerMarker{
		Version: nativeMarkerVersion, SessionID: w.config.Session.ID,
		State: state, OwnerToken: ownerToken,
	}
	body, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		return err
	}
	directory := filepath.Dir(w.NativeOwnerPath())
	temporary, err := os.CreateTemp(directory, "."+w.config.Session.ID+".native-owner-*")
	if err != nil {
		return fmt.Errorf("create native provider ownership marker: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("secure native provider ownership marker: %w", err)
	}
	if _, err := temporary.Write(append(body, '\n')); err != nil {
		temporary.Close()
		return fmt.Errorf("write native provider ownership marker: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync native provider ownership marker: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close native provider ownership marker: %w", err)
	}
	if err := os.Rename(temporaryPath, w.NativeOwnerPath()); err != nil {
		return fmt.Errorf("publish native provider ownership marker: %w", err)
	}
	return syncDirectory(directory)
}

func (w *Worker) removeNativeOwnership() error {
	if err := os.Remove(w.NativeOwnerPath()); err != nil {
		return fmt.Errorf("remove native provider ownership marker: %w", err)
	}
	return syncDirectory(filepath.Dir(w.NativeOwnerPath()))
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open worker directory for sync: %w", err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync worker directory: %w", err)
	}
	return nil
}
