package claudecompat

import (
	"testing"

	"github.com/ujzk/tele-agent/internal/claudever"
	"github.com/ujzk/tele-agent/internal/testutil/claudetest"
)

// TestVersionVerified keeps claudever.Verified the record of what these
// tests passed with (docs/claude-code.md): it fails for a version that is
// not listed. List the version once every other test passes with it.
func TestVersionVerified(t *testing.T) {
	claude := claudetest.Require(t)
	v, err := claudever.Version(t.Context(), claude)
	if err != nil {
		t.Fatal(err)
	}
	if !claudever.IsVerified(v) {
		t.Fatalf("Claude Code %s is not in claudever.Verified; add it once the other compatibility tests pass", v)
	}
}
