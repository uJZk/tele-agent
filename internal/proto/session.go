package proto

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// StreamKind is the first frame (StreamHeader) of every multiplexed stream
// between session main and tele server.
type StreamKind uint8

// Stream kinds.
const (
	// StreamControl carries Hello/HelloReply, then session-level control.
	StreamControl StreamKind = 1
	// StreamExec carries one remote command: ExecStart, then ExecFrames.
	StreamExec StreamKind = 2
	// StreamFS carries exactly one FSRequest and one FSResponse.
	StreamFS StreamKind = 3
	// StreamWatch carries WatchEvents from the server for the whole session.
	StreamWatch StreamKind = 4
	// StreamForward carries one forwarded TCP connection: ForwardOpen,
	// ForwardReply, then the connection's bytes in both directions
	// (docs/exec.md "端口转发").
	StreamForward StreamKind = 5
)

// Valid reports whether k is a known stream kind.
func (k StreamKind) Valid() bool {
	return k >= StreamControl && k <= StreamForward
}

func (k StreamKind) String() string {
	switch k {
	case StreamControl:
		return "control"
	case StreamExec:
		return "exec"
	case StreamFS:
		return "fs"
	case StreamWatch:
		return "watch"
	case StreamForward:
		return "forward"
	default:
		return fmt.Sprintf("StreamKind(%d)", uint8(k))
	}
}

// StreamHeader opens every multiplexed stream.
type StreamHeader struct {
	Kind StreamKind `cbor:"1,keyasint"`
}

// Hello is the first message of a session, sent by session main on the
// control stream.
type Hello struct {
	Version int `cbor:"1,keyasint"`
	// Token authenticates the session to the server.
	Token []byte `cbor:"2,keyasint,omitempty"`
	// SessionID is chosen by session main; see CheckSessionID.
	SessionID string `cbor:"3,keyasint"`
	// JoinKey, when set, adds this connection to the existing session
	// SessionID instead of starting one: it must equal the JoinKey of that
	// session's HelloReply (docs/transport.md "可恢复会话层").
	JoinKey []byte `cbor:"4,keyasint,omitempty"`
}

// JoinKeyLen is the length of a HelloReply's JoinKey.
const JoinKeyLen = 32

// ForwardOpen asks the server to connect a forwarded stream to a loopback
// TCP port on the target host.
type ForwardOpen struct {
	Port uint16 `cbor:"1,keyasint"`
}

// ForwardReply answers ForwardOpen. When Err is set the server closes the
// stream; otherwise the stream carries the connection's bytes.
type ForwardReply struct {
	Err *Error `cbor:"1,keyasint,omitempty"`
}

// HelloReply answers Hello. When Err is set the server closes the session.
type HelloReply struct {
	Version int        `cbor:"1,keyasint"`
	Err     *Error     `cbor:"2,keyasint,omitempty"`
	Target  TargetInfo `cbor:"3,keyasint"`
	// ScratchDir is the server-side directory that holds this session's
	// scratch areas (docs/exec.md "scratch 路径改写与回传").
	ScratchDir string `cbor:"4,keyasint"`
	// ServerTime is the server's clock when it sent the reply, in Unix
	// milliseconds; it lets the client report clock skew.
	ServerTime int64 `cbor:"5,keyasint,omitempty"`
	// JoinKey lets further connections join this session (Hello.JoinKey).
	// The reply to a join carries neither it nor Target.
	JoinKey []byte `cbor:"6,keyasint,omitempty"`
}

// TargetInfo describes the target host and user. It feeds the appended
// system prompt, HOME inside the remote view, and command lookup.
type TargetInfo struct {
	Hostname     string `cbor:"1,keyasint"`
	OSPrettyName string `cbor:"2,keyasint"`
	Kernel       string `cbor:"3,keyasint"`
	Arch         string `cbor:"4,keyasint"`
	User         string `cbor:"5,keyasint"`
	UID          uint32 `cbor:"6,keyasint"`
	GID          uint32 `cbor:"7,keyasint"`
	Home         string `cbor:"8,keyasint"`
	Shell        string `cbor:"9,keyasint"`
	// LoginPath is PATH as set by the user's login shell; argv-form
	// commands are looked up in it (docs/exec.md "shim").
	LoginPath string `cbor:"10,keyasint"`
}

// SessionIDLen is the length of a session ID: lowercase hex of 8 random
// bytes. The ID appears in paths that Claude sees, so it must never contain
// characters that need quoting (docs/claude-code.md "CLAUDE_CODE_SHELL_PREFIX").
const SessionIDLen = 16

// CheckSessionID validates a session ID received from a peer.
func CheckSessionID(id string) error {
	if len(id) != SessionIDLen {
		return fmt.Errorf("proto: session id %q: want %d hex characters", id, SessionIDLen)
	}
	for i := range len(id) {
		c := id[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return fmt.Errorf("proto: session id %q: not lowercase hex", id)
		}
	}
	return nil
}

// Error is a failure reported by the peer. Errno carries the remote errno
// unchanged when the failure came from a system call, so that errors.Is
// works against unix.Errno values on the receiving side.
type Error struct {
	Errno uint32 `cbor:"1,keyasint,omitempty"`
	Msg   string `cbor:"2,keyasint,omitempty"`
}

func (e *Error) Error() string {
	switch {
	case e.Msg != "" && e.Errno != 0:
		return e.Msg + ": " + unix.Errno(e.Errno).Error()
	case e.Msg != "":
		return e.Msg
	case e.Errno != 0:
		return unix.Errno(e.Errno).Error()
	default:
		return "remote error"
	}
}

// Unwrap exposes the remote errno.
func (e *Error) Unwrap() error {
	if e.Errno == 0 {
		return nil
	}
	return unix.Errno(e.Errno)
}

// ErrorFrom converts err for transmission, keeping its errno if it wraps
// one. It returns nil for a nil error.
func ErrorFrom(err error) *Error {
	if err == nil {
		return nil
	}
	pe := &Error{Msg: err.Error()}
	var errno unix.Errno
	if errors.As(err, &errno) {
		pe.Errno = uint32(errno)
	}
	return pe
}
