package proto

// Shim ↔ session main protocol (docs/exec.md section 2).
//
// The shim connects to the session's abstract unix socket and first sends a
// single byte whose SCM_RIGHTS control message carries its fds 0, 1 and 2,
// in that order. Everything after that byte is frames: one ShimRequest from
// the shim, then ShimFrames in both directions.

import (
	"bytes"
	"errors"
)

// MaxShimRequest bounds the ShimRequest frame. The request carries the
// shim's argv and environment, which execve(2) accepts up to a quarter of
// RLIMIT_STACK, capped at 6 MiB including one pointer per string. CBOR
// spends at most 9 bytes of header per string where the kernel counts a
// pointer and a NUL, so the largest possible exec fits with room left for
// the other fields.
const MaxShimRequest = 8 << 20

// EncodeShimRequest returns req as a complete frame. A request larger than
// MaxShimRequest, which session main would refuse, fails with a
// *FrameTooLargeError.
func EncodeShimRequest(req *ShimRequest) ([]byte, error) {
	var buf bytes.Buffer
	if err := WriteFrame(&buf, req); err != nil {
		if tooLarge := (*FrameTooLargeError)(nil); errors.As(err, &tooLarge) {
			tooLarge.Limit = MaxShimRequest
		}
		return nil, err
	}
	if n := buf.Len() - frameHeaderLen; n > MaxShimRequest {
		return nil, &FrameTooLargeError{Size: uint64(n), Limit: MaxShimRequest}
	}
	return buf.Bytes(), nil
}

// ShimRequest describes one invocation of a shim. Classification (which
// shim, bash unwrapping, local or remote) happens in session main, so the
// shim sends what it received unchanged.
type ShimRequest struct {
	// Token is the session token read from the session directory.
	Token []byte `cbor:"1,keyasint"`
	// Name is the shim that was invoked, e.g. "bash" or "tele-exec".
	Name string `cbor:"2,keyasint"`
	// Argv is the full argument vector, including argv[0].
	Argv []string `cbor:"3,keyasint"`
	// Dir is the shim's working directory as Claude sees it.
	Dir string `cbor:"4,keyasint"`
	// Env is the shim's full environment.
	Env []string `cbor:"5,keyasint,omitempty"`
}

// ShimOp identifies a ShimFrame.
type ShimOp uint8

// Shim frame operations.
const (
	// ShimSignal (shim → session main) forwards a caught signal.
	ShimSignal ShimOp = 1
	// ShimExit (session main → shim) is the last frame and carries Exit.
	ShimExit ShimOp = 2
)

// ShimFrame is every frame after ShimRequest.
type ShimFrame struct {
	Op     ShimOp      `cbor:"1,keyasint"`
	Signal int         `cbor:"2,keyasint,omitempty"`
	Exit   *ShimStatus `cbor:"3,keyasint,omitempty"`
}

// ShimStatus tells the shim how to end. If Signal is non-zero the shim
// terminates itself with that signal; otherwise it exits with Code. A
// non-empty Msg is written to stderr as "tele: <Msg>" first.
type ShimStatus struct {
	Code   int    `cbor:"1,keyasint,omitempty"`
	Signal int    `cbor:"2,keyasint,omitempty"`
	Msg    string `cbor:"3,keyasint,omitempty"`
}
