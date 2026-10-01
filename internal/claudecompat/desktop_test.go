package claudecompat

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ujzk/tele-agent/internal/dispatch"
	"github.com/ujzk/tele-agent/internal/testutil/claudetest"
)

// desktop is an interactive claude in a tele-like environment: PATH holds
// only a shim directory, in which the desktop programs are fakes that log
// their argv and play a clipboard (docs/claude-code.md "浏览器、剪贴板与通知").
// claude runs under strace, which tells the programs Claude starts from
// those its children start.
type desktop struct {
	*claudetest.Session
	api  *claudetest.API
	tr   *claudetest.Trace
	home string // Claude's HOME
	sess string // stands for the session directory; CLAUDE_CODE_TMPDIR is in it
	bin  string // stands for the shim directory
	log  string // the fakes' argv log
}

// desktopSpec configures a desktop.
type desktopSpec struct {
	// Programs are the desktop programs that exist, all of
	// dispatch.DesktopPrograms when nil.
	Programs []string
	// Clipboard is the MIME type the clipboard holds: image/png is served
	// from a PNG file, anything else as text.
	Clipboard string
	Env       []string
	Config    map[string]any
	Turns     []claudetest.Turn
	// Scripts are more programs in the shim directory, by name: shell
	// scripts written before claude starts, which looks some up at once.
	Scripts map[string]string
}

func newDesktop(t *testing.T, spec desktopSpec) *desktop {
	t.Helper()
	d := &desktop{home: t.TempDir(), sess: t.TempDir(), bin: t.TempDir(), log: filepath.Join(t.TempDir(), "argv")}
	d.tr = claudetest.NewTrace(t, claudetest.Require(t))
	if err := os.Mkdir(filepath.Join(d.sess, "tmp"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"bash", "rg", "git", "ps", "grep"} {
		claudetest.WriteScript(t, d.bin, name, `exec /usr/bin/`+name+` "$@"`+"\n")
	}
	img := filepath.Join(t.TempDir(), "clip.png")
	writePNG(t, img)
	// $1 is the program; the rest are its arguments.
	play := claudetest.WriteScript(t, t.TempDir(), "play", `name=$1; shift
case "$name $*" in
*TARGETS*|"wl-paste -l") echo '`+spec.Clipboard+`' ;;
*image/png*) [ '`+spec.Clipboard+`' = image/png ] && exec /bin/cat '`+img+`'; exit 1 ;;
*image/*) exit 1 ;;
*" -o"|*--output|*--no-newline) printf 'clip-text' ;;
*) exec /bin/cat >/dev/null ;;
esac
`)
	progs := spec.Programs
	if progs == nil {
		progs = dispatch.DesktopPrograms
	}
	for _, name := range progs {
		claudetest.WriteScript(t, d.bin, name, logArgv(d.log, play+" "+name))
	}
	for name, body := range spec.Scripts {
		claudetest.WriteScript(t, d.bin, name, body)
	}
	d.api = claudetest.NewAPI(t, spec.Turns...)
	d.Session = claudetest.Start(t, d.tr.Claude, d.api, claudetest.TTYOptions{
		Options: claudetest.Options{Home: d.home, Env: append([]string{
			"PATH=" + d.bin, "SHELL=" + d.bin + "/bash",
			"CLAUDE_CODE_SHELL=" + d.bin + "/bash",
			"CLAUDE_CODE_TMPDIR=" + d.sess + "/tmp",
			"USE_BUILTIN_RIPGREP=0",
		}, spec.Env...)},
		Config: spec.Config,
	})
	d.Ready()
	return d
}

// waitLogged waits until a fake desktop program named name has run, Claude
// starting it or one of Claude's scripts.
func (d *desktop) waitLogged(t *testing.T, name string) {
	t.Helper()
	ok := d.Poll(30*time.Second, func() bool {
		b, _ := os.ReadFile(d.log)
		return bytes.Contains(b, []byte("/"+name+"\x00"))
	})
	if !ok {
		t.Fatalf("%s did not run; screen:\n%s", name, claudetest.Text(d.Output()))
	}
}

// started stops claude and returns the desktop programs it started itself
// and the sh -c scripts it ran, other than the IDE detection (see ide).
// Each must be one tele runs locally; any other program must be a shim.
func (d *desktop) started(t *testing.T) (progs, scripts [][]string) {
	progs, scripts, _ = d.startedAll(t)
	return progs, scripts
}

// startedAll is started that also returns the IDE detection scripts.
func (d *desktop) startedAll(t *testing.T) (progs, scripts, ide [][]string) {
	t.Helper()
	d.Stop()
	seen := map[int]bool{} // children whose first program was seen
	for _, c := range d.tr.Calls(t) {
		// Only a child's first program is Claude's choice.
		if c.Name != "execve" || !c.Child || c.Failed() || seen[c.PID] {
			continue
		}
		seen[c.PID] = true
		p, argv := c.ExecPath(), c.ExecArgv()
		if argv == nil {
			t.Fatalf("cannot read the argv of execve(%s", c.Line)
		}
		name := filepath.Base(p)
		switch {
		case p == "/bin/sh" && len(argv) == 3 && strings.HasPrefix(argv[2], "ps aux | grep"):
			ide = append(ide, argv)
		case p == "/bin/sh":
			scripts = append(scripts, argv)
		case filepath.Dir(p) == d.bin && slices.Contains(dispatch.DesktopPrograms, name):
			progs = append(progs, argv)
		case filepath.Dir(p) == d.bin:
			continue
		default:
			t.Errorf("Claude started %s, neither a shim nor /bin/sh", p)
			continue
		}
		if act, err := dispatch.Classify(name, argv, d.sess, nil); err != nil || !act.Local {
			t.Errorf("Claude started %q, which tele does not run locally: %+v, %v", argv, act, err)
		}
	}
	return progs, scripts, ide
}

func writePNG(t *testing.T, name string) {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewGray(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestPasteImage pins how Claude reads an image from the clipboard: three
// sh -c scripts, which tele recognizes and runs locally, the second saving
// the image under CLAUDE_CODE_TMPDIR, from where Claude reads it.
func TestPasteImage(t *testing.T) {
	d := newDesktop(t, desktopSpec{Clipboard: "image/png", Env: []string{"DISPLAY=:0"}, Turns: []claudetest.Turn{claudetest.Say("seen")}})
	d.Type("\x16") // ctrl+v
	d.WaitText("[Image #1]")
	d.Type("look\r")
	d.WaitText("seen")
	var image bool
	for _, req := range d.api.AgentRequests() {
		for _, m := range req.Messages {
			image = image || slices.ContainsFunc(m.Content, func(b claudetest.Block) bool { return b.Type == "image" })
		}
	}
	if !image {
		t.Error("the image did not reach the API")
	}
	progs, scripts := d.started(t)
	if len(progs) != 0 || len(scripts) != 3 {
		t.Errorf("Claude started %q and ran %q, want three clipboard scripts", progs, scripts)
	}
	for _, s := range scripts {
		if strings.Contains(s[2], "claude_cli_latest_screenshot") && !strings.Contains(s[2], d.sess+"/tmp/claude-") {
			t.Errorf("the screenshot file is not in CLAUDE_CODE_TMPDIR: %q", s[2])
		}
	}
}

// TestPasteText pins the programs Claude reads text from the clipboard
// with when it holds no image: after the image probe, each it finds.
func TestPasteText(t *testing.T) {
	for _, tc := range []struct {
		name     string
		env      []string
		programs []string
		want     [][]string
	}{
		{"X11", []string{"DISPLAY=:0"}, []string{"xclip"}, [][]string{{"xclip", "-selection", "clipboard", "-o"}}},
		{"xsel", []string{"DISPLAY=:0"}, []string{"xsel"}, [][]string{{"xsel", "--clipboard", "--output"}}},
		{"Wayland", []string{"WAYLAND_DISPLAY=wayland-0"}, []string{"wl-paste"}, [][]string{{"wl-paste", "--no-newline"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newDesktop(t, desktopSpec{Clipboard: "text/plain", Env: tc.env, Programs: tc.programs})
			d.Type("\x16")
			d.WaitText("clip-text")
			progs, scripts := d.started(t)
			if !slices.EqualFunc(progs, tc.want, slices.Equal) || len(scripts) != 1 {
				t.Errorf("Claude started %q and ran %q, want %q after the image probe", progs, scripts, tc.want)
			}
		})
	}
}

// TestCopy pins the programs /copy writes the clipboard with, besides the
// OSC 52 sequence it sends the terminal, for each kind of display.
func TestCopy(t *testing.T) {
	for _, tc := range []struct {
		name     string
		env      []string
		programs []string
		want     [][]string
	}{
		{"X11", []string{"DISPLAY=:0"}, nil,
			[][]string{{"xclip", "-selection", "clipboard"}, {"xclip", "-selection", "primary"}}},
		{"xsel", []string{"DISPLAY=:0"}, []string{"xsel"},
			[][]string{{"xsel", "--clipboard", "--input"}, {"xsel", "--primary", "--input"}}},
		{"Wayland", []string{"WAYLAND_DISPLAY=wayland-0"}, []string{"wl-copy", "wl-paste"},
			[][]string{{"wl-copy"}, {"wl-copy", "--primary"}}},
		{"no display", nil, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newDesktop(t, desktopSpec{Clipboard: "text/plain", Env: tc.env, Programs: tc.programs, Turns: []claudetest.Turn{claudetest.Say("copy me")}})
			d.Type("hi\r")
			d.WaitText("copy me")
			d.Command("/copy")
			osc52 := "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte("copy me"))
			d.WaitFor("the OSC 52 sequence", func(out string) bool { return strings.Contains(out, osc52) })
			if tc.want != nil {
				d.waitLogged(t, tc.want[len(tc.want)-1][0])
			}
			d.WaitText("Copied")
			progs, scripts := d.started(t)
			if !slices.EqualFunc(progs, tc.want, slices.Equal) || len(scripts) != 0 {
				t.Errorf("Claude started %q and ran %q, want %q", progs, scripts, tc.want)
			}
		})
	}
}

// TestBrowser pins how Claude opens the browser for /login: xdg-open with
// the URL, or BROWSER instead when set, and only when a display is set.
func TestBrowser(t *testing.T) {
	login := func(t *testing.T, env []string) *desktop {
		d := newDesktop(t, desktopSpec{Env: env})
		d.Command("/login")
		d.WaitText("Select login method")
		d.Type("\r") // the first: a Claude account
		d.WaitText("oauth/authorize")
		return d
	}
	t.Run("xdg-open", func(t *testing.T) {
		d := login(t, []string{"DISPLAY=:0"})
		d.waitLogged(t, dispatch.NameXdgOpen)
		progs, _ := d.started(t)
		if len(progs) != 1 || len(progs[0]) != 2 || progs[0][0] != dispatch.NameXdgOpen || !strings.Contains(progs[0][1], "/oauth/authorize?") {
			t.Errorf("Claude started %q, want xdg-open <authorization URL>", progs)
		}
	})
	t.Run("BROWSER", func(t *testing.T) {
		log := filepath.Join(t.TempDir(), "argv")
		browser := claudetest.WriteScript(t, t.TempDir(), "browser", logArgv(log, "true"))
		d := login(t, []string{"DISPLAY=:0", "BROWSER=" + browser})
		if !d.Poll(30*time.Second, func() bool { _, err := os.Stat(log); return err == nil }) {
			t.Fatal("Claude did not start BROWSER")
		}
		if got := invocations(t, log); len(got) != 1 || len(got[0]) != 2 || !strings.Contains(got[0][1], "/oauth/authorize?") {
			t.Errorf("BROWSER got %q, want the authorization URL", got)
		}
		d.Stop()
		for _, c := range d.tr.Calls(t) {
			if c.Name == "execve" && c.Child && filepath.Base(c.ExecPath()) == dispatch.NameXdgOpen {
				t.Errorf("Claude started xdg-open besides BROWSER")
			}
		}
	})
	t.Run("no display", func(t *testing.T) {
		d := login(t, nil)
		d.Poll(3*time.Second, func() bool { return false })
		if progs, _ := d.started(t); len(progs) != 0 {
			t.Errorf("Claude started %q without a display", progs)
		}
	})
}

// TestNotification pins that Claude notifies through the terminal: it
// starts no program for a notification.
func TestNotification(t *testing.T) {
	d := newDesktop(t, desktopSpec{
		Env:    []string{"DISPLAY=:0"},
		Config: map[string]any{"preferredNotifChannel": "kitty", "messageIdleNotifThresholdMs": 1000},
		Turns:  []claudetest.Turn{claudetest.Say("done")},
	})
	d.Type("hi\r")
	d.WaitFor("the kitty notification", func(out string) bool { return strings.Contains(out, "\x1b]99;") })
	if progs, scripts := d.started(t); len(progs) != 0 || len(scripts) != 0 {
		t.Errorf("Claude started %q and ran %q", progs, scripts)
	}
}
