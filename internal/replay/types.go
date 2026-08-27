package replay

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
)

// ProviderID identifies one provider connector without normalizing its content.
type ProviderID string

const (
	ProviderCodex  ProviderID = "codex"
	ProviderClaude ProviderID = "claude-code"
	ProviderGemini ProviderID = "gemini-cli"
	ProviderGrok   ProviderID = "grok-build"
)

// ProfileID identifies one exact, executable native-history conformance profile.
type ProfileID string

const (
	ProfileCodex01501  ProfileID = "codex/0.150.1/app-server-thread-read-v1"
	ProfileClaude21226 ProfileID = "claude-code/2.1.226/project-session-jsonl-v1"
	ProfileGemini0250  ProfileID = "gemini-cli/0.25.0-preview.0/conversation-record-json-v1"
	ProfileGrok105     ProfileID = "grok-build/1.0.5/updates-jsonl-v1"
)

// Profile describes a version-pinned native history boundary. Fingerprint is a
// structural schema identifier, never a digest of provider Session content.
type Profile struct {
	ID               ProfileID
	Provider         ProviderID
	InstalledVersion string
	Source           string
	Fingerprint      string
}

const (
	codexSchemaDescriptor  = "codex:thread{id:string,turns:[{id:string,status:enum,items:[object]}]}"
	claudeSchemaDescriptor = "claude:jsonl{sessionId:string,version:string,type:string,uuid?:string,parentUuid?:string,message?:object,subtype?:string}"
	geminiSchemaDescriptor = "gemini:ConversationRecord{sessionId:string,projectHash:string,messages:[object]}"
	grokSchemaDescriptor   = "grok:jsonl{method:string,params:{sessionId:string,update:{sessionUpdate:object}}}"
)

var profiles = []Profile{
	{
		ID:               ProfileCodex01501,
		Provider:         ProviderCodex,
		InstalledVersion: "0.150.1",
		Source:           "app-server:thread/read(includeTurns=true)",
		Fingerprint:      schemaFingerprint(codexSchemaDescriptor),
	},
	{
		ID:               ProfileClaude21226,
		Provider:         ProviderClaude,
		InstalledVersion: "2.1.226",
		Source:           "projects/<project-key>/<session-id>.jsonl",
		Fingerprint:      schemaFingerprint(claudeSchemaDescriptor),
	},
	{
		ID:               ProfileGemini0250,
		Provider:         ProviderGemini,
		InstalledVersion: "0.25.0-preview.0",
		Source:           "tmp/<project-hash>/chats/<record-key>.json",
		Fingerprint:      schemaFingerprint(geminiSchemaDescriptor),
	},
	{
		ID:               ProfileGrok105,
		Provider:         ProviderGrok,
		InstalledVersion: "1.0.5",
		Source:           "sessions/<workspace-key>/<session-id>/updates.jsonl",
		Fingerprint:      schemaFingerprint(grokSchemaDescriptor),
	},
}

func schemaFingerprint(descriptor string) string {
	digest := sha256.Sum256([]byte(descriptor))
	return fmt.Sprintf("sha256:%x", digest)
}

// Profiles returns a copy of the exact profiles compiled into this build.
func Profiles() []Profile {
	result := make([]Profile, len(profiles))
	copy(result, profiles)
	return result
}

// ConformedProfile returns an exact profile match. It deliberately performs no
// SemVer range matching.
func ConformedProfile(provider ProviderID, installedVersion string) (Profile, bool) {
	for _, profile := range profiles {
		if profile.Provider == provider && profile.InstalledVersion == installedVersion {
			return profile, true
		}
	}
	return Profile{}, false
}

// OpaqueRef is a defensive-copy wrapper around the exact provider reference
// stored on a Session and consumed by both Resume and Replay. It intentionally
// has no String or text-marshalling method.
type OpaqueRef struct {
	raw []byte
}

// OpaqueRefFromBytes restores an opaque provider reference from metadata-only
// persistence without interpreting it in core code.
func OpaqueRefFromBytes(raw []byte) OpaqueRef {
	return OpaqueRef{raw: append([]byte(nil), raw...)}
}

// Bytes returns a defensive copy suitable for metadata-only persistence.
func (reference OpaqueRef) Bytes() []byte {
	return append([]byte(nil), reference.raw...)
}

// Request selects exactly Count completed invocations from one exact native
// Session. TargetID is transient, content-free correlation supplied by the
// owning connector.
type Request struct {
	TargetID         string
	Provider         ProviderID
	InstalledVersion string
	Reference        OpaqueRef
	Count            uint32
}

// BatchRequest is prepared atomically: every target must validate before any
// Sink method is called.
type BatchRequest struct {
	Targets []Request
}

// TargetBoundary identifies a transient Replay target without exposing its
// opaque provider reference.
type TargetBoundary struct {
	TargetID string
	Provider ProviderID
	Profile  ProfileID
}

// NativeBoundary exposes only native mechanical invocation identity and status.
// A terminal status does not imply success.
type NativeBoundary struct {
	InvocationID string
	Status       string
}

// NativeRecord is one provider-native JSON item in original order.
type NativeRecord struct {
	JSON json.RawMessage
}

// Sink is implemented by the connector-owned typed delegation capability.
// TaskAI core and persistence must not implement or receive this interface.
type Sink interface {
	BeginTarget(TargetBoundary) error
	BeginInvocation(NativeBoundary) error
	Record(NativeRecord) error
	EndInvocation(NativeBoundary) error
	EndTarget(TargetBoundary) error
}

// CodexThreadReader is the only Codex content port. Implementations must issue
// the supported app-server thread/read request with includeTurns=true and return
// the raw thread object, never enumerate threads or read rollout files.
type CodexThreadReader interface {
	ThreadRead(context.Context, string) (json.RawMessage, error)
}

// Dependencies are connector-owned native history boundaries.
//
// Roots are provider configuration roots (for example CLAUDE_CONFIG_DIR), not
// transcript paths. Missing roots make only that provider unavailable.
type Dependencies struct {
	Codex CodexThreadReader
	Roots map[ProviderID]string
}

// Limits are hard safety bounds. MaxSelectedBytes bounds raw native JSON for
// one target. MaxBatchBytes cumulatively bounds prepared raw JSON plus
// conservative slice/boundary overhead across every target. One current
// source snapshot, bounded by MaxSourceBytes, is transient during preparation.
// Zero fields use DefaultLimits.
type Limits struct {
	MaxTargets       int
	MaxCount         uint32
	MaxSourceBytes   int64
	MaxRecordBytes   int
	MaxRecords       int
	MaxSelectedBytes int64
	MaxBatchBytes    int64
}

// DefaultLimits bounds native reads without spilling content to disk.
var DefaultLimits = Limits{
	MaxTargets:       16,
	MaxCount:         32,
	MaxSourceBytes:   128 << 20,
	MaxRecordBytes:   32 << 20,
	MaxRecords:       250_000,
	MaxSelectedBytes: 64 << 20,
	MaxBatchBytes:    128 << 20,
}
