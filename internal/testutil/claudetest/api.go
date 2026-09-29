// Package claudetest drives the real Claude Code CLI against a scripted,
// local stand-in for the Anthropic API, for the Claude compatibility tests
// (docs/coding-standards.md "测试", docs/claude-code.md "验证方法").
//
// The tests run only when TELE_TEST_CLAUDE names a claude executable; see
// Require. The API stand-in answers every Messages request: requests that
// offer tools are agent turns and take the next scripted Turn, anything
// else (titles, classifiers) gets a short text reply. Every request is
// recorded so that tests can check what Claude sent, such as its system
// prompt or a tool result.
package claudetest

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ujzk/tele-agent/internal/testutil/certtest"
)

// Turn is one scripted assistant reply to an agent request.
type Turn struct {
	// Text is sent as a text block; empty means none.
	Text string
	// Tool, if set, is sent as a tool_use block after Text.
	Tool *ToolUse
}

// ToolUse asks Claude to run tool Name with Input.
type ToolUse struct {
	Name  string
	Input map[string]any
}

// Say returns a turn that answers with text and ends the conversation.
func Say(text string) Turn { return Turn{Text: text} }

// Use returns a turn that calls a tool.
func Use(name string, input map[string]any) Turn {
	return Turn{Tool: &ToolUse{Name: name, Input: input}}
}

// Request is a recorded Messages request.
type Request struct {
	// Agent is set for requests that offered tools.
	Agent    bool
	System   string // all system prompt text, joined by newlines
	Tools    []string
	Messages []Message
	Raw      json.RawMessage
}

// Message is one message of a request.
type Message struct {
	Role    string
	Content []Block
}

// Block is a content block of a message. Only the fields tests look at
// are decoded.
type Block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	// Content of a tool_result: a string or an array of text blocks.
	Content json.RawMessage `json:"content,omitempty"`
}

// ResultText returns the text of a tool_result block.
func (b Block) ResultText() string { return flattenText(b.Content) }

// API is the stand-in for the Anthropic API.
type API struct {
	t   testing.TB
	srv *httptest.Server

	mu       sync.Mutex // guards the fields below
	script   []Turn
	next     int
	requests []Request
	toolID   int

	pages []string // host and path of every other request

	host   string // set for a TLS API: the name in URL
	caFile string // set for a TLS API: PEM file of its certificate
}

// NewAPI starts an API that answers agent requests with script, in order.
// When the script runs out it answers "done" and fails the test.
func NewAPI(t testing.TB, script ...Turn) *API {
	t.Helper()
	a := &API{t: t, script: script}
	a.srv = httptest.NewServer(http.HandlerFunc(a.serve))
	t.Cleanup(a.srv.Close)
	return a
}

// NewTLSAPI is NewAPI serving HTTPS under the name host, with a
// certificate of its own that only CAFile makes trusted. host need not
// resolve: reach it through a Proxy.
func NewTLSAPI(t testing.TB, host string, script ...Turn) *API {
	t.Helper()
	a := &API{t: t, script: script, host: host}
	cert, caPEM := certtest.SelfSigned(t, siblings(host)...)
	a.caFile = filepath.Join(t.TempDir(), "api-ca.pem")
	if err := os.WriteFile(a.caFile, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	a.srv = httptest.NewUnstartedServer(http.HandlerFunc(a.serve))
	// Handshakes that fail on purpose (untrusted CA, a name the
	// certificate does not cover) are not worth a log line.
	a.srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	a.srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	a.srv.StartTLS()
	t.Cleanup(a.srv.Close)
	return a
}

// URL is the base URL to pass as ANTHROPIC_BASE_URL.
func (a *API) URL() string {
	if a.host != "" {
		return "https://" + a.host
	}
	return a.srv.URL
}

// CAFile is the PEM file that makes a TLS API's certificate trusted.
func (a *API) CAFile() string { return a.caFile }

// TrustEnv returns the variables that make Claude trust a TLS API's
// certificate, as tele points them at its CA bundle.
func (a *API) TrustEnv() []string {
	return []string{"SSL_CERT_FILE=" + a.caFile, "NODE_EXTRA_CA_CERTS=" + a.caFile}
}

// Addr is the address the API listens on.
func (a *API) Addr() string { return a.srv.Listener.Addr().String() }

// Requests returns the requests received so far.
func (a *API) Requests() []Request {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]Request(nil), a.requests...)
}

// Pages returns host and path of every request that was not a Messages
// request, such as a page fetched by WebFetch.
func (a *API) Pages() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.pages...)
}

// AgentRequests returns the agent requests received so far.
func (a *API) AgentRequests() []Request {
	var out []Request
	for _, r := range a.Requests() {
		if r.Agent {
			out = append(out, r)
		}
	}
	return out
}

// ToolResult returns the result Claude sent for the i-th scripted tool
// call (0-based), and whether it arrived.
func (a *API) ToolResult(i int) (Block, bool) {
	id := toolUseID(i + 1)
	for _, r := range a.AgentRequests() {
		for _, m := range r.Messages {
			for _, b := range m.Content {
				if b.Type == "tool_result" && b.ToolUseID == id {
					return b, true
				}
			}
		}
	}
	return Block{}, false
}

func toolUseID(n int) string { return fmt.Sprintf("toolu_tele%04d", n) }

// wireRequest is the part of a Messages request that is decoded.
type wireRequest struct {
	Model    string                  `json:"model"`
	Stream   bool                    `json:"stream"`
	System   json.RawMessage         `json:"system"`
	Tools    []struct{ Name string } `json:"tools"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
}

func (a *API) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || !strings.HasPrefix(r.URL.Path, "/v1/messages") {
		// Connectivity checks, and pages for WebFetch.
		a.mu.Lock()
		a.pages = append(a.pages, r.Host+r.URL.Path)
		a.mu.Unlock()
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<html><body><p>tele page</p></body></html>")
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if strings.HasSuffix(r.URL.Path, "/count_tokens") {
		writeJSON(w, map[string]any{"input_tokens": 1})
		return
	}
	var wr wireRequest
	if err := json.Unmarshal(body, &wr); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	req := Request{Agent: len(wr.Tools) > 0, System: flattenText(wr.System), Raw: body}
	for _, tl := range wr.Tools {
		req.Tools = append(req.Tools, tl.Name)
	}
	for _, m := range wr.Messages {
		req.Messages = append(req.Messages, Message{Role: m.Role, Content: blocks(m.Content)})
	}
	turn, toolID := a.record(req)
	msg := message(wr.Model, turn, toolID)
	if !wr.Stream {
		writeJSON(w, msg)
		return
	}
	stream(w, msg)
}

// record stores req and picks the reply, with the ID of its tool call.
func (a *API) record(req Request) (Turn, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.requests = append(a.requests, req)
	if !req.Agent {
		return Say("ok"), ""
	}
	if a.next == len(a.script) {
		a.t.Errorf("claudetest: agent request %d beyond the script of %d turns", a.next+1, len(a.script))
		return Say("done"), ""
	}
	turn := a.script[a.next]
	a.next++
	if turn.Tool == nil {
		return turn, ""
	}
	a.toolID++
	return turn, toolUseID(a.toolID)
}

// wireMessage is an assistant message of the Messages API.
type wireMessage struct {
	ID           string      `json:"id"`
	Type         string      `json:"type"`
	Role         string      `json:"role"`
	Model        string      `json:"model"`
	Content      []wireBlock `json:"content"`
	StopReason   *string     `json:"stop_reason"`
	StopSequence *string     `json:"stop_sequence"`
	Usage        wireUsage   `json:"usage"`
}

type wireBlock struct {
	Type  string          `json:"type"`
	Text  *string         `json:"text,omitempty"`
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

type wireUsage struct {
	InputTokens  int `json:"input_tokens,omitempty"`
	OutputTokens int `json:"output_tokens"`
}

// message builds the assistant message for turn; toolID names its tool
// call.
func message(model string, turn Turn, toolID string) wireMessage {
	msg := wireMessage{ID: "msg_tele", Type: "message", Role: "assistant", Model: model,
		Content: []wireBlock{}, Usage: wireUsage{InputTokens: 1, OutputTokens: 1}}
	stop := "end_turn"
	if turn.Text != "" {
		msg.Content = append(msg.Content, wireBlock{Type: "text", Text: &turn.Text})
	}
	if turn.Tool != nil {
		input, _ := json.Marshal(turn.Tool.Input) // maps of plain values
		if turn.Tool.Input == nil {
			input = []byte("{}")
		}
		msg.Content = append(msg.Content, wireBlock{Type: "tool_use", ID: toolID, Name: turn.Tool.Name, Input: input})
		stop = "tool_use"
	}
	msg.StopReason = &stop
	return msg
}

// stream writes msg as a Messages API event stream.
func stream(w http.ResponseWriter, msg wireMessage) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	event := func(name string, data any) {
		b, _ := json.Marshal(data) // plain structs and maps
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, b)
	}
	start := msg
	start.Content, start.StopReason = []wireBlock{}, nil
	event("message_start", map[string]any{"type": "message_start", "message": start})
	for i, c := range msg.Content {
		// Blocks start empty and arrive as one delta.
		switch c.Type {
		case "text":
			empty := ""
			event("content_block_start", map[string]any{"type": "content_block_start", "index": i,
				"content_block": wireBlock{Type: "text", Text: &empty}})
			event("content_block_delta", map[string]any{"type": "content_block_delta", "index": i,
				"delta": map[string]any{"type": "text_delta", "text": *c.Text}})
		case "tool_use":
			event("content_block_start", map[string]any{"type": "content_block_start", "index": i,
				"content_block": wireBlock{Type: "tool_use", ID: c.ID, Name: c.Name, Input: json.RawMessage("{}")}})
			event("content_block_delta", map[string]any{"type": "content_block_delta", "index": i,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": string(c.Input)}})
		}
		event("content_block_stop", map[string]any{"type": "content_block_stop", "index": i})
	}
	event("message_delta", map[string]any{"type": "message_delta",
		"delta": map[string]any{"stop_reason": msg.StopReason, "stop_sequence": nil},
		"usage": wireUsage{OutputTokens: 1}})
	event("message_stop", map[string]any{"type": "message_stop"})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v) // the client going away is not the test's concern
}

// flattenText returns content given as a string or as text blocks, such
// as a system prompt or a tool result, as one string.
func flattenText(raw json.RawMessage) string {
	var str string
	if json.Unmarshal(raw, &str) == nil {
		return str
	}
	var parts []Block
	_ = json.Unmarshal(raw, &parts) // absent or malformed: no text
	var out []string
	for _, p := range parts {
		out = append(out, p.Text)
	}
	return strings.Join(out, "\n")
}

// blocks decodes message content given as a string or blocks.
func blocks(raw json.RawMessage) []Block {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []Block{{Type: "text", Text: s}}
	}
	var out []Block
	_ = json.Unmarshal(raw, &out) // malformed content: no blocks
	return out
}

// siblings returns host and a wildcard for its parent domain, so that one
// certificate also covers other names there, such as a page for WebFetch.
func siblings(host string) []string {
	if _, parent, ok := strings.Cut(host, "."); ok && strings.Contains(parent, ".") {
		return []string{host, "*." + parent}
	}
	return []string{host}
}
