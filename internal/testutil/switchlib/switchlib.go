// Package switchlib builds the teleswitch preload library for tests, with
// the compiler flags of the Makefile, so that tests exercise the library
// as tele ships it without depending on a prior make run.
package switchlib

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ujzk/tele-agent/internal/testutil/privtest"
)

// Root returns the repository root: the directory holding go.mod above
// the test's working directory.
func Root(t testing.TB) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("switchlib: no go.mod above the working directory")
		}
		dir = parent
	}
}

// cflags reads TELESWITCH_CFLAGS from the Makefile, joining continued lines.
func cflags(t testing.TB, root string) []string {
	t.Helper()
	f, err := os.Open(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	var val string
	inside := false
	for sc.Scan() {
		line := sc.Text()
		if !inside {
			name, rest, ok := strings.Cut(line, ":=")
			if !ok || strings.TrimSpace(name) != "TELESWITCH_CFLAGS" {
				continue
			}
			line, inside = rest, true
		}
		cont := strings.HasSuffix(line, `\`)
		val += " " + strings.TrimSuffix(line, `\`)
		if !cont {
			break
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(val)
	if len(fields) == 0 {
		t.Fatal("switchlib: no TELESWITCH_CFLAGS in the Makefile")
	}
	return fields
}

// Build compiles the library for the host into a temporary directory and
// returns its path. It skips t when no C compiler is installed, or fails
// it when privtest.RequireEnv demands every privileged test to run.
func Build(t testing.TB) string {
	t.Helper()
	root := Root(t)
	cc := os.Getenv("CC")
	if cc == "" {
		cc = "cc"
	}
	if _, err := exec.LookPath(cc); err != nil {
		if os.Getenv(privtest.RequireEnv) == "1" {
			t.Fatalf("no C compiler to build teleswitch: %v", err)
		}
		t.Skipf("no C compiler to build teleswitch: %v", err)
	}
	out := filepath.Join(t.TempDir(), "teleswitch.so")
	args := append(cflags(t, root), "-o", out, filepath.Join(root, "internal", "teleswitch", "csrc", "teleswitch.c"))
	b, err := exec.CommandContext(context.Background(), cc, args...).CombinedOutput() //nolint:gosec // CC is the developer's compiler
	var ee *exec.ExitError
	switch {
	case errors.As(err, &ee):
		t.Fatalf("build teleswitch: %v\n%s", err, b)
	case err != nil:
		t.Fatalf("build teleswitch: %v", err)
	}
	return out
}
