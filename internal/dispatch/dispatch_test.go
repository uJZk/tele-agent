package dispatch

import (
	"errors"
	"fmt"
	"path"
	"reflect"
	"slices"
	"strings"
	"testing"
)

const (
	testSess     = "/.tele/0123456789abcdef"
	testTeleExec = testSess + "/bin/tele-exec"
	// bashCmd is the shape of a Bash tool command string
	// (docs/claude-code.md "CLAUDE_CODE_SHELL"), with every quoting hazard.
	bashCmd = `source /root/.claude/shell-snapshots/snapshot-bash-1.sh && shopt -u extglob 2>/dev/null || true && eval 'echo "a b" $HOME` + "`x`" + ` \; it'"'"'s' && pwd -P >| ` + testSess + `/tmp/claude-1a2b-cwd`
)

var testLocal = map[string]bool{"ps": true, "git": true}

// testShot is the clipboard screenshot file as Claude names it.
const (
	testShotRel = "tmp/claude-1000/" + screenshotName
	testShot    = testSess + "/" + testShotRel
)

func wrap(style string, inner string) string {
	q := quoteStyles[style]
	return q(testTeleExec) + " " + q(inner)
}

func TestClassify(t *testing.T) {
	tests := []struct {
		name    string
		argv    []string
		sessDir string
		want    Action
		wantErr error
	}{
		// bash: remote bash, -c script unwrapped when it is the tele-exec wrapper.
		{name: "bash", argv: []string{testSess + "/bin/bash", "-c", "-l", bashCmd},
			want: Action{Argv: []string{"bash", "-c", "-l", bashCmd}}},
		{name: "bash", argv: []string{"bash", "-c", "-l", wrap("single", bashCmd)},
			want: Action{Argv: []string{"bash", "-c", "-l", bashCmd}}},
		{name: "bash", argv: []string{"bash", "-c", "-l", wrap("double", bashCmd)},
			want: Action{Argv: []string{"bash", "-c", "-l", bashCmd}}},
		{name: "bash", argv: []string{"bash", "-c", "-l", wrap("backslash", bashCmd)},
			want: Action{Argv: []string{"bash", "-c", "-l", bashCmd}}},
		{name: "bash", argv: []string{"bash", "-lc", wrap("single", "ls"), "a0", "a1"},
			want: Action{Argv: []string{"bash", "-lc", "ls", "a0", "a1"}}},
		{name: "bash", argv: []string{"bash", "-o", "pipefail", "-e", "-c", wrap("single", "ls")},
			want: Action{Argv: []string{"bash", "-o", "pipefail", "-e", "-c", "ls"}}},
		{name: "bash", argv: []string{"bash", "-co", wrap("single", "opt"), wrap("single", "ls")},
			want: Action{Argv: []string{"bash", "-co", wrap("single", "opt"), "ls"}}},
		{name: "bash", argv: []string{"bash", "--rcfile", wrap("single", "rc"), "-c", wrap("single", "ls")},
			want: Action{Argv: []string{"bash", "--rcfile", wrap("single", "rc"), "-c", "ls"}}},
		{name: "bash", argv: []string{"bash", "-c", "--", wrap("single", "ls")},
			want: Action{Argv: []string{"bash", "-c", "--", "ls"}}},
		{name: "bash", sessDir: testSess + "/", argv: []string{"bash", "-c", wrap("single", "ls")},
			want: Action{Argv: []string{"bash", "-c", "ls"}}},
		{name: "bash", argv: []string{"bash", "-c", wrap("single", "")},
			want: Action{Argv: []string{"bash", "-c", ""}}},
		// An unwrapped script that looks like options gets "--" unless
		// the options already ended.
		{name: "bash", argv: []string{"bash", "-c", "-l", wrap("single", "-e echo")},
			want: Action{Argv: []string{"bash", "-c", "-l", "--", "-e echo"}}},
		{name: "bash", argv: []string{"bash", "-lc", wrap("double", "+x"), "a0"},
			want: Action{Argv: []string{"bash", "-lc", "--", "+x", "a0"}}},
		{name: "bash", argv: []string{"bash", "-co", "errexit", wrap("single", "-x")},
			want: Action{Argv: []string{"bash", "-co", "errexit", "--", "-x"}}},
		{name: "bash", argv: []string{"bash", "-c", "--", wrap("single", "-e echo")},
			want: Action{Argv: []string{"bash", "-c", "--", "-e echo"}}},
		{name: "bash", argv: []string{"bash", "-c", "-", wrap("single", "+x")},
			want: Action{Argv: []string{"bash", "-c", "-", "+x"}}},
		// Not the two-word form, or no -c script: forwarded unchanged.
		{name: "bash", argv: []string{"bash", "-c", wrap("single", "ls") + "; rm -rf ~"},
			want: Action{Argv: []string{"bash", "-c", wrap("single", "ls") + "; rm -rf ~"}}},
		{name: "bash", argv: []string{"bash", "-c", wrap("single", "ls") + " extra"},
			want: Action{Argv: []string{"bash", "-c", wrap("single", "ls") + " extra"}}},
		{name: "bash", argv: []string{"bash", "-c", "'/.tele/fedcba9876543210/bin/tele-exec' 'ls'"},
			want: Action{Argv: []string{"bash", "-c", "'/.tele/fedcba9876543210/bin/tele-exec' 'ls'"}}},
		{name: "bash", sessDir: "relative", argv: []string{"bash", "-c", "'relative/bin/tele-exec' 'ls'"},
			want: Action{Argv: []string{"bash", "-c", "'relative/bin/tele-exec' 'ls'"}}},
		{name: "bash", argv: []string{"bash", "-l", wrap("single", "ls")},
			want: Action{Argv: []string{"bash", "-l", wrap("single", "ls")}}},
		{name: "bash", argv: []string{"bash", "--bogus", "-c", wrap("single", "ls")},
			want: Action{Argv: []string{"bash", "--bogus", "-c", wrap("single", "ls")}}},
		{name: "bash", argv: []string{"bash", "-c"}, want: Action{Argv: []string{"bash", "-c"}}},
		{name: "bash", argv: []string{"bash"}, want: Action{Argv: []string{"bash"}}},
		{name: "bash", argv: []string{"bash", "script.sh", "-c"}, want: Action{Argv: []string{"bash", "script.sh", "-c"}}},

		// sh: only [sh -c <wrapper> ...] is unwrapped, into tele-exec.
		{name: "sh", argv: []string{"/bin/sh", "-c", wrap("single", bashCmd)},
			want: Action{Argv: []string{"sh", "-c", "--", bashCmd}}},
		{name: "sh", argv: []string{"sh", "-c", wrap("double", "hook.sh --x"), "a0", "a1"},
			want: Action{Argv: []string{"sh", "-c", "--", "hook.sh --x"}}},
		{name: "sh", argv: []string{"sh", "-c", wrap("single", "-e echo")},
			want: Action{Argv: []string{"sh", "-c", "--", "-e echo"}}},
		{name: "sh", argv: []string{"sh", "-c", "exec python3 -m server"},
			want: Action{Argv: []string{"sh", "-c", "exec python3 -m server"}}},
		{name: "sh", argv: []string{"sh", "-e", "-c", wrap("single", "ls")},
			want: Action{Argv: []string{"sh", "-e", "-c", wrap("single", "ls")}}},
		{name: "sh", argv: []string{"sh", "-c"}, want: Action{Argv: []string{"sh", "-c"}}},
		{name: "sh", argv: []string{"sh", "script.sh"}, want: Action{Argv: []string{"sh", "script.sh"}}},
		{name: "sh", argv: []string{"sh"}, want: Action{Argv: []string{"sh"}}},

		// tele-exec: one shell string for sh -c, extra arguments become $0...
		{name: "tele-exec", argv: []string{testTeleExec, "npx -y @modelcontextprotocol/server-memory"},
			want: Action{Argv: []string{"sh", "-c", "--", "npx -y @modelcontextprotocol/server-memory"}}},
		{name: "tele-exec", argv: []string{"tele-exec", "echo $0 $1", "a0", "a1"},
			want: Action{Argv: []string{"sh", "-c", "--", "echo $0 $1", "a0", "a1"}}},
		// A command that looks like options still runs as a command.
		{name: "tele-exec", argv: []string{"tele-exec", "-e echo"},
			want: Action{Argv: []string{"sh", "-c", "--", "-e echo"}}},
		{name: "tele-exec", argv: []string{"tele-exec", "+x"},
			want: Action{Argv: []string{"sh", "-c", "--", "+x"}}},
		{name: "tele-exec", argv: []string{"tele-exec"}, wantErr: ErrMissingCommand},

		// Forwarded programs.
		{name: "rg", argv: []string{testSess + "/bin/rg", "--files", "-g", "*.go"},
			want: Action{Argv: []string{"rg", "--files", "-g", "*.go"}}},
		{name: "git", argv: []string{"git", "status", "--porcelain"},
			want: Action{Argv: []string{"git", "status", "--porcelain"}}},
		{name: "uname", argv: []string{"uname", "-a"}, want: Action{Argv: []string{"uname", "-a"}}},

		// Local exec proxies.
		{name: "ps", argv: []string{testSess + "/bin/ps", "-o", "pid", "--ppid", "1"},
			want: Action{Local: true, Argv: []string{"ps", "-o", "pid", "--ppid", "1"}}},
		{name: "xdg-open", argv: []string{"xdg-open", "https://x"},
			want: Action{Local: true, Argv: []string{"xdg-open", "https://x"}}},
		{name: "xdg-open", argv: []string{testSess + "/bin/xdg-open", "http://localhost:4000/cb?a=1&b=%20"},
			want: Action{Local: true, Argv: []string{"xdg-open", "http://localhost:4000/cb?a=1&b=%20"}}},
		{name: "xclip", argv: []string{"xclip", "-selection", "clipboard"},
			want: Action{Local: true, Argv: []string{"xclip", "-selection", "clipboard"}}},
		{name: "xclip", argv: []string{"xclip", "-selection", "clipboard", "-o"},
			want: Action{Local: true, Argv: []string{"xclip", "-selection", "clipboard", "-o"}}},
		{name: "xsel", argv: []string{"xsel", "--primary", "--input"},
			want: Action{Local: true, Argv: []string{"xsel", "--primary", "--input"}}},
		{name: "wl-copy", argv: []string{"wl-copy"}, want: Action{Local: true, Argv: []string{"wl-copy"}}},
		{name: "wl-copy", argv: []string{"wl-copy", "--primary"},
			want: Action{Local: true, Argv: []string{"wl-copy", "--primary"}}},
		{name: "wl-paste", argv: []string{"wl-paste", "--no-newline"},
			want: Action{Local: true, Argv: []string{"wl-paste", "--no-newline"}}},

		// Desktop programs accept only the arguments Claude uses.
		{name: "xdg-open", argv: []string{"xdg-open", "file:///etc/passwd"}, wantErr: ErrRejected},
		{name: "xdg-open", argv: []string{"xdg-open", "/etc/passwd"}, wantErr: ErrRejected},
		{name: "xdg-open", argv: []string{"xdg-open", "--manual"}, wantErr: ErrRejected},
		{name: "xdg-open", argv: []string{"xdg-open", "https:///x"}, wantErr: ErrRejected},
		{name: "xdg-open", argv: []string{"xdg-open", "https://x/\n"}, wantErr: ErrRejected},
		{name: "xdg-open", argv: []string{"xdg-open", "https://x", "https://y"}, wantErr: ErrRejected},
		{name: "xdg-open", argv: []string{"xdg-open"}, wantErr: ErrRejected},
		{name: "xclip", argv: []string{"xclip", "-i", "/home/alice/.ssh/id_ed25519"}, wantErr: ErrRejected},
		{name: "wl-paste", argv: []string{"wl-paste", "--watch", "sh", "-c", "id"}, wantErr: ErrRejected},
		{name: "wl-copy", argv: []string{"wl-copy", "secret"}, wantErr: ErrRejected},

		// The clipboard image scripts run locally, naming the screenshot
		// file relative to the session directory.
		{name: "sh", argv: []string{"/bin/sh", "-c", clipboardScripts[0]},
			want: Action{Local: true, Argv: []string{"sh", "-c", clipboardScripts[0]}}},
		{name: "sh", argv: []string{"/bin/sh", "-c", fmt.Sprintf(clipboardScripts[1], testShot)},
			want: Action{Local: true, Argv: []string{"sh", "-c", fmt.Sprintf(clipboardScripts[1], testShotRel)}}},
		{name: "sh", argv: []string{"/bin/sh", "-c", fmt.Sprintf(clipboardScripts[2], testShot)},
			want: Action{Local: true, Argv: []string{"sh", "-c", "rm -f -- " + testShotRel}}},
		{name: "sh", sessDir: testSess + "/", argv: []string{"/bin/sh", "-c", fmt.Sprintf(clipboardScripts[2], testShot)},
			want: Action{Local: true, Argv: []string{"sh", "-c", "rm -f -- " + testShotRel}}},
		// Anything else stays remote: another file, another directory,
		// another command.
		{name: "sh", argv: []string{"/bin/sh", "-c", "rm -f -- " + testSess + "/tmp/claude-0/other.png"},
			want: Action{Argv: []string{"sh", "-c", "rm -f -- " + testSess + "/tmp/claude-0/other.png"}}},
		{name: "sh", argv: []string{"/bin/sh", "-c", "rm -f -- /tmp/claude-0/" + screenshotName},
			want: Action{Argv: []string{"sh", "-c", "rm -f -- /tmp/claude-0/" + screenshotName}}},
		{name: "sh", argv: []string{"/bin/sh", "-c", "rm -f -- " + testShot + "; rm -rf ~"},
			want: Action{Argv: []string{"sh", "-c", "rm -f -- " + testShot + "; rm -rf ~"}}},
		{name: "sh", argv: []string{"/bin/sh", "-c", "rm -f -- " + testShot + " " + testSess + "/tmp/claude-1/" + screenshotName},
			want: Action{Argv: []string{"sh", "-c", "rm -f -- " + testShot + " " + testSess + "/tmp/claude-1/" + screenshotName}}},
		{name: "sh", argv: []string{"/bin/sh", "-c", fmt.Sprintf(clipboardScripts[1], testShot), "a0"},
			want: Action{Argv: []string{"sh", "-c", fmt.Sprintf(clipboardScripts[1], testShot), "a0"}}},
		{name: "sh", sessDir: "relative", argv: []string{"/bin/sh", "-c", "rm -f -- relative/tmp/claude-0/" + screenshotName},
			want: Action{Argv: []string{"sh", "-c", "rm -f -- relative/tmp/claude-0/" + screenshotName}}},

		// The IDE detection script runs locally; nothing else like it does.
		{name: "sh", argv: []string{"/bin/sh", "-c", ideScript},
			want: Action{Local: true, Argv: []string{"sh", "-c", ideScript}}},
		{name: "sh", argv: []string{"/bin/sh", "-c", ideScript + "; id"},
			want: Action{Argv: []string{"sh", "-c", ideScript + "; id"}}},
		{name: "sh", argv: []string{"/bin/sh", "-c", ideScript, "a0"},
			want: Action{Argv: []string{"sh", "-c", ideScript, "a0"}}},

		// The editor gets one absolute path, which stays a path in the
		// remote view.
		{name: NameEditor, argv: []string{NameEditor, "/home/bob/proj/CLAUDE.md"},
			want: Action{Local: true, Edit: "/home/bob/proj/CLAUDE.md"}},
		{name: NameEditor, argv: []string{"/x/" + NameEditor, "/home/bob/proj/../.claude//CLAUDE.md"},
			want: Action{Local: true, Edit: "/home/bob/.claude/CLAUDE.md"}},
		{name: NameEditor, argv: []string{NameEditor, "+1", "/home/bob/CLAUDE.md"}, wantErr: ErrRejected},
		{name: NameEditor, argv: []string{NameEditor, "CLAUDE.md"}, wantErr: ErrRejected},
		{name: NameEditor, argv: []string{NameEditor, "/x\ny"}, wantErr: ErrRejected},
		{name: NameEditor, argv: []string{NameEditor}, wantErr: ErrRejected},

		// Errors.
		{name: "python3", argv: []string{"python3"}, wantErr: ErrUnknownProgram},
		{name: "tele", argv: []string{"tele", "doctor"}, wantErr: ErrUnknownProgram},
		{name: "", argv: []string{""}, wantErr: ErrUnknownProgram},
		{name: "bash", argv: nil, wantErr: ErrEmptyArgv},
	}
	for _, tt := range tests {
		sess := tt.sessDir
		if sess == "" {
			sess = testSess
		}
		argv := slices.Clone(tt.argv)
		got, err := Classify(tt.name, argv, sess, testLocal)
		if !errors.Is(err, tt.wantErr) {
			t.Errorf("Classify(%q, %q) error = %v, want %v", tt.name, tt.argv, err, tt.wantErr)
			continue
		}
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("Classify(%q, %q)\n got %#v\nwant %#v", tt.name, tt.argv, got, tt.want)
		}
		if !slices.Equal(argv, tt.argv) {
			t.Errorf("Classify(%q, %q) modified its argv to %q", tt.name, tt.argv, argv)
		}
		if len(got.Argv) > 1 && len(argv) > 1 && &got.Argv[1] == &argv[1] {
			t.Errorf("Classify(%q, %q) result aliases argv", tt.name, tt.argv)
		}
	}
}

var bashScriptTests = []struct {
	args []string
	want int
}{
	{[]string{"-c", "-l", "S"}, 2},
	{[]string{"-c", "S"}, 1},
	{[]string{"-lc", "S"}, 1},
	{[]string{"-cl", "S", "a0"}, 1},
	{[]string{"-l", "-c", "S"}, 2},
	{[]string{"-e", "-u", "-c", "S"}, 3},
	{[]string{"-o", "errexit", "-c", "S"}, 3},
	{[]string{"-co", "errexit", "S"}, 2},
	{[]string{"-oc", "errexit", "S"}, 2},
	{[]string{"-oo", "errexit", "nounset", "-c", "S"}, 4},
	{[]string{"+o", "posix", "-c", "S"}, 3},
	{[]string{"-O", "extglob", "-c", "S"}, 3},
	{[]string{"+O", "extglob", "-c", "S"}, 3},
	{[]string{"-c", "+e", "S"}, 2},
	{[]string{"+c", "S"}, 1},
	{[]string{"-c", "--", "S"}, 2},
	{[]string{"-c", "-", "S"}, 2},
	{[]string{"-c", "+", "S"}, 2},
	{[]string{"--norc", "-c", "S"}, 2},
	{[]string{"--noprofile", "--norc", "-c", "S"}, 3},
	{[]string{"-norc", "-c", "S"}, 2},
	{[]string{"--login", "-c", "S"}, 2},
	{[]string{"-login", "-c", "S"}, 2},
	{[]string{"--rcfile", "/dev/null", "-c", "S"}, 3},
	{[]string{"--init-file", "-c", "-c", "S"}, 3},
	{[]string{"--posix", "-lc", "S"}, 2},
	{[]string{"-c", "-x", "S"}, 2},
	{[]string{"-c", "--norc", "S"}, -1}, // word options only come first: -, n, o (takes S), r, c
	{[]string{"-c", "-o"}, -1},
	{[]string{"-c"}, -1},
	{[]string{"-l", "S"}, -1},
	{[]string{"S", "-c"}, -1},
	{[]string{"--", "-c", "S"}, -1},
	{[]string{"-", "-c", "S"}, -1},
	{[]string{"--bogus", "-c", "S"}, -1},
	{[]string{"---norc", "-c", "S"}, -1},
	{[]string{"--rcfile"}, -1},
	{nil, -1},
}

func TestBashScriptIndex(t *testing.T) {
	for _, tt := range bashScriptTests {
		if got, _ := bashScriptIndex(tt.args); got != tt.want {
			t.Errorf("bashScriptIndex(%q) = %d, want %d", tt.args, got, tt.want)
		}
	}
	for _, tt := range []struct {
		args  []string
		ended bool
	}{
		{[]string{"-c", "S"}, false},
		{[]string{"-c", "--", "S"}, true},
		{[]string{"-c", "-", "S"}, true},
		{[]string{"-lc", "--", "S", "a0"}, true},
		{[]string{"-c", "+", "S"}, false},   // "+" is an empty option cluster
		{[]string{"-co", "--", "S"}, false}, // "--" is the argument of -o
	} {
		if i, ended := bashScriptIndex(tt.args); i < 0 || tt.args[i] != "S" || ended != tt.ended {
			t.Errorf("bashScriptIndex(%q) = %d, %v; want the index of S, %v", tt.args, i, ended, tt.ended)
		}
	}
}

var plainWordsTests = []struct {
	script string
	want   []string // nil with ok == false
	ok     bool
}{
	{"", nil, true},
	{" \t ", nil, true},
	{"a", []string{"a"}, true},
	{" a\t b ", []string{"a", "b"}, true},
	{"'a b'", []string{"a b"}, true},
	{`'a'\''b'`, []string{"a'b"}, true},
	{`'a\b'`, []string{`a\b`}, true},
	{"'a\nb'", []string{"a\nb"}, true},
	{"'a\\\nb'", []string{"a\\\nb"}, true},
	{`"a b"`, []string{"a b"}, true},
	{`"a\"b"`, []string{`a"b`}, true},
	{`"a\\b"`, []string{`a\b`}, true},
	{`"a\$b"`, []string{`a$b`}, true},
	{"\"a\\`b\"", []string{"a`b"}, true},
	{`"a\b"`, []string{`a\b`}, true},
	{`"it's \!"`, []string{`it's \!`}, true}, // shell-quote's escaping of '!' is not an escape in sh
	{"\"a\nb\"", []string{"a\nb"}, true},
	{"\"a\\\nb\"", []string{"ab"}, true},
	{`a\ b`, []string{"a b"}, true},
	{`\'\"\\\$`, []string{`'"\$`}, true},
	{"a\\\nb", []string{"ab"}, true},
	{"a \\\n b", []string{"a", "b"}, true},
	{"\\\n", nil, true},
	{"a\\\n#b", []string{"a#b"}, true},
	{`''`, []string{""}, true},
	{`'' ""`, []string{"", ""}, true},
	{`a''b"c"\d`, []string{"abcd"}, true},
	{"a#b", []string{"a#b"}, true},
	{"''#", []string{"#"}, true},
	{"~/x a~", []string{"~/x", "a~"}, true},
	{"a]b}c=d!e%f^g,h:i@j", []string{"a]b}c=d!e%f^g,h:i@j"}, true},
	{"'*' \\? \"[a]\" '{a,b}'", []string{"*", "?", "[a]", "{a,b}"}, true},
	{"é\r\v", []string{"é\r\v"}, true},

	{"a #b", nil, false},
	{"#a", nil, false},
	{"a;b", nil, false},
	{"a|b", nil, false},
	{"a&b", nil, false},
	{"a>b", nil, false},
	{"a<b", nil, false},
	{"(a)", nil, false},
	{"a\nb", nil, false},
	{"$a", nil, false},
	{"a$", nil, false},
	{"`a`", nil, false},
	{`"$a"`, nil, false},
	{`"$"`, nil, false},
	{"\"`a`\"", nil, false},
	{"a*", nil, false},
	{"a?", nil, false},
	{"[a]", nil, false},
	{"{a,b}", nil, false},
	{"'unterminated", nil, false},
	{`"unterminated`, nil, false},
	{`"a\`, nil, false},
	{`trailing\`, nil, false},
	{"a\x00b", nil, false},
	{"'a\x00b'", nil, false},
}

func TestPlainWords(t *testing.T) {
	for _, tt := range plainWordsTests {
		got, ok := plainWords(tt.script)
		if ok != tt.ok || !slices.Equal(got, tt.want) {
			t.Errorf("plainWords(%q) = %q, %v; want %q, %v", tt.script, got, ok, tt.want, tt.ok)
		}
	}
}

func TestUnwrapScriptAllStyles(t *testing.T) {
	inners := []string{"", "ls", bashCmd, "a\nb", "'", `"`, `\`, "$(id)", "~/hook.sh", "  spaced  ", "\t", "é"}
	seps := []string{" ", "\t", "  \t ", " \\\n "}
	for _, inner := range inners {
		for s1, q1 := range quoteStyles {
			for s2, q2 := range quoteStyles {
				for _, sep := range seps {
					script := " " + q1(testTeleExec) + sep + q2(inner) + " "
					got, ok := unwrapScript(script, testTeleExec)
					if !ok || got != inner {
						t.Errorf("unwrapScript(%s+%s %q) = %q, %v; want %q", s1, s2, script, got, ok, inner)
					}
				}
			}
		}
	}
	if _, ok := unwrapScript(quoteSingle(testTeleExec), testTeleExec); ok {
		t.Error("unwrapScript accepted the wrapper without a command")
	}
	if _, ok := unwrapScript(quoteSingle(testTeleExec)+" a b", testTeleExec); ok {
		t.Error("unwrapScript accepted three words")
	}
	if _, ok := unwrapScript("x y", ""); ok {
		t.Error("unwrapScript accepted an empty tele-exec path")
	}
}

func TestFilterEnv(t *testing.T) {
	baseline := []string{
		"PATH=/.tele/s/bin", "HOME=/home/u", "TERM=xterm-256color", "LANG=C.UTF-8",
		"KEEP=same", "CHANGED=old", "TELE_SESSION=/.tele/s", "https_proxy=http://p",
		"LC_ALL=C", "DUP=1",
	}
	env := []string{
		"PATH=/.tele/s/bin:/extra", // never, even changed
		"HOME=/home/u",
		"TERM=xterm-256color", // always, even unchanged
		"LANG=C.UTF-8",
		"KEEP=same",   // unchanged: dropped
		"CHANGED=new", // changed: kept
		"NEW=1",       // new: kept
		"TELE_SESSION=/.tele/s", "TELE_OTHER=x",
		"HTTPS_PROXY=a", "https_proxy=b", "Http_Proxy=c", "NO_PROXY=d", "no_proxy=e", "ALL_PROXY=f", "all_proxy=g", "HTTP_PROXY=h",
		"SSL_CERT_FILE=x", "NODE_EXTRA_CA_CERTS=x", "LD_PRELOAD=x",
		"CLAUDE_CODE_SHELL=x", "CLAUDE_CODE_SHELL_PREFIX=x", "USE_BUILTIN_RIPGREP=0",
		"SSH_AUTH_SOCK=x", "DISPLAY=:0", "WAYLAND_DISPLAY=w", "XDG_RUNTIME_DIR=/run", "DBUS_SESSION_BUS_ADDRESS=x",
		"LC_ALL=C", "LC_CTYPE=C", "COLORTERM=truecolor", "LANGUAGE=en", "TZ=UTC", "NO_COLOR=1", "FORCE_COLOR=1",
		"DUP=2", "EMPTY=", "noequals", "=novalue",
		"DUP=1", // last duplicate wins: equal to baseline, so dropped
		"LATE=a", "LATE=b",
		"CLAUDE_CODE_ENTRYPOINT=cli",
	}
	want := []string{
		"TERM=xterm-256color", "LANG=C.UTF-8", "CHANGED=new", "NEW=1",
		"LC_ALL=C", "LC_CTYPE=C", "COLORTERM=truecolor", "LANGUAGE=en", "TZ=UTC", "NO_COLOR=1", "FORCE_COLOR=1",
		"EMPTY=", "LATE=b", "CLAUDE_CODE_ENTRYPOINT=cli",
	}
	if got := FilterEnv(env, baseline); !slices.Equal(got, want) {
		t.Errorf("FilterEnv:\n got %q\nwant %q", got, want)
	}
	if got := FilterEnv(nil, baseline); len(got) != 0 {
		t.Errorf("FilterEnv(nil) = %q", got)
	}
	if got := FilterEnv([]string{"A=1", "TERM=x"}, nil); !slices.Equal(got, []string{"A=1", "TERM=x"}) {
		t.Errorf("FilterEnv without baseline = %q", got)
	}
	// Baseline duplicates: the last one is the baseline value.
	if got := FilterEnv([]string{"A=2"}, []string{"A=1", "A=2"}); len(got) != 0 {
		t.Errorf("FilterEnv against duplicated baseline = %q", got)
	}
}

func FuzzQuoteRoundTrip(f *testing.F) {
	for _, s := range []string{"", "ls", bashCmd, "a\nb", "'\"\\$`", "\\\n", "~", "#", "é\x80\xff", "a\x00b"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, x string) {
		hasNUL := strings.IndexByte(x, 0) >= 0
		for style, q := range quoteStyles {
			words, ok := plainWords(q(x))
			if hasNUL {
				if ok {
					t.Fatalf("%s: plainWords(%q) accepted NUL", style, q(x))
				}
				continue
			}
			if !ok || len(words) != 1 || words[0] != x {
				t.Fatalf("%s: plainWords(%q) = %q, %v; want [%q]", style, q(x), words, ok, x)
			}
			for style2, q2 := range quoteStyles {
				inner, ok := unwrapScript(q2(testTeleExec)+" "+q(x), testTeleExec)
				if !ok || inner != x {
					t.Fatalf("%s+%s: unwrap = %q, %v; want %q", style2, style, inner, ok, x)
				}
			}
		}
	})
}

func FuzzPlainWords(f *testing.F) {
	for _, s := range []string{"a b", `'a'\''b' "c\"d" e\ f`, "a\\\nb", "#", "a;b", "$x", `"\`, "~ ~x"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		words, ok := plainWords(s)
		if !ok {
			return
		}
		// Whatever parses must re-parse to the same words once requoted.
		quoted := make([]string, len(words))
		for i, w := range words {
			quoted[i] = quoteSingle(w)
		}
		again, ok := plainWords(strings.Join(quoted, " "))
		if !ok || !slices.Equal(again, words) {
			t.Fatalf("plainWords(%q) = %q, but requoted gives %q, %v", s, words, again, ok)
		}
	})
}

func FuzzClassify(f *testing.F) {
	f.Add("bash", "-c\x00-l\x00"+wrap("single", "ls"))
	f.Add("sh", "-c\x00"+wrap("double", "ls")+"\x00a0")
	f.Add("tele-exec", "")
	f.Add("bash", "--rcfile")
	f.Add("ps", "-o\x00pid")
	f.Add("xdg-open", "https://x")
	f.Add("xclip", "-selection\x00clipboard")
	f.Add("sh", "-c\x00"+fmt.Sprintf(clipboardScripts[2], testShot))
	f.Add("sh", "-c\x00"+ideScript)
	f.Add(NameEditor, "/home/bob/CLAUDE.md")
	f.Add("x", "")
	f.Fuzz(func(t *testing.T, name, args string) {
		argv := []string{name}
		if args != "" {
			argv = append(argv, strings.Split(args, "\x00")...)
		}
		orig := slices.Clone(argv)
		act, err := Classify(name, argv, testSess, testLocal)
		if !slices.Equal(argv, orig) {
			t.Fatalf("Classify modified argv: %q -> %q", orig, argv)
		}
		if err != nil {
			return
		}
		if act.Edit != "" {
			if name != NameEditor || !act.Local || act.Argv != nil || len(orig) != 2 || !path.IsAbs(act.Edit) {
				t.Fatalf("Classify(%q, %q) = %+v", name, orig, act)
			}
			return
		}
		if len(act.Argv) == 0 {
			t.Fatalf("Classify(%q, %q) returned an empty Argv", name, orig)
		}
		switch {
		case act.Local && name == "sh":
			if _, ok := clipboardScript(orig[2], testSess); (!ok && orig[2] != ideScript) || len(orig) != 3 || orig[1] != "-c" {
				t.Fatalf("Classify(%q, %q) = local %q", name, orig, act.Argv)
			}
		case act.Local:
			desktop := slices.Contains(DesktopPrograms, name)
			if !testLocal[name] && !desktop || act.Argv[0] != name {
				t.Fatalf("Classify(%q, %q) = local %q", name, orig, act.Argv)
			}
			if desktop && name != NameXdgOpen && !slices.ContainsFunc(clipboardArgs[name], func(a []string) bool { return slices.Equal(a, act.Argv[1:]) }) {
				t.Fatalf("Classify(%q, %q) = local %q, arguments not allowed", name, orig, act.Argv)
			}
		case act.Argv[0] == "bash", act.Argv[0] == "sh":
		case remotePrograms[act.Argv[0]]:
			if act.Argv[0] != name {
				t.Fatalf("Classify(%q, %q) = %q", name, orig, act.Argv)
			}
		default:
			t.Fatalf("Classify(%q, %q) = unexpected program %q", name, orig, act.Argv)
		}
	})
}

func FuzzFilterEnv(f *testing.F) {
	f.Add("A=1\x00TERM=x\x00PATH=/x\x00A=2", "A=2\x00TERM=x")
	f.Fuzz(func(t *testing.T, envs, bases string) {
		env := strings.Split(envs, "\x00")
		got := FilterEnv(env, strings.Split(bases, "\x00"))
		seen := map[string]bool{}
		for _, kv := range got {
			k, _, ok := strings.Cut(kv, "=")
			if !ok || k == "" || seen[k] || isNeverForwarded(k) || !slices.Contains(env, kv) {
				t.Fatalf("FilterEnv(%q) returned bad entry %q in %q", env, kv, got)
			}
			seen[k] = true
		}
	})
}

func TestKeepRemotePath(t *testing.T) {
	const p = "/.tele/0123456789abcdef/bin"
	for _, tc := range []struct{ in, want string }{
		{"cat >> f << 'E'\nexport PATH=" + p + "\nE\n", "cat >> f << 'E'\nexport PATH=\"$PATH\"\nE\n"},
		{"export PATH=" + p, `export PATH="$PATH"`},
		{"export PATH=" + p + ":/usr/bin\n", "export PATH=" + p + ":/usr/bin\n"},
		{"echo export PATH=" + p + "\n", "echo export PATH=" + p + "\n"},
		{"no path here", "no path here"},
	} {
		if got := KeepRemotePath(tc.in, p); got != tc.want {
			t.Errorf("KeepRemotePath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if got := KeepRemotePath("export PATH=", ""); got != "export PATH=" {
		t.Errorf("empty Claude PATH changed the argument: %q", got)
	}
}
