// Package claudever tells whether a Claude Code version has passed the
// Claude compatibility tests (docs/claude-code.md). tele runs any version
// but warns about one that is not listed.
package claudever

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"slices"
	"time"
)

// Verified lists the Claude Code versions the compatibility tests
// (internal/claudecompat) passed with. Add a version only after running
// them against it with TELE_TEST_CLAUDE; TestVersionVerified there fails
// until the tested version is listed.
var Verified = []string{
	"2.1.284",
	"2.1.286",
}

// versionTimeout bounds claude --version.
const versionTimeout = 30 * time.Second

// ErrNoVersion is returned when claude --version prints no version.
var ErrNoVersion = errors.New("claudever: no version in claude --version output")

var versionRE = regexp.MustCompile(`^(\d+\.\d+\.\d+)\b`)

// Parse extracts the version from claude --version output, such as
// "2.1.284 (Claude Code)".
func Parse(out string) (string, error) {
	m := versionRE.FindStringSubmatch(out)
	if m == nil {
		return "", ErrNoVersion
	}
	return m[1], nil
}

// Version runs claude --version.
func Version(ctx context.Context, claude string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, versionTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, claude, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("claudever: %s --version: %w", claude, err)
	}
	return Parse(string(out))
}

// IsVerified reports whether version is in Verified.
func IsVerified(version string) bool {
	return slices.Contains(Verified, version)
}

// Warning returns the warning to show for version, or "" for a verified
// one.
func Warning(version string) string {
	if IsVerified(version) {
		return ""
	}
	return fmt.Sprintf("Claude Code %s has not been verified with tele (verified: %v); "+
		"it may rely on behavior tele does not handle yet. It runs anyway; "+
		"report problems with the version", version, Verified)
}
