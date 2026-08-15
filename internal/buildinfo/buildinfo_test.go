package buildinfo

import "testing"

func TestCurrentAndStringExposeInjectedBuildMetadata(t *testing.T) {
	originalVersion, originalCommit, originalDate := Version, Commit, Date
	t.Cleanup(func() { Version, Commit, Date = originalVersion, originalCommit, originalDate })
	Version, Commit, Date = "1.2.3", "abc123", "2026-08-14T00:00:00Z"
	current := Current()
	if current.Version != Version || current.Commit != Commit || current.Date != Date {
		t.Fatalf("Current() = %#v", current)
	}
	if got, want := current.String(), "acpctl 1.2.3 (commit abc123, built 2026-08-14T00:00:00Z)"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}
