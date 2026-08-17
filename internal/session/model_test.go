package session

import (
	"encoding/json"
	"strings"
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

func TestProviderOptionsFromSessionMetadata(t *testing.T) {
	options, err := ParseProviderOptions(json.RawMessage(`{"permission":"read-approval","model":" org/模型@edge+v=2 ","reasoning_effort":" high "}`))
	if err != nil {
		t.Fatal(err)
	}
	if options.Model != "org/模型@edge+v=2" || options.ReasoningEffort != "high" {
		t.Fatalf("options = %#v", options)
	}
	for name, model := range map[string]string{
		"leading option": "--dangerous", "bidi override": "safe\u202Eevil", "line separator": "safe\u2028evil", "nul": "safe\x00evil",
	} {
		t.Run(name, func(t *testing.T) {
			body, marshalErr := json.Marshal(map[string]string{"model": model})
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			if _, err := ParseProviderOptions(body); err == nil {
				t.Fatalf("invalid model %q was accepted", model)
			}
		})
	}
	tooLong, err := json.Marshal(map[string]string{"model": strings.Repeat("界", 86)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseProviderOptions(tooLong); err == nil || !strings.Contains(err.Error(), "256 bytes") {
		t.Fatalf("overlong model error = %v", err)
	}
	if err := (ProviderOptions{Model: string([]byte{0xff})}).Validate(); err == nil {
		t.Fatal("invalid UTF-8 model was accepted")
	}
	if _, err := ParseProviderOptions(json.RawMessage(`{"reasoning_effort":"high;override=true"}`)); err == nil || !strings.Contains(err.Error(), "reasoning effort") {
		t.Fatalf("invalid effort error = %v", err)
	}
	if _, err := ParseProviderOptions(json.RawMessage(`{"model":42}`)); err == nil || !strings.Contains(err.Error(), "must be a string") {
		t.Fatalf("non-string model error = %v", err)
	}
}
