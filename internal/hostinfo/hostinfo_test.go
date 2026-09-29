package hostinfo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseOSRelease(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"NAME=Ubuntu\nPRETTY_NAME=\"Ubuntu 24.04.4 LTS\"\n", "Ubuntu 24.04.4 LTS"},
		{"PRETTY_NAME='Arch Linux'\n", "Arch Linux"},
		{"PRETTY_NAME=Plain\n", "Plain"},
		{"PRETTY_NAME=\"Esc \\\"quoted\\\"\"\n", `Esc "quoted"`},
		{"NAME=x\n", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := ParseOSRelease([]byte(tt.in)); got != tt.want {
			t.Errorf("ParseOSRelease(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestParsePasswdShell(t *testing.T) {
	passwd := "root:x:0:0:root:/root:/bin/bash\nbob:x:1000:1000::/home/bob:/usr/bin/zsh\nbad:line\n"
	tests := []struct {
		uid  uint32
		want string
	}{
		{0, "/bin/bash"},
		{1000, "/usr/bin/zsh"},
		{42, ""},
	}
	for _, tt := range tests {
		if got := ParsePasswdShell([]byte(passwd), tt.uid); got != tt.want {
			t.Errorf("ParsePasswdShell(uid %d) = %q, want %q", tt.uid, got, tt.want)
		}
	}
}

func TestParseLoginPath(t *testing.T) {
	out := "Welcome banner\n\n" + pathMarker + "/a:/b\nmore noise\n"
	if got := ParseLoginPath([]byte(out)); got != "/a:/b" {
		t.Fatalf("ParseLoginPath = %q", got)
	}
	if got := ParseLoginPath([]byte("no marker\n")); got != "" {
		t.Fatalf("ParseLoginPath without marker = %q", got)
	}
}

func TestLoginPathNoisyProfile(t *testing.T) {
	// A fake login shell whose "profile" prints noise and sets PATH.
	shell := filepath.Join(t.TempDir(), "fakesh")
	script := "#!/bin/sh\necho 'motd noise'\nPATH=/opt/x/bin:/usr/bin\nexport PATH\nshift 2\nexec /bin/sh -c \"$1\"\n"
	if err := os.WriteFile(shell, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := LoginPath(t.Context(), shell); got != "/opt/x/bin:/usr/bin" {
		t.Fatalf("LoginPath = %q", got)
	}
}

func TestLoginPathFallback(t *testing.T) {
	if got := LoginPath(t.Context(), "/nonexistent/shell"); got != FallbackPath {
		t.Fatalf("LoginPath(missing shell) = %q, want fallback", got)
	}
}

func TestGather(t *testing.T) {
	ti, err := Gather(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if ti.User == "" || !filepath.IsAbs(ti.Home) || ti.Hostname == "" || ti.Arch == "" ||
		!strings.HasPrefix(ti.Kernel, "Linux ") || ti.Shell == "" || ti.LoginPath == "" {
		t.Fatalf("incomplete TargetInfo: %+v", ti)
	}
	if ti.UID != uint32(os.Getuid()) {
		t.Fatalf("UID = %d, want %d", ti.UID, os.Getuid())
	}
}
