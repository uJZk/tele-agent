// Package pairing encodes the strings that pair a local host alias with a
// tele server (docs/cli.md "安装与配对").
//
// "tele host add" creates an Offer: a fresh PSK, a one-time token and the
// port to listen on, printed as "tele1:…" for "tele server install
// --pair". The server answers with a Receipt, "tele1r:…", whose proof is an
// HMAC of the token under the PSK: "tele host confirm" checks that the
// server installed this very offer before the alias can be used.
//
// The offer carries the PSK, so it is as secret as the PSK itself; the
// receipt carries no secret.
package pairing

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/ujzk/tele-agent/internal/proto"
	"github.com/ujzk/tele-agent/internal/sstransport"
)

// Prefixes of the encoded strings. The digit is the format version.
const (
	OfferPrefix   = "tele1:"
	ReceiptPrefix = "tele1r:"
)

// TokenLen is the length of the one-time pairing token.
const TokenLen = 16

// maxEncoded bounds an encoded string, so that a pasted file of garbage is
// rejected before decoding.
const maxEncoded = 4096

// Offer is what the local side hands to the server.
type Offer struct {
	PSK   sstransport.PSK `cbor:"1,keyasint"`
	Token []byte          `cbor:"2,keyasint"`
	// Port is the port the server should listen on: the port of the
	// alias's endpoint, unless a NAT maps it.
	Port uint16 `cbor:"3,keyasint"`
}

// NewOffer returns an offer with a fresh PSK and token.
func NewOffer(port uint16) (*Offer, error) {
	psk, err := sstransport.NewPSK()
	if err != nil {
		return nil, err
	}
	token := make([]byte, TokenLen)
	if _, err := rand.Read(token); err != nil {
		return nil, err
	}
	return &Offer{PSK: psk, Token: token, Port: port}, nil
}

// Encode returns the offer as "tele1:…".
func (o *Offer) Encode() (string, error) {
	return encode(OfferPrefix, o)
}

// ParseOffer decodes "tele1:…".
func ParseOffer(s string) (*Offer, error) {
	var o Offer
	if err := decode(OfferPrefix, s, &o); err != nil {
		return nil, fmt.Errorf("invalid pairing string: %w", err)
	}
	if len(o.Token) != TokenLen || o.Port == 0 {
		return nil, errors.New("invalid pairing string: missing token or port")
	}
	return &o, nil
}

// Receipt is the server's answer to an offer.
type Receipt struct {
	// Proof is HMAC-SHA256 of the offer's token under its PSK.
	Proof []byte `cbor:"1,keyasint"`
	// Port is the port the server listens on.
	Port     uint16 `cbor:"2,keyasint"`
	User     string `cbor:"3,keyasint"`
	Hostname string `cbor:"4,keyasint"`
}

// NewReceipt answers offer o.
func NewReceipt(o *Offer, port uint16, user, hostname string) *Receipt {
	return &Receipt{Proof: proof(o.PSK, o.Token), Port: port, User: user, Hostname: hostname}
}

// Encode returns the receipt as "tele1r:…".
func (r *Receipt) Encode() (string, error) {
	return encode(ReceiptPrefix, r)
}

// ParseReceipt decodes "tele1r:…".
func ParseReceipt(s string) (*Receipt, error) {
	var r Receipt
	if err := decode(ReceiptPrefix, s, &r); err != nil {
		return nil, fmt.Errorf("invalid pairing receipt: %w", err)
	}
	return &r, nil
}

// ErrMismatch reports a receipt that answers another offer.
var ErrMismatch = errors.New("the receipt does not answer this pairing: it was produced for another pairing string, or the server's key differs")

// Verify checks that r answers an offer with psk and token.
func (r *Receipt) Verify(psk sstransport.PSK, token []byte) error {
	if !hmac.Equal(r.Proof, proof(psk, token)) {
		return ErrMismatch
	}
	return nil
}

func proof(psk sstransport.PSK, token []byte) []byte {
	m := hmac.New(sha256.New, psk[:])
	m.Write([]byte("tele pairing receipt\x00"))
	m.Write(token)
	return m.Sum(nil)
}

func encode(prefix string, v any) (string, error) {
	b, err := proto.Marshal(v)
	if err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b), nil
}

func decode(prefix, s string, v any) error {
	s = strings.TrimSpace(s)
	if len(s) > maxEncoded {
		return errors.New("too long")
	}
	body, ok := strings.CutPrefix(s, prefix)
	if !ok {
		return fmt.Errorf("want a string starting with %q", prefix)
	}
	b, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return errors.New("not base64url")
	}
	return proto.Unmarshal(b, v)
}
