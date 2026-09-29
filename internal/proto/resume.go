package proto

import "fmt"

// Messages of the resumable session layer (docs/transport.md "可恢复会话层"),
// which carries the multiplexed streams of one session across transport
// connections. Every transport connection starts with ResumeHello.
//
// A new session: the client sends ResumeHello with Key and no Session; the
// server answers ResumeReply with the Session ID it assigned.
//
// A resumed session: the client sends ResumeHello with Session; the server
// answers ResumeReply with a Nonce; the client proves that it knows the
// session key with ResumeProof; the server answers ResumeReply again.
// Knowing the ID alone never lets anyone take over a session. Every
// refusal is a ResumeReply with Err.
//
// Then both sides exchange SessionFrames. Each side counts the bytes of the
// session's byte stream it received; Recv in the handshake and Ack in
// frames report that count, and on resumption each side sends the peer's
// bytes again from the count the peer reported, so every byte arrives
// exactly once and in order.

// ResumeVersion is the version of the resumable session layer.
const ResumeVersion = 1

// Sizes in the resumable session layer.
const (
	// ResumeIDLen is the length of a session ID of this layer.
	ResumeIDLen = 16
	// ResumeKeyLen is the length of a session key.
	ResumeKeyLen = 32
	// ResumeNonceLen is the length of a resumption challenge.
	ResumeNonceLen = 32
	// MaxSessionData bounds the data of one SessionFrame.
	MaxSessionData = 64 << 10
	// MaxSessionFrame bounds every frame of the layer.
	MaxSessionFrame = MaxSessionData + 1024
)

// ResumeHello opens a transport connection.
type ResumeHello struct {
	Version int `cbor:"1,keyasint"`
	// Session is the ID of the session to resume; empty for a new one.
	Session []byte `cbor:"2,keyasint,omitempty"`
	// Key is the key of a new session, chosen by the client. It travels
	// only over the authenticated, encrypted transport.
	Key []byte `cbor:"3,keyasint,omitempty"`
}

// ResumeProof answers the ResumeReply that carries a Nonce.
type ResumeProof struct {
	// MAC is HMAC-SHA256 over the nonce, keyed with the session key.
	MAC []byte `cbor:"1,keyasint"`
	// Recv is the count of session bytes the client received.
	Recv uint64 `cbor:"2,keyasint"`
}

// ResumeReply ends the handshake. When Err is set, the server closes the
// connection; the session, if any, is unaffected.
type ResumeReply struct {
	Version int    `cbor:"1,keyasint"`
	Err     *Error `cbor:"2,keyasint,omitempty"`
	// Session is the ID of the session, new or resumed.
	Session []byte `cbor:"3,keyasint,omitempty"`
	// Recv is the count of session bytes the server received.
	Recv uint64 `cbor:"4,keyasint"`
	// Nonce, in the first reply to a resumption, is the challenge that
	// ResumeProof answers.
	Nonce []byte `cbor:"5,keyasint,omitempty"`
}

// SessionFrameKind identifies a SessionFrame.
type SessionFrameKind uint8

// Session frame kinds.
const (
	// SessionData carries the next Data bytes of the session.
	SessionData SessionFrameKind = 1
	// SessionAck reports Ack, the count of session bytes received.
	SessionAck SessionFrameKind = 2
	// SessionPing asks for a SessionPong; both keep the connection's
	// liveness known.
	SessionPing SessionFrameKind = 3
	// SessionPong answers SessionPing.
	SessionPong SessionFrameKind = 4
	// SessionFin ends the session: every byte before it was sent, and
	// the sender will not resume it.
	SessionFin SessionFrameKind = 5
)

// SessionFrame is every frame after the handshake.
type SessionFrame struct {
	Kind SessionFrameKind `cbor:"1,keyasint"`
	Data []byte           `cbor:"2,keyasint,omitempty"`
	Ack  uint64           `cbor:"3,keyasint,omitempty"`
}

// Check validates a frame received from the peer.
func (f *SessionFrame) Check() error {
	if f.Kind < SessionData || f.Kind > SessionFin {
		return fmt.Errorf("proto: invalid session frame kind %d", f.Kind)
	}
	if len(f.Data) > MaxSessionData || (f.Kind != SessionData && len(f.Data) != 0) {
		return fmt.Errorf("proto: invalid session frame data of %d bytes", len(f.Data))
	}
	return nil
}
