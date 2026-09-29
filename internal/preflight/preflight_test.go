package preflight

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ujzk/tele-agent/internal/manifest"
)

// item is a check whose state a repair changes.
type item struct {
	status   Status
	declined Status
	fixed    bool
}

func (it *item) check(name string) Check {
	return Check{Name: name, Run: func(context.Context) Result {
		if it.fixed {
			return Result{Detail: "fixed"}
		}
		return Result{
			Status:   it.status,
			Detail:   "broken",
			Fix:      func(context.Context, Recorder) error { it.fixed = true; return nil },
			Commands: []string{"fix " + name},
			Undo:     &manifest.Entry{ID: name, Undo: "unfix " + name},
			Declined: it.declined,
		}
	}}
}

type recorder struct{ ids []string }

func (r *recorder) Record(e manifest.Entry) error { r.ids = append(r.ids, e.ID); return nil }

func (r *recorder) WriteFile(string, string, string, []byte, os.FileMode) error { return nil }

func TestRun(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mode     Mode
		answer   string // nil terminal when empty
		declined Status
		ok       bool
		ran      []string
	}{
		{"no terminal declines", Repair, "", Warn, true, nil},
		{"no terminal declines a required item", Repair, "", Fatal, false, nil},
		{"consent", Repair, "y\n", Fatal, true, []string{"fix consent"}},
		{"refusal", Repair, "n\n", Fatal, false, nil},
		{"yes", Yes, "", Fatal, true, []string{"fix consent"}},
		{"check only", CheckOnly, "y\n", Warn, false, nil},
		{"print commands", PrintCommands, "y\n", Warn, false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixable := &item{status: Fixable}
			consent := &item{status: Consent, declined: tc.declined}
			var ran []string
			var out bytes.Buffer
			rec := &recorder{}
			r := &Runner{Mode: tc.mode, Out: &out, Recorder: rec, Shell: func(_ context.Context, c string) error {
				ran = append(ran, c)
				consent.fixed = true
				return nil
			}}
			if tc.answer != "" {
				r.Terminal = readWriter{strings.NewReader(tc.answer), &out}
			}
			ok := r.Run(t.Context(), []Check{fixable.check("fixable"), consent.check("consent"), {Name: "fine", Run: func(context.Context) Result { return Result{} }}})
			if ok != tc.ok || strings.Join(ran, ",") != strings.Join(tc.ran, ",") {
				t.Fatalf("ok = %v, ran %q; want %v, %q\n%s", ok, ran, tc.ok, tc.ran, out.String())
			}
			repairs := tc.mode == Repair || tc.mode == Yes
			if fixable.fixed != repairs {
				t.Errorf("fixable item fixed = %v", fixable.fixed)
			}
			if len(tc.ran) > 0 && strings.Join(rec.ids, ",") != "consent" {
				t.Errorf("recorded %q", rec.ids)
			}
			if !consent.fixed && tc.mode != CheckOnly && !strings.Contains(out.String(), "    fix consent") {
				t.Errorf("the command of the pending item is not shown:\n%s", out.String())
			}
		})
	}
}

func TestShellQuote(t *testing.T) {
	for _, s := range []string{"plain", "", "it's", "a b", "$HOME", "`x`", "a\nb"} {
		out, err := exec.CommandContext(t.Context(), "sh", "-c", "printf %s "+ShellQuote(s)).Output()
		if err != nil || string(out) != s {
			t.Errorf("ShellQuote(%q) = %s: sh prints %q, %v", s, ShellQuote(s), out, err)
		}
	}
}

func TestWriteRootFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "it's here")
	content := "a = $HOME `x`\n'quoted'"
	cmd := strings.Replace(WriteRootFile(path, content), "sudo tee", "tee", 1)
	if err := exec.CommandContext(t.Context(), "sh", "-c", cmd).Run(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != content+"\n" {
		t.Fatalf("wrote %q, %v", b, err)
	}
}
