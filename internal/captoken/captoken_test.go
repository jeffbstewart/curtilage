package captoken

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"
)

func keyOf(b byte) []byte { return bytes.Repeat([]byte{b}, MinKeyLen) }

var now = time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)

func TestMintVerifyRoundTrip(t *testing.T) {
	kr, err := New(keyOf(1), nil)
	if err != nil {
		t.Fatal(err)
	}
	in := Claims{EventID: "ABCDEFGHIJKLMNOPQRST", Media: 1, Expires: now.Add(4 * time.Hour)}
	tok := kr.Mint(in)
	if strings.ContainsAny(tok, "+/=") {
		t.Errorf("token is not URL-safe: %q", tok)
	}
	got, err := kr.Verify(tok, now)
	if err != nil || got != in {
		t.Fatalf("Verify = %+v, %v; want %+v", got, err, in)
	}
	if _, err := kr.Verify(tok, in.Expires); !errors.Is(err, ErrExpired) {
		t.Errorf("at expiry -> %v, want ErrExpired", err)
	}
	if _, err := kr.Verify(tok, in.Expires.Add(-time.Second)); err != nil {
		t.Errorf("a second before expiry -> %v", err)
	}
}

func TestTamperingIsDetected(t *testing.T) {
	kr, _ := New(keyOf(1), nil)
	tok := kr.Mint(Claims{EventID: "event-1", Media: 1, Expires: now.Add(time.Hour)})
	raw, _ := base64.RawURLEncoding.DecodeString(tok)
	for i := range raw {
		flipped := append([]byte(nil), raw...)
		flipped[i] ^= 0x01
		_, err := kr.Verify(base64.RawURLEncoding.EncodeToString(flipped), now)
		switch {
		case i < kidLen && !errors.Is(err, ErrUnknownKey):
			t.Errorf("byte %d (kid) flipped -> %v", i, err)
		case i >= kidLen && !errors.Is(err, ErrBadMAC):
			t.Errorf("byte %d flipped -> %v", i, err)
		}
	}
	for _, bad := range []string{"", "!!!", "AAAA", base64.RawURLEncoding.EncodeToString(raw[:len(raw)-macLen-1])} {
		if _, err := kr.Verify(bad, now); !errors.Is(err, ErrMalformed) {
			t.Errorf("Verify(%q) -> %v, want ErrMalformed", bad, err)
		}
	}
	// Signed by someone else's key.
	other, _ := New(keyOf(9), nil)
	if _, err := kr.Verify(other.Mint(Claims{EventID: "e", Media: 1, Expires: now.Add(time.Hour)}), now); !errors.Is(err, ErrUnknownKey) {
		t.Errorf("foreign key -> %v", err)
	}
}

// The rotation story: tokens from before the roll keep verifying while
// the old key is prior; new tokens are signed by the new key; once the
// prior is dropped, old tokens are refused.
func TestRotation(t *testing.T) {
	before, _ := New(keyOf(1), nil)
	old := before.Mint(Claims{EventID: "e1", Media: 1, Expires: now.Add(4 * time.Hour)})

	during, err := New(keyOf(2), keyOf(1))
	if err != nil {
		t.Fatal(err)
	}
	if !during.HasPrior() || during.CurrentKeyID() == before.CurrentKeyID() {
		t.Fatal("rotation not reflected in the ring")
	}
	if _, err := during.Verify(old, now); err != nil {
		t.Errorf("old token during rotation -> %v", err)
	}
	fresh := during.Mint(Claims{EventID: "e2", Media: 1, Expires: now.Add(4 * time.Hour)})
	if _, err := before.Verify(fresh, now); !errors.Is(err, ErrUnknownKey) {
		t.Errorf("an instance still on the old ring must not accept new tokens: %v", err)
	}

	after, _ := New(keyOf(2), nil)
	if _, err := after.Verify(old, now); !errors.Is(err, ErrUnknownKey) {
		t.Errorf("old token after the prior was dropped -> %v", err)
	}
	if _, err := after.Verify(fresh, now); err != nil {
		t.Errorf("new token after rotation -> %v", err)
	}
}

func TestKeyValidation(t *testing.T) {
	if _, err := New(keyOf(1)[:MinKeyLen-1], nil); err == nil {
		t.Error("short current key accepted")
	}
	if _, err := New(keyOf(1), keyOf(2)[:MinKeyLen-1]); err == nil {
		t.Error("short prior key accepted")
	}
	if _, err := New(keyOf(1), keyOf(1)); err == nil {
		t.Error("prior == current accepted")
	}
	if b, err := ParseKey(""); err != nil || b != nil {
		t.Errorf("ParseKey(\"\") = %v, %v", b, err)
	}
	good := base64.StdEncoding.EncodeToString(keyOf(7))
	if b, err := ParseKey(good); err != nil || !bytes.Equal(b, keyOf(7)) {
		t.Errorf("ParseKey(good) = %v, %v", b, err)
	}
	if _, err := ParseKey(base64.StdEncoding.EncodeToString(keyOf(7)[:16])); err == nil {
		t.Error("short configured key accepted")
	}
	if _, err := ParseKey("not base64!"); err == nil {
		t.Error("non-base64 key accepted")
	}
}

// Two instances configured with the same secrets mint and verify each
// other's tokens: the pool story.
func TestPoolAgrees(t *testing.T) {
	a, _ := New(keyOf(3), keyOf(2))
	b, _ := New(keyOf(3), keyOf(2))
	tok := a.Mint(Claims{EventID: "e", Media: 1, Expires: now.Add(time.Hour)})
	if _, err := b.Verify(tok, now); err != nil {
		t.Fatal(err)
	}
	if a.CurrentKeyID() != b.CurrentKeyID() {
		t.Error("key ids differ for the same secret")
	}
}
