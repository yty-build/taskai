package replay

import (
	"encoding/json"
	"io"
	"net/url"
	"path/filepath"
	"strings"
)

const (
	referenceVersion  = 1
	maxReferenceBytes = 4096
	maxComponentBytes = 255
)

type nativeReference struct {
	Version   int        `json:"v"`
	Provider  ProviderID `json:"provider"`
	SessionID string     `json:"session_id"`
	Scope     string     `json:"scope,omitempty"`
	RecordKey string     `json:"record_key,omitempty"`
}

// NewCodexReference creates the connector-opaque reference used by both
// Codex Resume and Replay.
func NewCodexReference(threadID string) (OpaqueRef, error) {
	return encodeReference(nativeReference{
		Version: referenceVersion, Provider: ProviderCodex, SessionID: threadID,
	})
}

// NewClaudeReference creates an exact project-scoped Claude Session reference.
func NewClaudeReference(projectKey, sessionID string) (OpaqueRef, error) {
	return encodeReference(nativeReference{
		Version: referenceVersion, Provider: ProviderClaude,
		SessionID: sessionID, Scope: projectKey,
	})
}

// NewGeminiReference creates an exact project and record-key scoped Gemini
// Session reference. recordKey is a basename, never a native path.
func NewGeminiReference(projectHash, recordKey, sessionID string) (OpaqueRef, error) {
	return encodeReference(nativeReference{
		Version: referenceVersion, Provider: ProviderGemini,
		SessionID: sessionID, Scope: projectHash, RecordKey: recordKey,
	})
}

// NewGrokReference creates an exact workspace-scoped Grok Session reference.
func NewGrokReference(workspaceKey, sessionID string) (OpaqueRef, error) {
	return encodeReference(nativeReference{
		Version: referenceVersion, Provider: ProviderGrok,
		SessionID: sessionID, Scope: workspaceKey,
	})
}

// ResolveCodexReference is the connector-only Resume counterpart to
// NewCodexReference. It returns the exact app-server thread ID and performs no
// lookup, enumeration, fallback, or path conversion.
func ResolveCodexReference(reference OpaqueRef) (threadID string, err error) {
	profile, _ := ConformedProfile(ProviderCodex, "0.150.1")
	native, decodeErr := decodeReference(reference, ProviderCodex, profile)
	if decodeErr != nil {
		return "", decodeErr
	}
	return native.SessionID, nil
}

// ResolveClaudeReference is the connector-only Resume counterpart to
// NewClaudeReference.
func ResolveClaudeReference(reference OpaqueRef) (projectKey, sessionID string, err error) {
	profile, _ := ConformedProfile(ProviderClaude, "2.1.226")
	native, decodeErr := decodeReference(reference, ProviderClaude, profile)
	if decodeErr != nil {
		return "", "", decodeErr
	}
	return native.Scope, native.SessionID, nil
}

// ResolveGeminiReference is the connector-only Resume counterpart to
// NewGeminiReference.
func ResolveGeminiReference(reference OpaqueRef) (projectHash, recordKey, sessionID string, err error) {
	profile, _ := ConformedProfile(ProviderGemini, "0.25.0-preview.0")
	native, decodeErr := decodeReference(reference, ProviderGemini, profile)
	if decodeErr != nil {
		return "", "", "", decodeErr
	}
	return native.Scope, native.RecordKey, native.SessionID, nil
}

// ResolveGrokReference is the connector-only Resume counterpart to
// NewGrokReference.
func ResolveGrokReference(reference OpaqueRef) (workspaceKey, sessionID string, err error) {
	profile, _ := ConformedProfile(ProviderGrok, "1.0.5")
	native, decodeErr := decodeReference(reference, ProviderGrok, profile)
	if decodeErr != nil {
		return "", "", decodeErr
	}
	return native.Scope, native.SessionID, nil
}

func encodeReference(reference nativeReference) (OpaqueRef, error) {
	if err := validateReference(reference); err != nil {
		return OpaqueRef{}, ErrInvalidReference
	}
	raw, err := json.Marshal(reference)
	if err != nil || len(raw) > maxReferenceBytes {
		return OpaqueRef{}, ErrInvalidReference
	}
	return OpaqueRefFromBytes(raw), nil
}

func decodeReference(raw OpaqueRef, provider ProviderID, profile Profile) (nativeReference, error) {
	if len(raw.raw) == 0 || len(raw.raw) > maxReferenceBytes {
		return nativeReference{}, failure(CodeInvalidReference, profile)
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw.raw)))
	decoder.DisallowUnknownFields()
	var reference nativeReference
	if err := decoder.Decode(&reference); err != nil {
		return nativeReference{}, failure(CodeInvalidReference, profile)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nativeReference{}, failure(CodeInvalidReference, profile)
	}
	if reference.Provider != provider || validateReference(reference) != nil {
		return nativeReference{}, failure(CodeInvalidReference, profile)
	}
	return reference, nil
}

func validateReference(reference nativeReference) error {
	if reference.Version != referenceVersion || !safeComponent(reference.SessionID) {
		return ErrInvalidReference
	}
	switch reference.Provider {
	case ProviderCodex:
		if reference.Scope != "" || reference.RecordKey != "" {
			return ErrInvalidReference
		}
	case ProviderClaude, ProviderGrok:
		if !safeComponent(reference.Scope) || reference.RecordKey != "" {
			return ErrInvalidReference
		}
	case ProviderGemini:
		if !safeComponent(reference.Scope) || !safeComponent(reference.RecordKey) {
			return ErrInvalidReference
		}
	default:
		return ErrInvalidReference
	}
	return nil
}

func safeComponent(component string) bool {
	if component == "" || len(component) > maxComponentBytes || strings.ContainsRune(component, 0) {
		return false
	}
	if component == "." || component == ".." || filepath.IsAbs(component) || filepath.VolumeName(component) != "" {
		return false
	}
	if strings.ContainsAny(component, `/\\`) {
		return false
	}
	decoded := component
	// Decode until stable so arbitrarily nested encodings cannot hide a dot
	// segment. Encoded separators are valid literal characters in some native
	// provider keys (notably Grok workspace keys), so only decoded traversal
	// segments and NUL are rejected; filesystem resolution never decodes them.
	for range maxComponentBytes {
		next, err := url.PathUnescape(decoded)
		if err != nil || strings.ContainsRune(next, 0) {
			return false
		}
		for _, segment := range strings.FieldsFunc(next, func(r rune) bool { return r == '/' || r == '\\' }) {
			if segment == "." || segment == ".." {
				return false
			}
		}
		if next == decoded {
			break
		}
		decoded = next
	}
	return true
}
