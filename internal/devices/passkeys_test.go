package devices

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/jeffbstewart/curtilage/internal/webauthn"
)

// A registered credential survives the file: reload parses the COSE
// bytes back into a verifying key.  The COSE bytes here are a real
// EC2 map (the webauthn tests exercise the crypto; this test
// exercises the round trip).
func TestPasskeyRoundTrip(t *testing.T) {
	// A valid EC2 P-256 COSE key: generator point coordinates.
	gx := []byte{0x6b, 0x17, 0xd1, 0xf2, 0xe1, 0x2c, 0x42, 0x47, 0xf8, 0xbc, 0xe6, 0xe5, 0x63, 0xa4, 0x40, 0xf2,
		0x77, 0x03, 0x7d, 0x81, 0x2d, 0xeb, 0x33, 0xa0, 0xf4, 0xa1, 0x39, 0x45, 0xd8, 0x98, 0xc2, 0x96}
	gy := []byte{0x4f, 0xe3, 0x42, 0xe2, 0xfe, 0x1a, 0x7f, 0x9b, 0x8e, 0xe7, 0xeb, 0x4a, 0x7c, 0x0f, 0x9e, 0x16,
		0x2b, 0xce, 0x33, 0x57, 0x6b, 0x31, 0x5e, 0xce, 0xcb, 0xb6, 0x40, 0x68, 0x37, 0xbf, 0x51, 0xf5}
	cose := []byte{0xa5, 0x01, 0x02, 0x03, 0x26, 0x20, 0x01, 0x21, 0x58, 0x20}
	cose = append(cose, gx...)
	cose = append(cose, 0x22, 0x58, 0x20)
	cose = append(cose, gy...)
	alg, key, err := webauthn.ParseCOSEKey(cose)
	if err != nil || alg != -7 {
		t.Fatalf("test key: %d %v", alg, err)
	}
	cred := webauthn.Credential{ID: []byte("cred-1"), Alg: alg, Key: key, COSE: cose, SignCount: 4}

	path := filepath.Join(t.TempDir(), "devices.textproto")
	r, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 9, 7, 23, 0, 0, 0, time.UTC)
	if r.HasPasskeys() {
		t.Fatal("passkeys before any registration")
	}
	pk, err := r.AddPasskey("Jeff's phone", cred, t0)
	if err != nil || pk.ID == "" {
		t.Fatalf("add: %+v %v", pk, err)
	}
	if _, err := r.AddPasskey("again", cred, t0); err == nil {
		t.Fatal("the same credential registered twice")
	}
	if !r.HasPasskeys() {
		t.Fatal("bootstrap door still open")
	}

	r2, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := r2.PasskeyByCredentialID([]byte("cred-1"))
	if !ok || got.Name != "Jeff's phone" || got.Credential.SignCount != 4 || got.Credential.Key == nil {
		t.Fatalf("reload: %+v %v", got, ok)
	}
	r2.SetPasskeySignCount(got.ID, 9, t0)
	if got, _ = r2.PasskeyByCredentialID([]byte("cred-1")); got.Credential.SignCount != 9 {
		t.Fatalf("count: %+v", got)
	}

	// The last key cannot be removed; a second can, then the first
	// still cannot.
	if err := r2.RemovePasskey(got.ID, t0); err == nil {
		t.Fatal("removed the last passkey")
	}
	cred2 := cred
	cred2.ID = []byte("cred-2")
	second, err := r2.AddPasskey("yubikey", cred2, t0)
	if err != nil {
		t.Fatal(err)
	}
	if err := r2.RemovePasskey(second.ID, t0); err != nil {
		t.Fatalf("remove second: %v", err)
	}
	if len(r2.Passkeys()) != 1 {
		t.Fatalf("passkeys: %+v", r2.Passkeys())
	}
	if err := r2.RemovePasskey("nope", t0); err == nil {
		t.Fatal("removed a passkey that never was")
	}
}
