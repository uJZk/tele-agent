package claudecompat

import (
	"os/exec"
	"testing"

	"github.com/ujzk/tele-agent/internal/testutil/claudetest"
)

// TestNoProcNoStart pins what makes the launch view fail closed
// (docs/filesystem.md "已知陷阱"): tele execs Claude in a view whose /proc
// is empty, and Claude's runtime cannot start there, so a Claude whose
// preload was ignored never runs in the local view. It must end before
// sending any request.
func TestNoProcNoStart(t *testing.T) {
	claude := claudetest.Require(t)
	unshare, err := exec.LookPath("unshare")
	if err != nil {
		t.Skipf("unshare: %v", err)
	}
	w := claudetest.WriteScript(t, t.TempDir(), "claude-noproc",
		`exec `+unshare+` -rm /bin/sh -c 'mount -t tmpfs -o ro none /proc && exec "$0" "$@"' '`+claude+`' "$@"`+"\n")
	if out, err := exec.CommandContext(t.Context(), unshare, "-rm", "true").CombinedOutput(); err != nil {
		t.Skipf("no mount namespace for the test: %v: %s", err, out)
	}
	api := claudetest.NewAPI(t, claudetest.Say("must not run"))
	r := claudetest.Run(t, w, api, claudetest.Options{Prompt: "hi", Args: []string{"--max-turns", "1"}})
	if r.Err == nil {
		t.Fatalf("claude ran without /proc: result %q", r.Output.Result)
	}
	if n := len(api.Requests()); n != 0 {
		t.Fatalf("claude sent %d requests without /proc", n)
	}
}
