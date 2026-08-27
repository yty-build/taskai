package connector_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yty-build/acp-gateway/internal/acp"
	"github.com/yty-build/acp-gateway/internal/connector"
	"github.com/yty-build/acp-gateway/internal/replay"
	"github.com/yty-build/acp-gateway/internal/testkit"
)

func TestACPConnectorHelperProcess(t *testing.T) {
	if os.Getenv("TASKAI_TEST_ACP_HELPER") != "1" {
		return
	}
	mode := os.Getenv("TASKAI_TEST_ACP_MODE")
	sessionID := "synthetic-native-session"
	cancelled := make(chan struct{})
	var cancelOnce sync.Once
	var agent *acp.Agent
	record := func(method string) {
		path := os.Getenv("TASKAI_TEST_ACP_TRACE")
		if path == "" {
			return
		}
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
		if err == nil {
			_, _ = file.WriteString(method + "\n")
			_ = file.Close()
		}
	}
	handler := acp.HandlerFuncs{
		Request: func(_ context.Context, message acp.Message) (json.RawMessage, *acp.ErrorObject) {
			record(message.Method)
			switch message.Method {
			case acp.MethodInitialize:
				return json.RawMessage(`{"protocolVersion":1,"agentCapabilities":{"sessionCapabilities":{"resume":true}}}`), nil
			case acp.MethodSessionNew:
				return json.RawMessage(`{"sessionId":"synthetic-native-session"}`), nil
			case acp.MethodSessionResume:
				var params struct {
					SessionID string `json:"sessionId"`
				}
				if json.Unmarshal(message.Params, &params) != nil || params.SessionID != sessionID {
					return nil, &acp.ErrorObject{Code: -32602, Message: "resume target rejected"}
				}
				return json.RawMessage(`{}`), nil
			case acp.MethodSessionPrompt:
				switch mode {
				case "burst":
					for sequence := 1; sequence <= 48; sequence++ {
						params, _ := json.Marshal(map[string]any{
							"sessionId": sessionID,
							"update":    map[string]any{"sessionUpdate": "future_public_shape", "sequence": sequence, "opaque": []any{3, 2, 1}},
						})
						if err := agent.Notify(acp.MethodSessionUpdate, params); err != nil {
							return nil, &acp.ErrorObject{Code: -32000, Message: "transport"}
						}
					}
				case "malformed":
					_, _ = os.Stdout.WriteString("{\"jsonrpc\":\n")
				case "oversized":
					payload := `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"synthetic-native-session","x":"` + strings.Repeat("x", acp.MaxFrameBytes) + `"}}\n`
					_, _ = os.Stdout.WriteString(payload)
				case "transport":
					_ = os.Stdout.Close()
					_ = os.Stdin.Close()
					return nil, &acp.ErrorObject{Code: -32000, Message: "transport"}
				case "foreign":
					foreign := json.RawMessage(`{"sessionId":"unrelated-session","update":{"sessionUpdate":"future_public_shape","opaque":true}}`)
					if err := agent.Notify(acp.MethodSessionUpdate, foreign); err != nil {
						return nil, &acp.ErrorObject{Code: -32000, Message: "transport"}
					}
				case "cancel":
					<-cancelled
					return json.RawMessage(`{"stopReason":"cancelled"}`), nil
				default:
					messageParams := json.RawMessage(`{"sessionId":"synthetic-native-session","update":{"sessionUpdate":"agent_message_chunk","messageId":"answer","content":{"type":"text","text":"synthetic"}}}`)
					if err := agent.Notify(acp.MethodSessionUpdate, messageParams); err != nil {
						return nil, &acp.ErrorObject{Code: -32000, Message: "transport"}
					}
					unknownParams := json.RawMessage(`{"sessionId":"synthetic-native-session", "update": {"sessionUpdate":"future_public_shape", "opaque": [3, 2, 1]}}`)
					if err := agent.Notify(acp.MethodSessionUpdate, unknownParams); err != nil {
						return nil, &acp.ErrorObject{Code: -32000, Message: "transport"}
					}
				}
				return json.RawMessage(`{"stopReason":"end_turn"}`), nil
			default:
				return nil, &acp.ErrorObject{Code: -32601, Message: "method not supported"}
			}
		},
		Notification: func(_ context.Context, message acp.Message) error {
			record(message.Method)
			if message.Method == acp.MethodSessionCancel {
				cancelOnce.Do(func() { close(cancelled) })
			}
			return nil
		},
	}
	agent = acp.NewAgent(os.Stdin, os.Stdout, handler)
	_ = agent.Start(context.Background())
	_ = agent.Wait()
}

func TestACPFactoryNewPromptUnknownFramesAndNativeCompletion(t *testing.T) {
	configureACPHelper(t, "normal", "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	factory := helperFactory(t)
	child, err := factory.New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Close() })
	if native, resolveErr := replay.ResolveCodexReference(child.NativeReference()); resolveErr != nil || native != "synthetic-native-session" {
		t.Fatalf("native reference did not resolve exactly: %v", resolveErr)
	}
	var frames []json.RawMessage
	completion, err := child.Prompt(ctx, "OPAQUE-NOT-LOGGED", func(frame json.RawMessage) error {
		frames = append(frames, append(json.RawMessage(nil), frame...))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(completion) != `{"stopReason":"end_turn"}` || len(frames) != 2 {
		t.Fatalf("completion=%s frames=%d", completion, len(frames))
	}
	var unknown struct {
		Params struct {
			Update struct {
				SessionUpdate string          `json:"sessionUpdate"`
				Opaque        json.RawMessage `json:"opaque"`
			} `json:"update"`
		} `json:"params"`
	}
	if err := json.Unmarshal(frames[1], &unknown); err != nil {
		t.Fatal(err)
	}
	var opaque []int
	if json.Unmarshal(unknown.Params.Update.Opaque, &opaque) != nil || unknown.Params.Update.SessionUpdate != "future_public_shape" || !reflect.DeepEqual(opaque, []int{3, 2, 1}) {
		t.Fatalf("unknown update changed: %s", frames[1])
	}
}

func TestACPFactoryExactResumeNeverStartsReplacementSession(t *testing.T) {
	trace := filepath.Join(t.TempDir(), "methods.log")
	configureACPHelper(t, "normal", trace)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	factory := helperFactory(t)
	reference, err := replay.NewCodexReference("synthetic-native-session")
	if err != nil {
		t.Fatal(err)
	}
	child, err := factory.Resume(ctx, reference)
	if err != nil {
		t.Fatal(err)
	}
	_ = child.Close()
	methods := readTrace(t, trace)
	if !reflect.DeepEqual(methods, []string{acp.MethodInitialize, acp.MethodSessionResume}) {
		t.Fatalf("Resume methods = %v", methods)
	}
	for _, method := range methods {
		if method == acp.MethodSessionNew {
			t.Fatal("Resume fell back to session/new")
		}
	}
}

func TestACPFactoryCreatesExactProviderReferenceEnvelopes(t *testing.T) {
	tests := []struct {
		name   string
		codec  connector.ProviderReferenceCodec
		assert func(*testing.T, replay.OpaqueRef)
	}{
		{
			name: "codex", codec: connector.CodexReferenceCodec{},
			assert: func(t *testing.T, reference replay.OpaqueRef) {
				session, err := replay.ResolveCodexReference(reference)
				if err != nil || session != "synthetic-native-session" {
					t.Fatalf("Codex reference resolution error = %v", err)
				}
			},
		},
		{
			name: "claude", codec: connector.ClaudeReferenceCodec{ProjectKey: "project-key"},
			assert: func(t *testing.T, reference replay.OpaqueRef) {
				scope, session, err := replay.ResolveClaudeReference(reference)
				if err != nil || scope != "project-key" || session != "synthetic-native-session" {
					t.Fatalf("Claude reference resolution error = %v", err)
				}
			},
		},
		{
			name: "gemini", codec: connector.GeminiReferenceCodec{
				ProjectHash: "project-hash",
				RecordKey:   func(string) (string, error) { return "session-record.json", nil },
			},
			assert: func(t *testing.T, reference replay.OpaqueRef) {
				scope, recordKey, session, err := replay.ResolveGeminiReference(reference)
				if err != nil || scope != "project-hash" || recordKey != "session-record.json" || session != "synthetic-native-session" {
					t.Fatalf("Gemini reference resolution error = %v", err)
				}
			},
		},
		{
			name: "grok", codec: connector.GrokReferenceCodec{WorkspaceKey: "workspace-key"},
			assert: func(t *testing.T, reference replay.OpaqueRef) {
				scope, session, err := replay.ResolveGrokReference(reference)
				if err != nil || scope != "workspace-key" || session != "synthetic-native-session" {
					t.Fatalf("Grok reference resolution error = %v", err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			configureACPHelper(t, "normal", "")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			factory := helperFactoryWithReferences(t, test.codec)
			child, err := factory.New(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer child.Close()
			resolved, err := test.codec.Resolve(child.NativeReference())
			if err != nil || resolved != "synthetic-native-session" {
				t.Fatalf("codec round trip error = %v", err)
			}
			test.assert(t, child.NativeReference())
		})
	}
}

func TestScopedReferenceCodecsRejectForeignScopeAndMapping(t *testing.T) {
	t.Parallel()
	claudeReference, _ := replay.NewClaudeReference("project-a", "session")
	if _, err := (connector.ClaudeReferenceCodec{ProjectKey: "project-b"}).Resolve(claudeReference); !errors.Is(err, replay.ErrInvalidReference) {
		t.Fatalf("foreign Claude scope error = %v", err)
	}
	geminiReference, _ := replay.NewGeminiReference("project", "record-a.json", "session")
	gemini := connector.GeminiReferenceCodec{
		ProjectHash: "project", RecordKey: func(string) (string, error) { return "record-b.json", nil },
	}
	if _, err := gemini.Resolve(geminiReference); !errors.Is(err, replay.ErrInvalidReference) {
		t.Fatalf("foreign Gemini record mapping error = %v", err)
	}
	if _, err := (connector.GeminiReferenceCodec{ProjectHash: "project"}).New("session"); !errors.Is(err, replay.ErrInvalidReference) {
		t.Fatalf("missing Gemini record mapping error = %v", err)
	}
	grokReference, _ := replay.NewGrokReference("workspace-a", "session")
	if _, err := (connector.GrokReferenceCodec{WorkspaceKey: "workspace-b"}).Resolve(grokReference); !errors.Is(err, replay.ErrInvalidReference) {
		t.Fatalf("foreign Grok scope error = %v", err)
	}
}

func TestClaudeAndGrokExactACPResumeUseSameOpaqueReference(t *testing.T) {
	tests := []struct {
		name  string
		codec connector.ProviderReferenceCodec
	}{
		{name: "claude", codec: connector.ClaudeReferenceCodec{ProjectKey: "project-key"}},
		{name: "grok", codec: connector.GrokReferenceCodec{WorkspaceKey: "workspace-key"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			trace := filepath.Join(t.TempDir(), "methods.log")
			configureACPHelper(t, "normal", trace)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			factory := helperFactoryWithReferences(t, test.codec)
			reference, err := test.codec.New("synthetic-native-session")
			if err != nil {
				t.Fatal(err)
			}
			child, err := factory.Resume(ctx, reference)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(child.NativeReference().Bytes(), reference.Bytes()) {
				t.Fatal("exact Resume changed opaque reference")
			}
			_ = child.Close()
			if methods := readTrace(t, trace); !reflect.DeepEqual(methods, []string{acp.MethodInitialize, acp.MethodSessionResume}) {
				t.Fatalf("Resume methods = %v", methods)
			}
		})
	}
}

func TestGeminiPinnedACPResumeFailsBeforeLaunch(t *testing.T) {
	t.Parallel()
	codec := connector.GeminiReferenceCodec{
		ProjectHash: "project-hash", RecordKey: func(string) (string, error) { return "record.json", nil },
	}
	reference, err := codec.New("session")
	if err != nil {
		t.Fatal(err)
	}
	factory := connector.NewGeminiSubordinateFactory(
		filepath.Join(t.TempDir(), "must-not-launch"), t.TempDir(), "project-hash", codec.RecordKey, nil,
	)
	if _, err := factory.Resume(context.Background(), reference); !errors.Is(err, connector.ErrProviderUnsupported) {
		t.Fatalf("Gemini Resume error = %v", err)
	}
}

func TestACPFactoryCancellationReturnsNativeCancelledCompletion(t *testing.T) {
	configureACPHelper(t, "cancel", "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	child, err := helperFactory(t).New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Close() })
	done := make(chan struct {
		completion json.RawMessage
		err        error
	}, 1)
	go func() {
		completion, promptErr := child.Prompt(ctx, "opaque", func(json.RawMessage) error { return nil })
		done <- struct {
			completion json.RawMessage
			err        error
		}{completion: completion, err: promptErr}
	}()
	if err := child.Cancel(ctx); err != nil {
		t.Fatal(err)
	}
	result := <-done
	if result.err != nil || string(result.completion) != `{"stopReason":"cancelled"}` {
		t.Fatalf("cancel completion=%s error=%v", result.completion, result.err)
	}
}

func TestACPFactoryIsolatesMalformedOversizedAndTransportLoss(t *testing.T) {
	for _, mode := range []string{"malformed", "oversized", "transport", "foreign"} {
		t.Run(mode, func(t *testing.T) {
			configureACPHelper(t, mode, "")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			child, err := helperFactory(t).New(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer child.Close()
			_, err = child.Prompt(ctx, "PRIVATE-TRAFFIC-CANARY", func(json.RawMessage) error { return nil })
			if !errors.Is(err, connector.ErrTransport) {
				t.Fatalf("Prompt() error = %v, want ErrTransport", err)
			}
			if strings.Contains(err.Error(), "PRIVATE") {
				t.Fatalf("traffic error leaked content: %v", err)
			}
		})
	}
}

func TestProductionRawReaderBlocksAtFollowHighWater(t *testing.T) {
	configureACPHelper(t, "burst", "")
	registry := connector.NewRegistry()
	factory := helperFactory(t)
	versions := connector.Versions{ACPProtocol: 1, Connector: "test-process"}
	profile := connector.NewConformedProfile(
		"fake-process",
		"Fake process ACP",
		versions,
		connector.Conformance{Exact: versions, Proven: connector.Capabilities{Subordinate: true}},
		nil,
		factory,
		factory,
		nil,
		nil,
		connector.ReplayBinding{},
	)
	if err := registry.Register(profile); err != nil {
		t.Fatal(err)
	}
	delegator, err := connector.NewDelegator(context.Background(), "manager", registry, &testkit.EventRecorder{}, &testkit.SequenceIDs{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = delegator.Close() })
	start := invokeResult(t, delegator, `{"operation":"start","provider":"fake-process","prompt":"opaque","mode":"follow"}`)
	waitForPauses(t, delegator, start.ChildSessionID, start.TurnToken, 1)
	stats, err := delegator.FollowStats(start.ChildSessionID, start.TurnToken)
	if err != nil {
		t.Fatal(err)
	}
	if stats.PeakFrames != connector.FollowHighWaterFrames || stats.Pauses < 1 || stats.PeakBytes > connector.FollowHighWaterBytes {
		t.Fatalf("raw-reader stats = %+v", stats)
	}
	collected := append([]json.RawMessage(nil), start.Frames...)
	result := start
	for result.More {
		result = invokeResult(t, delegator, encodeNext(result))
		if len(result.Frames) > connector.FollowBatchFrames {
			t.Fatalf("batch contains %d frames", len(result.Frames))
		}
		collected = append(collected, result.Frames...)
	}
	if len(collected) != 48 || result.Completion == nil || result.Completion.StopReason != "end_turn" {
		t.Fatalf("frames=%d completion=%+v", len(collected), result.Completion)
	}
	for index, frame := range collected {
		var envelope struct {
			Params struct {
				Update struct {
					Sequence int `json:"sequence"`
				} `json:"update"`
			} `json:"params"`
		}
		if json.Unmarshal(frame, &envelope) != nil || envelope.Params.Update.Sequence != index+1 {
			t.Fatalf("frame %d out of order: %s", index+1, frame)
		}
	}
}

func configureACPHelper(t *testing.T, mode, trace string) {
	t.Helper()
	t.Setenv("TASKAI_TEST_ACP_HELPER", "1")
	t.Setenv("TASKAI_TEST_ACP_MODE", mode)
	t.Setenv("TASKAI_TEST_ACP_TRACE", trace)
}

func helperFactory(t *testing.T) connector.ACPFactory {
	t.Helper()
	return helperFactoryWithReferences(t, connector.CodexReferenceCodec{})
}

func helperFactoryWithReferences(t *testing.T, references connector.ProviderReferenceCodec) connector.ACPFactory {
	t.Helper()
	return connector.ACPFactory{
		Command: connector.CommandSpec{
			Executable: os.Args[0],
			Args:       []string{"-test.run=^TestACPConnectorHelperProcess$"},
		},
		CWD:        t.TempDir(),
		References: references,
	}
}

func readTrace(t *testing.T, path string) []string {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var methods []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		methods = append(methods, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return methods
}
