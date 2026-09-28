package dispatch

import (
	"bytes"
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// These tests check the parsers against the shells they model. The shells
// are an oracle only; the tests skip where they are missing.

func requireShell(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Skipf("%s not available: %v", path, err)
	}
}

func runShell(t *testing.T, argv ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir(), "LC_ALL=C.UTF-8"}
	cmd.Dir = t.TempDir()
	out, err := cmd.Output()
	if err != nil && ctx.Err() != nil {
		t.Fatalf("%q: %v", argv, err)
	}
	return out
}

// TestPlainWordsMatchesShells runs every accepted script through dash and
// bash in one batch and compares the words they pass to printf.
func TestPlainWordsMatchesShells(t *testing.T) {
	var scripts []string
	for _, tt := range plainWordsTests {
		// A tilde prefix is deliberately kept for the target host to expand.
		if tt.ok && !strings.Contains(tt.script, "~") {
			scripts = append(scripts, tt.script)
		}
	}
	rng := rand.New(rand.NewPCG(1, 2))
	for range 300 {
		x := randomWord(rng)
		for _, q := range []func(string) string{quoteSingle, quoteDouble, quoteBackslash} {
			scripts = append(scripts, q(testTeleExec)+" "+q(x))
		}
	}

	var src strings.Builder
	for i, s := range scripts {
		fmt.Fprintf(&src, "printf '%%s\\0' '\x01%d' %s\n", i, s)
	}
	for _, sh := range []string{"/bin/dash", "/bin/bash"} {
		t.Run(sh, func(t *testing.T) {
			requireShell(t, sh)
			got := splitRecords(runShell(t, sh, "-c", src.String()))
			if len(got) != len(scripts) {
				t.Fatalf("%s printed %d records, want %d", sh, len(got), len(scripts))
			}
			for i, s := range scripts {
				want, _ := plainWords(s)
				if !slices.Equal(got[i], want) {
					t.Errorf("%s -c %q: shell words %q, plainWords %q", sh, s, got[i], want)
				}
			}
		})
	}
}

// randomWord returns a string weighted towards shell metacharacters. It
// has no NUL (impossible in argv) and no \x01 (the record marker).
func randomWord(rng *rand.Rand) string {
	const special = " \t\n'\"\\$`;&|<>()*?[]{}#~!=%^,:@-+é"
	b := make([]byte, rng.IntN(12))
	for i := range b {
		if rng.IntN(2) == 0 {
			b[i] = special[rng.IntN(len(special))]
		} else {
			b[i] = byte(2 + rng.IntN(254))
		}
	}
	return string(b)
}

// splitRecords splits the NUL-terminated fields "\x01<i>", word, word,
// "\x01<i+1>", ... into records.
func splitRecords(out []byte) map[int][]string {
	recs := map[int][]string{}
	cur := -1
	fields := bytes.Split(out, []byte{0})
	for _, f := range fields[:len(fields)-1] {
		if n, ok := bytes.CutPrefix(f, []byte{1}); ok {
			cur, _ = strconv.Atoi(string(n))
			recs[cur] = nil
			continue
		}
		recs[cur] = append(recs[cur], string(f))
	}
	return recs
}

// TestBashScriptIndexMatchesBash runs bash with every option vector and
// checks that it executes the argument bashScriptIndex points at, and none
// when it returns -1.
func TestBashScriptIndexMatchesBash(t *testing.T) {
	requireShell(t, "/bin/bash")
	const mark = "SCRIPT-RAN"
	for _, tt := range bashScriptTests {
		args := slices.Clone(tt.args)
		for i, a := range args {
			if a == "S" {
				args[i] = "printf %s " + mark
			}
		}
		out := runShell(t, append([]string{"/bin/bash"}, args...)...)
		ran := bytes.Contains(out, []byte(mark))
		if ran != (tt.want >= 0) {
			t.Errorf("bash %q: ran script = %v, bashScriptIndex = %d", tt.args, ran, tt.want)
		}
	}
}

// TestOptionLikeCommandRuns runs Classify's result for commands that start
// with '-' or '+' through the shells and checks that they run as commands
// rather than being parsed as shell options.
func TestOptionLikeCommandRuns(t *testing.T) {
	const mark = "SCRIPT-RAN"
	var invocations [][]string
	for _, lead := range []string{"-e", "+x", "--", "-"} {
		// The first word fails as a command; the second proves the whole
		// string ran as a script.
		inner := lead + " 2>/dev/null; printf %s " + mark
		invocations = append(invocations,
			[]string{nameTeleExec, inner},
			[]string{nameSh, "-c", wrap("single", inner), "a0"},
			[]string{nameBash, "-c", wrap("single", inner)},
			[]string{nameBash, "-lc", wrap("double", inner), "a0"},
			[]string{nameBash, "-o", "pipefail", "-c", "--", wrap("backslash", inner)},
		)
	}
	shells := map[string][]string{nameSh: {"/bin/dash", "/bin/bash"}, nameBash: {"/bin/bash"}}
	for _, argv := range invocations {
		act, err := Classify(argv[0], argv, testSess, testLocal)
		if err != nil {
			t.Fatalf("Classify(%q): %v", argv, err)
		}
		for _, sh := range shells[act.Argv[0]] {
			if _, err := os.Stat(sh); err != nil {
				continue // the other shells still check the argv
			}
			if out := runShell(t, append([]string{sh}, act.Argv[1:]...)...); !bytes.Contains(out, []byte(mark)) {
				t.Errorf("%s %q (from %q) did not run the command", sh, act.Argv[1:], argv)
			}
		}
	}
}
