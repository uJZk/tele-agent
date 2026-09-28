package dispatch

import (
	"strings"
)

// The quoting styles Claude may use when it wraps a command in
// CLAUDE_CODE_SHELL_PREFIX (docs/claude-code.md "CLAUDE_CODE_SHELL_PREFIX").
var quoteStyles = map[string]func(string) string{
	"single":    quoteSingle,
	"double":    quoteDouble,
	"backslash": quoteBackslash,
}

func quoteSingle(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func quoteDouble(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := range len(s) {
		switch c := s[i]; c {
		case '$', '`', '"', '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// quoteBackslash escapes every byte that could be special. A newline cannot
// be escaped with a backslash (that is a line continuation), so it is
// single-quoted instead; an empty word needs quotes of some kind.
func quoteBackslash(s string) string {
	if s == "" {
		return "''"
	}
	var b strings.Builder
	for i := range len(s) {
		c := s[i]
		switch {
		case c == '\n':
			b.WriteString("'\n'")
		case c >= 0x80, c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '/', c == '.', c == '_', c == '-':
			b.WriteByte(c)
		default:
			b.WriteByte('\\')
			b.WriteByte(c)
		}
	}
	return b.String()
}
