package native

import "testing"

func TestTmuxHarnessUsesDerivedNameAndShellQuotesOnlyPrivateEntrypoint(t *testing.T) {
	if got, want := TmuxSessionName("S-ABC_123"), "agent-S-ABC_123"; got != want {
		t.Fatalf("TmuxSessionName() = %q, want %q", got, want)
	}
	got := shellJoin("/tmp/agent's binary", "_native", "--session", "S-safe")
	want := `'/tmp/agent'"'"'s binary' '_native' '--session' 'S-safe'`
	if got != want {
		t.Fatalf("shellJoin() = %q, want %q", got, want)
	}
}
