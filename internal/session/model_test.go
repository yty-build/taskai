package session

import (
	"testing"
	"time"
)

func TestStatuses(t *testing.T) {
	for _, value := range []string{"starting", "WORKING", " waiting ", "done", "failed", "cancelled", "stopped"} {
		if _, err := ParseStatus(value); err != nil {
			t.Errorf("ParseStatus(%q): %v", value, err)
		}
	}
	if _, err := ParseStatus("paused"); err == nil {
		t.Fatal("ParseStatus(paused) error = nil")
	}
}

func TestCreateParamsValidation(t *testing.T) {
	if err := (CreateParams{Agent: "codex"}).Validate(); err == nil {
		t.Fatal("missing Task ID accepted")
	}
	if err := (CreateParams{TaskID: "T-1"}).Validate(); err == nil {
		t.Fatal("missing agent accepted")
	}
	if err := (CreateParams{TaskID: "T-1", Agent: "codex", MetadataJSON: []byte("no")}).Validate(); err == nil {
		t.Fatal("invalid metadata accepted")
	}
	verifiedAt := time.Now().UTC()
	for name, params := range map[string]CreateParams{
		"provider store without timestamp": {
			TaskID: "T-1", Agent: "codex", ProviderConversationID: "canonical", ConversationIDSource: ConversationSourceProviderStore,
		},
		"timestamp without provider store": {
			TaskID: "T-1", Agent: "codex", ProviderConversationID: "canonical", ConversationIDSource: ConversationSourceACP, ConversationIDVerifiedAt: &verifiedAt,
		},
		"verified native mismatch": {
			TaskID: "T-1", Agent: "codex", ProviderConversationID: "canonical", NativeSessionID: "replacement",
			ConversationIDSource: ConversationSourceProviderStore, ConversationIDVerifiedAt: &verifiedAt,
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := params.Validate(); err == nil {
				t.Fatal("invalid identity state accepted")
			}
		})
	}
}
