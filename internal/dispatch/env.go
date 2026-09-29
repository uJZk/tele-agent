package dispatch

import "strings"

// neverForward lists variables that only make sense for the local Claude
// process or point at local resources (docs/exec.md "环境变量"). TELE_* and
// the proxy variables are matched in isNeverForwarded.
var neverForward = map[string]bool{
	"PATH":                     true,
	"HOME":                     true,
	"LD_PRELOAD":               true,
	"SSL_CERT_FILE":            true,
	"SSL_CERT_DIR":             true,
	"NODE_EXTRA_CA_CERTS":      true,
	"CLAUDE_CODE_SHELL":        true,
	"CLAUDE_CODE_SHELL_PREFIX": true,
	"USE_BUILTIN_RIPGREP":      true,
	"SSH_AUTH_SOCK":            true,
	"DISPLAY":                  true,
	"WAYLAND_DISPLAY":          true,
	"XDG_RUNTIME_DIR":          true,
	"DBUS_SESSION_BUS_ADDRESS": true,
}

// alwaysForward lists variables describing the terminal and locale, which
// the target host needs even when Claude passed tele's value unchanged. LC_*
// is matched in isAlwaysForwarded.
var alwaysForward = map[string]bool{
	"TERM":        true,
	"COLORTERM":   true,
	"LANG":        true,
	"LANGUAGE":    true,
	"TZ":          true,
	"NO_COLOR":    true,
	"FORCE_COLOR": true,
}

func isNeverForwarded(key string) bool {
	if neverForward[key] || strings.HasPrefix(key, "TELE_") {
		return true
	}
	// Clients honour both cases of the proxy variables.
	for _, p := range [...]string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY"} {
		if strings.EqualFold(key, p) {
			return true
		}
	}
	return false
}

func isAlwaysForwarded(key string) bool {
	return alwaysForward[key] || strings.HasPrefix(key, "LC_")
}

// FilterEnv selects the KEY=VALUE entries of a shim's environment env that
// are forwarded to the target host: the terminal and locale variables when
// present, and otherwise only what is new or changed relative to baseline,
// the environment tele gave Claude. Variables that point at local resources
// are never forwarded. Of duplicate keys the last entry wins, as in os/exec;
// the result keeps env's order and has one entry per key. Entries without
// '=' or with an empty key are dropped.
func FilterEnv(env, baseline []string) []string {
	base := make(map[string]string, len(baseline))
	for _, kv := range baseline {
		if k, v, ok := strings.Cut(kv, "="); ok && k != "" {
			base[k] = v
		}
	}
	last := make(map[string]int, len(env))
	for i, kv := range env {
		if k, _, ok := strings.Cut(kv, "="); ok && k != "" {
			last[k] = i
		}
	}
	var out []string
	for i, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" || last[k] != i || isNeverForwarded(k) {
			continue
		}
		if bv, inBase := base[k]; isAlwaysForwarded(k) || !inBase || bv != v {
			out = append(out, kv)
		}
	}
	return out
}

// KeepRemotePath rewrites the PATH line of Claude's shell snapshot in s, a
// command's argument: Claude writes its own PATH into every snapshot as
// "export PATH=<value>" on a line of its own (docs/claude-code.md
// "CLAUDE_CODE_SHELL"), which under tele is the shim directory, absent on
// the target. The line becomes one that keeps the PATH the command
// already has, the target's login PATH. claudePath is PATH as tele gave it
// to Claude; it holds no characters Claude would quote.
func KeepRemotePath(s, claudePath string) string {
	if claudePath == "" {
		return s
	}
	line := "export PATH=" + claudePath
	var b strings.Builder
	for {
		i := strings.Index(s, line)
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		end := i + len(line)
		// Only a whole line: the value must not continue.
		whole := (i == 0 || s[i-1] == '\n') && (end == len(s) || s[end] == '\n')
		b.WriteString(s[:i])
		if whole {
			b.WriteString(`export PATH="$PATH"`)
		} else {
			b.WriteString(line)
		}
		s = s[end:]
	}
}
