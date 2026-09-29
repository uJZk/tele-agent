package claudecompat

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/ujzk/tele-agent/internal/testutil/claudetest"
)

// TestOAuthRefreshThroughProxy pins that refreshing an expired OAuth access
// token goes through HTTPS_PROXY like the API requests (docs/claude-code.md
// "代理与 CA"): under tele a connection that bypasses the proxy would
// resolve the token host in the remote view.
//
// The credentials file holds an expired access token. Claude refreshes it
// at its token endpoint, which the proxy sends to the stand-in, and uses
// the new token for the API request that follows.
func TestOAuthRefreshThroughProxy(t *testing.T) {
	claude := hideHandoffCredentials(t, claudetest.Require(t))
	const tokenHost = "platform.claude.com"
	// The stand-in's certificate covers *.claude.com, so it serves both
	// the token endpoint and the API.
	api := claudetest.NewTLSAPI(t, "api.claude.com", claudetest.Say("refreshed"))
	proxy := claudetest.NewProxy(t, api.Addr(), "")
	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(home, ".claude", ".credentials.json"), map[string]any{
		"claudeAiOauth": map[string]any{
			"accessToken":      "sk-ant-oat01-tele-expired",
			"refreshToken":     "tele-refresh-1",
			"expiresAt":        time.Now().Add(-time.Hour).UnixMilli(),
			"scopes":           []string{"user:inference", "user:profile"},
			"subscriptionType": "pro",
		},
	})
	r := claudetest.Run(t, claude, api, claudetest.Options{
		Prompt: "hi",
		Home:   home,
		Args:   []string{"--max-turns", "1"},
		Env: append([]string{
			"ANTHROPIC_API_KEY=",
			"HTTPS_PROXY=" + proxy.URL(), "https_proxy=" + proxy.URL(),
		}, api.TrustEnv()...),
	})
	if !slices.Contains(proxy.Targets(), tokenHost+":443") || !slices.Contains(api.Pages(), tokenHost+"/v1/oauth/token") {
		t.Fatalf("proxy saw %q, pages %q; want the token refresh at %s through the proxy\nstderr: %s", proxy.Targets(), api.Pages(), tokenHost, r.Stderr)
	}
	r.Must(t)
	reqs := api.Requests()
	if len(reqs) == 0 || reqs[len(reqs)-1].Auth != "Bearer "+claudetest.RefreshedToken {
		t.Errorf("API requests %d, last Authorization %q; want the refreshed token", len(reqs), lastAuth(reqs))
	}
}

// handoffDir is where Claude Code running in Anthropic's cloud environment
// leaves credentials for its subprocesses. Claude reads them before its
// own credentials file, whatever HOME is, so a test that must use its own
// credentials hides the directory.
const handoffDir = "/home/claude/.claude/remote"

// hideHandoffCredentials returns claude, or, where handoffDir exists, a
// wrapper that runs claude with an empty file system mounted over it in
// namespaces of its own. It skips t when that is not possible.
func hideHandoffCredentials(t *testing.T, claude string) string {
	t.Helper()
	if _, err := os.Stat(handoffDir); err != nil {
		return claude
	}
	unshare, err := exec.LookPath("unshare")
	if err != nil {
		t.Skipf("%s exists and unshare is missing to hide it: %v", handoffDir, err)
	}
	w := claudetest.WriteScript(t, t.TempDir(), "claude-isolated",
		`exec `+unshare+` -rm /bin/sh -c 'mount -t tmpfs none `+handoffDir+` && exec "$0" "$@"' '`+claude+`' "$@"`+"\n")
	if out, err := exec.CommandContext(t.Context(), w, "--version").CombinedOutput(); err != nil {
		t.Skipf("cannot hide %s: %v: %s", handoffDir, err, out)
	}
	return w
}

func lastAuth(reqs []claudetest.Request) string {
	if len(reqs) == 0 {
		return ""
	}
	return reqs[len(reqs)-1].Auth
}
