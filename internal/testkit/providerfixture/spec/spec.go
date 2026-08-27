// Package spec defines the synthetic, test-owned contract shared by the
// provider fixture executable and the PTY acceptance driver. Production code
// does not import this package.
package spec

import "encoding/json"

const (
	RootEnvironment        = "TASKAI_TEST_PROVIDER_DIR"
	ReleaseFIFOEnvironment = "TASKAI_TEST_RELEASE_FIFO"

	TraceFilename   = "trace.jsonl"
	AuthFilename    = "auth.json"
	SessionsDirname = "sessions"
	LiveDirname     = "live"

	PromptCanary             = "TASKAI_ACCEPTANCE_PROMPT_CANARY_7f1d3a"
	ResumePromptCanary       = "TASKAI_ACCEPTANCE_RESUME_PROMPT_CANARY_993bc1"
	SubordinatePromptCanary  = "TASKAI_ACCEPTANCE_SUBORDINATE_PROMPT_CANARY_4d82e0"
	AnswerCanary             = "TASKAI_ACCEPTANCE_ANSWER_CANARY_1c5a9e"
	ToolCanary               = "TASKAI_ACCEPTANCE_TOOL_CANARY_f273b4"
	ReplayCanary             = "TASKAI_ACCEPTANCE_REPLAY_CANARY_a8d602"
	UserContextCanary        = "TASKAI_ACCEPTANCE_USER_CONTEXT_CANARY_33ef70"
	AuthCanary               = "TASKAI_ACCEPTANCE_AUTH_CANARY_81b70d"
	CredentialCanary         = "TASKAI_ACCEPTANCE_CREDENTIAL_CANARY_5e29f4"
	ProjectInstructionCanary = "TASKAI_ACCEPTANCE_PROJECT_INSTRUCTION_CANARY_c2a743"

	ManagerOutputCanary       = "TASKAI_ACCEPTANCE_MANAGER_OUTPUT_ANSWER_REPLAY_OK"
	ManagerResumeOutputCanary = "TASKAI_ACCEPTANCE_MANAGER_RESUME_CAPABILITY_OK"

	EventProcessStart        = "process_start"
	EventProcessExit         = "process_exit"
	EventAuthStatus          = "auth_status"
	EventAuthLogin           = "auth_login"
	EventAppRequest          = "app_request"
	EventThreadStart         = "thread_start"
	EventThreadResume        = "thread_resume"
	EventThreadRead          = "thread_read"
	EventTurnStart           = "turn_start"
	EventToolCall            = "tool_call"
	EventToolResult          = "tool_result"
	EventManagerOutput       = "manager_output"
	EventACPRequest          = "acp_request"
	EventSessionNew          = "session_new"
	EventSessionResume       = "session_resume"
	EventSubordinateWaiting  = "subordinate_waiting"
	EventSubordinateReleased = "subordinate_released"
	EventSubordinateAnswer   = "subordinate_answer"
)

// TraceEvent is append-only synthetic provider evidence. Params may contain
// test canaries, so the fixture writes it only under the provider-owned root.
type TraceEvent struct {
	Event      string          `json:"event"`
	PID        int             `json:"pid"`
	ParentPID  int             `json:"parent_pid"`
	Executable string          `json:"executable,omitempty"`
	Argv       []string        `json:"argv,omitempty"`
	Method     string          `json:"method,omitempty"`
	ThreadID   string          `json:"thread_id,omitempty"`
	SessionID  string          `json:"session_id,omitempty"`
	TurnID     string          `json:"turn_id,omitempty"`
	Operation  string          `json:"operation,omitempty"`
	Connected  *bool           `json:"connected,omitempty"`
	Params     json.RawMessage `json:"params,omitempty"`
}

// PersistenceCanaries returns content that is allowed under the synthetic
// provider root but forbidden anywhere beneath TaskAI's state root.
func PersistenceCanaries() []string {
	return []string{
		PromptCanary,
		ResumePromptCanary,
		SubordinatePromptCanary,
		AnswerCanary,
		ToolCanary,
		ReplayCanary,
		UserContextCanary,
		AuthCanary,
		CredentialCanary,
		ProjectInstructionCanary,
		ManagerOutputCanary,
		ManagerResumeOutputCanary,
	}
}
