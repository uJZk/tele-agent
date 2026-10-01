package claudecompat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ujzk/tele-agent/internal/dispatch"
)

// fakeEditor is a terminal editor that logs its argv, then appends a line
// to the last argument, the file.
func fakeEditor(log string) string {
	return logArgv(log, "/bin/sh -c 'for f; do :; done; printf \"from the editor\\n\" >> \"$f\"' editor") + "\n"
}

// TestEditor pins how Claude starts the editor tele gives it in VISUAL:
// by that name, with exactly one argument, the absolute path of the file,
// for the prompt (ctrl+g, a file in CLAUDE_CODE_TMPDIR) and for a memory
// file (/memory); and it reads the file back after the editor exits
// (docs/claude-code.md "外部编辑器与 IDE 探测").
func TestEditor(t *testing.T) {
	log := filepath.Join(t.TempDir(), "editor")
	d := newDesktop(t, desktopSpec{
		Env:     []string{"VISUAL=" + dispatch.NameEditor},
		Scripts: map[string]string{dispatch.NameEditor: fakeEditor(log)},
	})
	d.Type("draft prompt")
	d.WaitText("draft prompt")
	d.Type("\x07") // ctrl+g
	d.WaitText("from the editor")

	d.Type("\x15") // clear the prompt
	d.Command("/memory")
	d.WaitText("User instructions")
	d.Type("\r") // the first: ~/.claude/CLAUDE.md
	d.WaitText("Opened")

	got := invocations(t, log)
	if len(got) != 2 {
		t.Fatalf("editor ran %d times: %q", len(got), got)
	}
	for _, argv := range got {
		if len(argv) != 2 || filepath.Base(argv[0]) != dispatch.NameEditor {
			t.Errorf("editor started as %q, want %s <file>", argv, dispatch.NameEditor)
			continue
		}
		if act, err := dispatch.Classify(dispatch.NameEditor, argv, d.sess, nil); err != nil || act.Edit != argv[1] {
			t.Errorf("Classify(%q) = %+v, %v", argv, act, err)
		}
	}
	if prompt := got[0][1]; !strings.HasPrefix(prompt, d.sess+"/tmp/claude-") || !strings.HasSuffix(prompt, ".md") {
		t.Errorf("the prompt file %s is not in CLAUDE_CODE_TMPDIR", prompt)
	}
	if memory := got[1][1]; memory != filepath.Join(d.home, ".claude", "CLAUDE.md") {
		t.Errorf("/memory opened %s, want ~/.claude/CLAUDE.md", memory)
	}
}

// TestEditorChoice pins how Claude chooses the editor without VISUAL and
// EDITOR, which tele-editor repeats with the user's PATH: the first of
// code, vi and nano it finds, looked up once at start.
func TestEditorChoice(t *testing.T) {
	log := filepath.Join(t.TempDir(), "editor")
	d := newDesktop(t, desktopSpec{Scripts: map[string]string{
		"nano": fakeEditor(log),
		"vi":   fakeEditor(log),
	}})
	d.Type("draft")
	d.WaitText("draft")
	d.Type("\x07")
	d.WaitText("from the editor")
	if got := invocations(t, log); len(got) != 1 || filepath.Base(got[0][0]) != "vi" {
		t.Errorf("Claude started %q, want vi", got)
	}
}

// TestIDEDetection pins the script Claude runs at start to find the IDEs
// running on this machine: exactly the one tele runs locally.
func TestIDEDetection(t *testing.T) {
	log := filepath.Join(t.TempDir(), "ps")
	d := newDesktop(t, desktopSpec{Scripts: map[string]string{"ps": logArgv(log, "/usr/bin/ps")}})
	if !d.Poll(30*time.Second, func() bool { _, err := os.Stat(log); return err == nil }) {
		t.Fatal("Claude did not look for IDEs")
	}
	_, _, ide := d.startedAll(t)
	if len(ide) == 0 {
		t.Fatal("Claude did not look for IDEs")
	}
	for _, argv := range ide {
		act, err := dispatch.Classify("sh", argv, d.sess, nil)
		if err != nil || !act.Local {
			t.Errorf("Claude looked for IDEs with %q, which tele does not run locally", argv)
		}
	}
}
