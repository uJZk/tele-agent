// Package proto defines every message exchanged between tele processes and
// the framing they travel in: shim ↔ session main over a local unix socket,
// and session main ↔ tele server over the session channel.
//
// Messages are CBOR maps with integer keys, carried in frames prefixed with a
// 4-byte big-endian payload length. Fields may be added but never change
// meaning, and the keys of removed fields are never reused
// (docs/coding-standards.md "协议"). All input from a peer is untrusted:
// the frame length is checked before decoding, and callers validate paths
// and enum values with the Check* helpers.
package proto

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/fxamacker/cbor/v2"
)

// Version is exchanged in Hello. Bump it for any change an older peer cannot
// safely ignore.
const Version = 2

// Frame size limits. Every Recv names the limit that applies to its stream.
const (
	// MaxControlFrame bounds frames that carry only metadata.
	MaxControlFrame = 1 << 20
	// MaxDataFrame bounds frames that may carry file contents or stream
	// data. It leaves room for ScratchTotalMax plus metadata.
	MaxDataFrame = ScratchTotalMax + MaxControlFrame
	// MaxExecStart bounds the ExecStart frame. Its argv and environment
	// come from a shim request and may take up to MaxShimRequest, so the
	// frame has room for those, ScratchTotalMax of uploads and metadata:
	// a large command line never crowds out the uploads it carries.
	MaxExecStart = MaxShimRequest + MaxDataFrame
)

const frameHeaderLen = 4

var (
	encMode = mustEncMode()
	decMode = mustDecMode()
)

func mustEncMode() cbor.EncMode {
	m, err := cbor.EncOptions{}.EncMode()
	if err != nil {
		panic(fmt.Sprintf("proto: cbor enc mode: %v", err))
	}
	return m
}

func mustDecMode() cbor.DecMode {
	m, err := cbor.DecOptions{
		DupMapKey:        cbor.DupMapKeyEnforcedAPF,
		IndefLength:      cbor.IndefLengthForbidden,
		TagsMd:           cbor.TagsForbidden,
		MaxNestedLevels:  16,
		MaxArrayElements: 1 << 20,
		MaxMapPairs:      1 << 10,
	}.DecMode()
	if err != nil {
		panic(fmt.Sprintf("proto: cbor dec mode: %v", err))
	}
	return m
}

// FrameTooLargeError reports a frame whose declared or encoded size exceeds
// the limit of its stream.
type FrameTooLargeError struct {
	Size  uint64
	Limit int
}

func (e *FrameTooLargeError) Error() string {
	return fmt.Sprintf("proto: frame of %d bytes exceeds limit %d", e.Size, e.Limit)
}

// Marshal encodes v with the protocol's CBOR options.
func Marshal(v any) ([]byte, error) {
	b, err := encMode.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("proto: encode %T: %w", v, err)
	}
	return b, nil
}

// Unmarshal decodes b into v with the protocol's CBOR options.
func Unmarshal(b []byte, v any) error {
	if err := decMode.Unmarshal(b, v); err != nil {
		return fmt.Errorf("proto: decode %T: %w", v, err)
	}
	return nil
}

// WriteFrame encodes v and writes it as one frame with a single Write call,
// so that it is not interleaved with frames written concurrently on
// connections whose Write is atomic (net.Conn, yamux streams). Frames
// larger than MaxDataFrame are refused.
func WriteFrame(w io.Writer, v any) error {
	return WriteFrameLimit(w, v, MaxDataFrame)
}

// WriteFrameLimit is WriteFrame for the frames whose stream accepts more
// than MaxDataFrame, such as ExecStart (MaxExecStart). Nothing is written
// when the frame exceeds limit.
func WriteFrameLimit(w io.Writer, v any, limit int) error {
	payload, err := Marshal(v)
	if err != nil {
		return err
	}
	if len(payload) > limit {
		return &FrameTooLargeError{Size: uint64(len(payload)), Limit: limit}
	}
	buf := make([]byte, frameHeaderLen+len(payload))
	binary.BigEndian.PutUint32(buf, uint32(len(payload)))
	copy(buf[frameHeaderLen:], payload)
	_, err = w.Write(buf)
	return err
}

// ReadFrame reads one frame and decodes it into v. It returns io.EOF only
// when the stream ends cleanly at a frame boundary.
func ReadFrame(r io.Reader, v any, limit int) error {
	var hdr [frameHeaderLen]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if uint64(n) > uint64(limit) {
		return &FrameTooLargeError{Size: uint64(n), Limit: limit}
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return err
	}
	return Unmarshal(buf, v)
}

// Conn exchanges frames over a byte stream. Send may be called from several
// goroutines; Recv must be called from one goroutine at a time.
type Conn struct {
	rw    io.ReadWriteCloser
	limit int

	sendMu sync.Mutex // serializes Send
}

// NewConn returns a Conn that rejects incoming frames larger than limit.
func NewConn(rw io.ReadWriteCloser, limit int) *Conn {
	return &Conn{rw: rw, limit: limit}
}

// Send writes v as one frame.
func (c *Conn) Send(v any) error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	return WriteFrame(c.rw, v)
}

// Recv reads the next frame into v.
func (c *Conn) Recv(v any) error {
	return ReadFrame(c.rw, v, c.limit)
}

// Close closes the underlying stream.
func (c *Conn) Close() error {
	return c.rw.Close()
}
