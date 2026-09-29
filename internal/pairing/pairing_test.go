package pairing

import (
	"errors"
	"strings"
	"testing"
)

func TestOfferReceipt(t *testing.T) {
	o, err := NewOffer(8443)
	if err != nil {
		t.Fatal(err)
	}
	s, err := o.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(s, OfferPrefix) || strings.ContainsAny(s, " \n'\"") {
		t.Fatalf("offer %q is not a shell-safe word with the offer prefix", s)
	}
	got, err := ParseOffer("  " + s + "\n") // pasted with whitespace
	if err != nil {
		t.Fatal(err)
	}
	if got.PSK != o.PSK || string(got.Token) != string(o.Token) || got.Port != 8443 {
		t.Fatal("offer does not round-trip")
	}

	r := NewReceipt(got, 9000, "bob", "build1")
	rs, err := r.Encode()
	if err != nil {
		t.Fatal(err)
	}
	gr, err := ParseReceipt(rs)
	if err != nil {
		t.Fatal(err)
	}
	if gr.Port != 9000 || gr.User != "bob" || gr.Hostname != "build1" {
		t.Fatalf("receipt = %+v", gr)
	}
	if err := gr.Verify(o.PSK, o.Token); err != nil {
		t.Fatalf("Verify with the offer's key: %v", err)
	}
	// A receipt for another offer, or produced with another key, fails.
	other, _ := NewOffer(8443)
	if err := gr.Verify(other.PSK, o.Token); !errors.Is(err, ErrMismatch) {
		t.Errorf("Verify with another PSK = %v", err)
	}
	if err := gr.Verify(o.PSK, other.Token); !errors.Is(err, ErrMismatch) {
		t.Errorf("Verify with another token = %v", err)
	}
	// The receipt carries no secret.
	if strings.Contains(rs, o.PSK.Encode()) {
		t.Error("receipt contains the PSK")
	}
}

func TestParseErrors(t *testing.T) {
	o, _ := NewOffer(1)
	good, _ := o.Encode()
	r, _ := NewReceipt(o, 1, "u", "h").Encode()
	for _, s := range []string{
		"",
		"tele1:",
		"tele1:!!!",
		"tele2:" + strings.TrimPrefix(good, OfferPrefix),
		r, // a receipt is not an offer
		OfferPrefix + strings.Repeat("A", maxEncoded),
		OfferPrefix + "oA", // an empty CBOR map: no token, no port
	} {
		if _, err := ParseOffer(s); err == nil {
			t.Errorf("ParseOffer(%.40q) succeeded", s)
		}
	}
	if _, err := ParseReceipt(good); err == nil {
		t.Error("ParseReceipt accepted an offer")
	}
}

// TestErrorsDoNotLeak checks that a damaged offer, which still holds most
// of the PSK, is not echoed in the error.
func TestErrorsDoNotLeak(t *testing.T) {
	o, _ := NewOffer(1)
	s, _ := o.Encode()
	damaged := s[:len(s)-3] + "***"
	_, err := ParseOffer(damaged)
	if err == nil {
		t.Fatal("damaged offer parsed")
	}
	if strings.Contains(err.Error(), s[len(OfferPrefix):len(OfferPrefix)+20]) {
		t.Fatalf("error echoes the offer: %v", err)
	}
}
