package worker

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const nativeGenerationBytes = 16

const (
	nativeMarkerVersion        = 1
	nativeMarkerReserved       = "reserved"
	nativeMarkerOwned          = "owned"
	nativeMarkerRefreshPending = "refresh_pending"
)

type nativeOwnerMarker struct {
	Version       int    `json:"version"`
	SessionID     string `json:"session_id"`
	State         string `json:"state"`
	OwnerToken    string `json:"owner_token"`
	Generation    string `json:"generation,omitempty"`
	StaleAfterACP *bool  `json:"stale_after_acp,omitempty"`
}

// restoreNativeOwnership fails closed for both ACP and native work. A malformed,
// insecure, or unknown marker may still represent a live native provider and
// is never interpreted as an expired lease.
func (w *Worker) restoreNativeOwnership() error {
	path := w.NativeOwnerPath()
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return w.blockNativeOwnership("could not be inspected")
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return w.blockNativeOwnership("is not a private 0600 regular file")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return w.blockNativeOwnership("could not be read")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var marker nativeOwnerMarker
	if err := decoder.Decode(&marker); err != nil {
		return w.blockNativeOwnership("is malformed")
	}
	if err := requireJSONEOF(decoder); err != nil {
		return w.blockNativeOwnership("is malformed")
	}
	if marker.Version != nativeMarkerVersion || marker.SessionID != w.config.Session.ID {
		return w.blockNativeOwnership("does not match this Session or protocol version")
	}
	ownerToken, err := validateNativeOwnerToken(marker.OwnerToken)
	if err != nil {
		return w.blockNativeOwnership("has an invalid owner token")
	}
	if marker.State == nativeMarkerReserved || marker.State == nativeMarkerOwned {
		if err := validateNativeGeneration(marker.Generation); err != nil {
			return w.blockNativeOwnership("has an invalid ownership generation")
		}
	}
	if marker.State != nativeMarkerReserved && marker.State != nativeMarkerOwned && marker.State != nativeMarkerRefreshPending {
		return w.blockNativeOwnership("has an unknown ownership phase")
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	switch marker.State {
	case nativeMarkerReserved:
		w.nativeOwnerToken = ownerToken
		w.nativeOwnerGeneration = marker.Generation
		w.nativeOwnerReserved = true
		w.nativeOwnerStale = false
	case nativeMarkerOwned:
		w.nativeOwnerToken = ownerToken
		w.nativeOwnerGeneration = marker.Generation
		w.nativeOwnerReserved = false
		// Committed v2 markers predate the optional stale bit and already used
		// exclusive ownership, so only an explicit WIP stale marker is exit-only.
		w.nativeOwnerStale = marker.StaleAfterACP != nil && *marker.StaleAfterACP
	case nativeMarkerRefreshPending:
		// A released native generation may have advanced the provider store.
		// Preserve this durable phase until an exact ACP rehydration succeeds.
		w.nativeReconcilePending = true
		w.requireACPFreshnessLocked()
	}
	return nil
}

func (w *Worker) blockNativeOwnership(reason string) error {
	w.mu.Lock()
	w.nativeOwnershipBlocked = true
	w.nativeOwnershipReason = reason
	w.mu.Unlock()
	return fmt.Errorf("native provider ownership marker %q %s; refusing ACP and native work until it is recovered explicitly", w.NativeOwnerPath(), reason)
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

func newNativeGeneration() (string, error) {
	value := make([]byte, nativeGenerationBytes)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate native ownership generation: %w", err)
	}
	return hex.EncodeToString(value), nil
}

func validateNativeGeneration(value string) error {
	if len(value) != nativeGenerationBytes*2 {
		return fmt.Errorf("native generation must be %d hexadecimal characters", nativeGenerationBytes*2)
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != nativeGenerationBytes {
		return fmt.Errorf("native generation is invalid")
	}
	return nil
}

func (w *Worker) persistNativeOwnership(state, ownerToken, generation string) error {
	marker := nativeOwnerMarker{
		Version: nativeMarkerVersion, SessionID: w.config.Session.ID,
		State: state, OwnerToken: ownerToken, Generation: generation,
	}
	if state == nativeMarkerReserved || state == nativeMarkerOwned {
		stale := state == nativeMarkerOwned && w.nativeOwnerStale
		marker.StaleAfterACP = &stale
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

func (w *Worker) loadNativeRefreshPendingMarker() (nativeOwnerMarker, error) {
	info, err := os.Lstat(w.NativeOwnerPath())
	if err != nil {
		return nativeOwnerMarker{}, fmt.Errorf("inspect ACP rehydration marker: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return nativeOwnerMarker{}, fmt.Errorf("ACP rehydration marker is not a private 0600 regular file")
	}
	body, err := os.ReadFile(w.NativeOwnerPath())
	if err != nil {
		return nativeOwnerMarker{}, fmt.Errorf("read ACP rehydration marker: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var marker nativeOwnerMarker
	if err := decoder.Decode(&marker); err != nil {
		return nativeOwnerMarker{}, fmt.Errorf("decode ACP rehydration marker: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return nativeOwnerMarker{}, fmt.Errorf("decode ACP rehydration marker: %w", err)
	}
	if marker.Version != nativeMarkerVersion || marker.SessionID != w.config.Session.ID {
		return nativeOwnerMarker{}, fmt.Errorf("ACP rehydration marker does not match this Session or protocol version")
	}
	if _, err := validateNativeOwnerToken(marker.OwnerToken); err != nil {
		return nativeOwnerMarker{}, fmt.Errorf("ACP rehydration marker has an invalid owner token")
	}
	if err := validateNativeGeneration(marker.Generation); err != nil {
		return nativeOwnerMarker{}, fmt.Errorf("ACP rehydration marker has an invalid generation")
	}
	if marker.State != nativeMarkerRefreshPending {
		return nativeOwnerMarker{}, fmt.Errorf("ACP rehydration marker unexpectedly changed to native state %q; preserving it and refusing stale dispatch", marker.State)
	}
	return marker, nil
}

// clearNativeRefreshPendingLocked removes only the non-owning freshness phase.
// Callers hold w.mu, so a concurrently acquired native generation either wins
// first and clears the in-memory phase or is serialized after this removal.
func (w *Worker) clearNativeRefreshPendingLocked() error {
	if !w.nativeReconcilePending {
		return nil
	}
	info, err := os.Lstat(w.NativeOwnerPath())
	if os.IsNotExist(err) {
		w.nativeReconcilePending = false
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect ACP rehydration marker: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return fmt.Errorf("ACP rehydration marker is not a private 0600 regular file")
	}
	body, err := os.ReadFile(w.NativeOwnerPath())
	if os.IsNotExist(err) {
		w.nativeReconcilePending = false
		return nil
	}
	if err != nil {
		return fmt.Errorf("read ACP rehydration marker: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var marker nativeOwnerMarker
	if err := decoder.Decode(&marker); err != nil {
		return fmt.Errorf("decode ACP rehydration marker: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return fmt.Errorf("decode ACP rehydration marker: %w", err)
	}
	if marker.Version != nativeMarkerVersion || marker.SessionID != w.config.Session.ID {
		return fmt.Errorf("ACP rehydration marker does not match this Session or protocol version")
	}
	if _, err := validateNativeOwnerToken(marker.OwnerToken); err != nil {
		return fmt.Errorf("ACP rehydration marker has an invalid owner token")
	}
	if marker.Generation != "" {
		if err := validateNativeGeneration(marker.Generation); err != nil {
			return fmt.Errorf("ACP rehydration marker has an invalid generation")
		}
	}
	if marker.State != nativeMarkerRefreshPending {
		return fmt.Errorf("ACP rehydration marker unexpectedly changed to native state %q; preserving it and refusing stale dispatch", marker.State)
	}
	if err := w.removeNativeOwnership(); err != nil {
		return err
	}
	w.nativeReconcilePending = false
	return nil
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
