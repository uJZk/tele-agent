package dispatch

import "strings"

// plainWords splits script into words the way sh and bash tokenize and
// quote-remove it, provided the script is nothing but words separated by
// blanks. It reports false when the shell would do anything else with it:
// operators and redirections, parameter, command and arithmetic expansion,
// pathname and brace expansion, comments, unterminated quotes, a trailing
// backslash (on which shells disagree), or NUL (which cannot occur in a real
// argv; shells would truncate the script there).
//
// Claude quotes the tele-exec wrapper with single quotes, double quotes or
// backslashes (docs/claude-code.md "CLAUDE_CODE_SHELL_PREFIX"), so all three
// follow POSIX exactly: inside double quotes a backslash only escapes $ ` "
// \ and newline, and backslash-newline is a line continuation everywhere
// except inside single quotes.
//
// An unquoted '~' is kept literally. A tilde prefix in the command word
// would be expanded from HOME by the local shell before tele-exec saw it;
// keeping it lets the target host's sh expand it instead, from the same
// HOME, since Claude's HOME is the target user's home
// (docs/claude-code.md "注入的环境").
func plainWords(script string) ([]string, bool) {
	if strings.IndexByte(script, 0) >= 0 {
		return nil, false
	}
	var (
		words  []string
		cur    []byte
		inWord bool // cur holds a word, possibly empty ('')
	)
	for i := 0; i < len(script); {
		c := script[i]
		switch {
		case c == ' ' || c == '\t':
			if inWord {
				words = append(words, string(cur))
				cur, inWord = cur[:0], false
			}
			i++
		case c == '\\':
			if i+1 == len(script) {
				return nil, false
			}
			if script[i+1] != '\n' { // backslash-newline vanishes
				cur, inWord = append(cur, script[i+1]), true
			}
			i += 2
		case c == '\'':
			end := strings.IndexByte(script[i+1:], '\'')
			if end < 0 {
				return nil, false
			}
			cur, inWord = append(cur, script[i+1:i+1+end]...), true
			i += end + 2
		case c == '"':
			n, ok := appendDoubleQuoted(&cur, script[i+1:])
			if !ok {
				return nil, false
			}
			inWord = true
			i += n + 1
		case isShellSpecial(c), c == '#' && !inWord:
			return nil, false
		default:
			cur, inWord = append(cur, c), true
			i++
		}
	}
	if inWord {
		words = append(words, string(cur))
	}
	return words, true
}

// appendDoubleQuoted appends the quote-removed contents of a double-quoted
// string to cur. s starts right after the opening quote; n counts the bytes
// consumed including the closing quote. It reports false for an expansion
// or a missing closing quote.
func appendDoubleQuoted(cur *[]byte, s string) (n int, ok bool) {
	for i := 0; i < len(s); {
		switch c := s[i]; c {
		case '"':
			return i + 1, true
		case '$', '`':
			return 0, false
		case '\\':
			if i+1 == len(s) {
				return 0, false
			}
			switch e := s[i+1]; e {
			case '$', '`', '"', '\\':
				*cur = append(*cur, e)
				i += 2
			case '\n':
				i += 2
			default:
				*cur = append(*cur, '\\')
				i++
			}
		default:
			*cur = append(*cur, c)
			i++
		}
	}
	return 0, false
}

// isShellSpecial reports whether an unquoted c makes the shell do more than
// collect it into a word.
func isShellSpecial(c byte) bool {
	switch c {
	case ';', '&', '|', '<', '>', '(', ')', '\n', // operators
		'$', '`', // expansions
		'*', '?', '[', // pathname expansion
		'{': // bash brace expansion
		return true
	}
	return false
}
