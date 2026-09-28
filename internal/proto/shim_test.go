package proto

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"
)

// TestEncodeShimRequestLimit checks the boundary of MaxShimRequest on both
// sides: a request of exactly the limit is encoded and read back, one byte
// more is refused before anything could be sent.
func TestEncodeShimRequestLimit(t *testing.T) {
	// withEnv returns a request whose one Env entry is n bytes. Beyond
	// 65535 bytes the string header has a fixed size, so the payload grows
	// by exactly one byte per byte of Env.
	withEnv := func(n int) *ShimRequest {
		return &ShimRequest{Token: []byte("t"), Name: "bash", Argv: []string{"bash"}, Dir: "/", Env: []string{strings.Repeat("x", n)}}
	}
	const probe = 1 << 16
	b, err := Marshal(withEnv(probe))
	if err != nil {
		t.Fatal(err)
	}
	atLimit := MaxShimRequest - (len(b) - probe)

	frame, err := EncodeShimRequest(withEnv(atLimit))
	if err != nil {
		t.Fatalf("request of exactly MaxShimRequest bytes: %v", err)
	}
	var got ShimRequest
	if err := ReadFrame(bytes.NewReader(frame), &got, MaxShimRequest); err != nil {
		t.Fatalf("ReadFrame at the limit: %v", err)
	}
	if len(got.Env) != 1 || len(got.Env[0]) != atLimit || !slices.Equal(got.Argv, []string{"bash"}) {
		t.Errorf("round trip lost data: argv %q, %d env entries", got.Argv, len(got.Env))
	}

	for _, n := range []int{atLimit + 1, MaxDataFrame} {
		var tooLarge *FrameTooLargeError
		_, err := EncodeShimRequest(withEnv(n))
		if !errors.As(err, &tooLarge) || tooLarge.Limit != MaxShimRequest || tooLarge.Size <= MaxShimRequest {
			t.Errorf("Env of %d bytes: %v, want FrameTooLargeError with limit %d", n, err, MaxShimRequest)
		}
	}
}
