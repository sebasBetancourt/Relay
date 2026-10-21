package buildinfo

import (
	"strings"
	"testing"
)

func TestStringUsesStampedValues(t *testing.T) {
	v, c, d := Version, Commit, Date
	t.Cleanup(func() { Version, Commit, Date = v, c, d })

	Version, Commit, Date = "v0.1.0", "abcdef1234567", "2026-10-25T10:00:00Z"
	if got := String(); got != "v0.1.0 (abcdef1, 2026-10-25T10:00:00Z)" {
		t.Fatalf("String() = %q", got)
	}
}

func TestStringDefaultsToDev(t *testing.T) {
	if got := String(); !strings.HasPrefix(got, "dev") {
		t.Fatalf("unstamped build should report dev, got %q", got)
	}
}
