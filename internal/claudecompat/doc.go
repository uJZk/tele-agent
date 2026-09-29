// Package claudecompat holds the Claude compatibility tests: each test
// pins down one behavior of Claude Code that tele relies on and that
// docs/claude-code.md records, by running the real claude against a
// scripted API (internal/testutil/claudetest). They run only when
// TELE_TEST_CLAUDE names a claude executable.
//
// A failing test means the contract changed: update docs/claude-code.md
// first, then the code that depends on it.
package claudecompat
