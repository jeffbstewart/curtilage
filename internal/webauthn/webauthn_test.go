package webauthn

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// The tests synthesize both ceremonies with their own keys: a tiny
// CBOR writer below builds attestation objects the way a browser
// would, so every verifier path runs without one.

func cborUint(v uint64) []byte {
	switch {
	case v < 24:
		return []byte{byte(v)}
	case v < 256:
		return []byte{24, byte(v)}
	default:
		return []byte{25, byte(v >> 8), byte(v)}
	}
}

func cborInt(v int64) []byte {
	if v >= 0 {
		return cborUint(uint64(v))
	}
	b := cborUint(uint64(-1 - v))
	b[0] |= 0x20
	return b
}

func cborBytes(b []byte) []byte {
	h := cborUint(uint64(len(b)))
	h[0] |= 0x40
	return append(h, b...)
}

func cborText(s string) []byte {
	h := cborUint(uint64(len(s)))
	h[0] |= 0x60
	return append(h, s...)
}

type kv struct {
	k, v []byte
}

func cborMap(items ...kv) []byte {
	h := cborUint(uint64(len(items)))
	h[0] |= 0xa0
	for _, it := range items {
		h = append(h, it.k...)
		h = append(h, it.v...)
	}
	return h
}

var rp = RelyingParty{Origin: "https://curtilage.example.net", ID: "curtilage.example.net"}

func clientDataJSON(typ string, challenge []byte, origin string) []byte {
	b, _ := json.Marshal(map[string]string{
		"type": typ, "challenge": base64.RawURLEncoding.EncodeToString(challenge), "origin": origin,
	})
	return b
}

// authData builds authenticator data for rp with flags/count and
// optional attested credential (credID + COSE key bytes).
func authData(rpID string, flags byte, count uint32, credID, coseKey []byte) []byte {
	h := sha256.Sum256([]byte(rpID))
	out := append([]byte{}, h[:]...)
	out = append(out, flags, byte(count>>24), byte(count>>16), byte(count>>8), byte(count))
	if credID != nil {
		out = append(out, make([]byte, 16)...) // aaguid
		out = append(out, byte(len(credID)>>8), byte(len(credID)))
		out = append(out, credID...)
		out = append(out, coseKey...)
	}
	return out
}

func attObj(authData []byte) []byte {
	return cborMap(
		kv{cborText("fmt"), cborText("none")},
		kv{cborText("attStmt"), cborMap()},
		kv{cborText("authData"), cborBytes(authData)},
	)
}

func es256Key(t *testing.T) (*ecdsa.PrivateKey, []byte) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cose := cborMap(
		kv{cborInt(1), cborInt(2)}, kv{cborInt(3), cborInt(-7)},
		kv{cborInt(-1), cborInt(1)},
		kv{cborInt(-2), cborBytes(priv.X.FillBytes(make([]byte, 32)))},
		kv{cborInt(-3), cborBytes(priv.Y.FillBytes(make([]byte, 32)))},
	)
	return priv, cose
}

func TestRegistrationAndAssertionES256(t *testing.T) {
	priv, cose := es256Key(t)
	challenge := []byte("registration-challenge-1")
	credID := []byte("cred-id-0001")
	ad := authData(rp.ID, flagUP|flagUV|flagAT, 0, credID, cose)
	cred, err := rp.VerifyRegistration(clientDataJSON("webauthn.create", challenge, rp.Origin), attObj(ad), challenge)
	if err != nil {
		t.Fatal(err)
	}
	if string(cred.ID) != string(credID) || cred.Alg != -7 {
		t.Fatalf("credential: %+v", cred)
	}

	// A signed assertion verifies and reports the counter.
	ac := []byte("assertion-challenge-1")
	acd := clientDataJSON("webauthn.get", ac, rp.Origin)
	aad := authData(rp.ID, flagUP|flagUV, 7, nil, nil)
	cdh := sha256.Sum256(acd)
	digest := sha256.Sum256(append(append([]byte{}, aad...), cdh[:]...))
	sig, err := ecdsa.SignASN1(rand.Reader, priv, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	count, err := rp.VerifyAssertion(cred, acd, aad, sig, ac)
	if err != nil || count != 7 {
		t.Fatalf("assertion: %d, %v", count, err)
	}

	// Every refusal path, each one mutation away from the good case.
	bad := func(name string, cd, ad2, sig2, ch []byte, wantErr string) {
		t.Helper()
		if _, err := rp.VerifyAssertion(cred, cd, ad2, sig2, ch); err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Errorf("%s -> %v, want %q", name, err, wantErr)
		}
	}
	bad("wrong challenge", acd, aad, sig, []byte("other"), "challenge")
	bad("wrong type", clientDataJSON("webauthn.create", ac, rp.Origin), aad, sig, ac, "type")
	bad("wrong origin", clientDataJSON("webauthn.get", ac, "https://evil.example"), aad, sig, ac, "origin")
	bad("wrong rp", acd, authData("evil.example", flagUP|flagUV, 7, nil, nil), sig, ac, "rp id")
	bad("no UV", acd, authData(rp.ID, flagUP, 7, nil, nil), sig, ac, "not verified")
	bad("bad signature", acd, aad, append([]byte{0}, sig...), ac, "does not verify")

	// A counter that goes backwards on a counting authenticator is a
	// clone; a never-counting one (both zero) is Apple, and fine.
	counted := cred
	counted.SignCount = 7
	if _, err := rp.VerifyAssertion(counted, acd, aad, sig, ac); err == nil || !strings.Contains(err.Error(), "counter") {
		t.Errorf("replayed counter -> %v", err)
	}
	zeroAD := authData(rp.ID, flagUP|flagUV, 0, nil, nil)
	zd := sha256.Sum256(append(append([]byte{}, zeroAD...), cdh[:]...))
	zsig, _ := ecdsa.SignASN1(rand.Reader, priv, zd[:])
	if _, err := rp.VerifyAssertion(cred, acd, zeroAD, zsig, ac); err != nil {
		t.Errorf("zero counter (Apple) -> %v", err)
	}
}

func TestRegistrationRefusals(t *testing.T) {
	_, cose := es256Key(t)
	challenge := []byte("reg-challenge")
	good := func() ([]byte, []byte) {
		return clientDataJSON("webauthn.create", challenge, rp.Origin),
			attObj(authData(rp.ID, flagUP|flagUV|flagAT, 0, []byte("id"), cose))
	}
	cd, _ := good()
	for name, tc := range map[string]struct {
		cd, obj []byte
		wantErr string
	}{
		"no UV":        {cd, attObj(authData(rp.ID, flagUP|flagAT, 0, []byte("id"), cose)), "not verified"},
		"no AT":        {cd, attObj(authData(rp.ID, flagUP|flagUV, 0, nil, nil)), "attested"},
		"not CBOR":     {cd, []byte("junk"), "CBOR"},
		"wrong origin": {clientDataJSON("webauthn.create", challenge, "https://evil.example"), attObj(authData(rp.ID, flagUP|flagUV|flagAT, 0, []byte("id"), cose)), "origin"},
	} {
		if _, err := rp.VerifyRegistration(tc.cd, tc.obj, challenge); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%s -> %v, want %q", name, err, tc.wantErr)
		}
	}
}

func TestRS256(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	cose := cborMap(
		kv{cborInt(1), cborInt(3)}, kv{cborInt(3), cborInt(-257)},
		kv{cborInt(-1), cborBytes(priv.N.Bytes())},
		kv{cborInt(-2), cborBytes([]byte{1, 0, 1})},
	)
	challenge := []byte("rsa-reg")
	cred, err := rp.VerifyRegistration(
		clientDataJSON("webauthn.create", challenge, rp.Origin),
		attObj(authData(rp.ID, flagUP|flagUV|flagAT, 0, []byte("rsa-cred"), cose)), challenge)
	if err != nil {
		t.Fatal(err)
	}
	ac := []byte("rsa-assert")
	acd := clientDataJSON("webauthn.get", ac, rp.Origin)
	aad := authData(rp.ID, flagUP|flagUV, 3, nil, nil)
	cdh := sha256.Sum256(acd)
	digest := sha256.Sum256(append(append([]byte{}, aad...), cdh[:]...))
	sig, err := rsa.SignPKCS1v15(rand.Reader, priv, 5, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	if count, err := rp.VerifyAssertion(cred, acd, aad, sig, ac); err != nil || count != 3 {
		t.Fatalf("rsa assertion: %d, %v", count, err)
	}
}
