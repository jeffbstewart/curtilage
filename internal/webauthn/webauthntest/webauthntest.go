// Package webauthntest synthesizes WebAuthn ceremonies the way a
// browser would, so packages building ON the verifier (the house
// admin area) can test their flows without one.  Test support only.
package webauthntest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"

	"github.com/jeffbstewart/curtilage/internal/webauthn"
)

// Authenticator is one fake ES256 passkey.
type Authenticator struct {
	Key    *ecdsa.PrivateKey
	CredID []byte
	Count  uint32
}

// New mints a fake authenticator with its own P-256 key.
func New(credID string) *Authenticator {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	return &Authenticator{Key: key, CredID: []byte(credID)}
}

// Register produces a create() ceremony's outputs for rp.
func (a *Authenticator) Register(rp webauthn.RelyingParty, challenge []byte) (clientDataJSON, attestationObject []byte) {
	cose := cborMap(
		kv{cborInt(1), cborInt(2)}, kv{cborInt(3), cborInt(-7)},
		kv{cborInt(-1), cborInt(1)},
		kv{cborInt(-2), cborBytes(a.Key.X.FillBytes(make([]byte, 32)))},
		kv{cborInt(-3), cborBytes(a.Key.Y.FillBytes(make([]byte, 32)))},
	)
	ad := authData(rp.ID, 0x01|0x04|0x40, a.Count, a.CredID, cose)
	obj := cborMap(
		kv{cborText("fmt"), cborText("none")},
		kv{cborText("attStmt"), cborMap()},
		kv{cborText("authData"), cborBytes(ad)},
	)
	return clientData("webauthn.create", challenge, rp.Origin), obj
}

// Assert produces a get() ceremony's outputs for rp, bumping the
// counter.
func (a *Authenticator) Assert(rp webauthn.RelyingParty, challenge []byte) (clientDataJSON, authenticatorData, signature []byte) {
	a.Count++
	cd := clientData("webauthn.get", challenge, rp.Origin)
	ad := authData(rp.ID, 0x01|0x04, a.Count, nil, nil)
	cdh := sha256.Sum256(cd)
	digest := sha256.Sum256(append(append([]byte{}, ad...), cdh[:]...))
	sig, err := ecdsa.SignASN1(rand.Reader, a.Key, digest[:])
	if err != nil {
		panic(err)
	}
	return cd, ad, sig
}

func clientData(typ string, challenge []byte, origin string) []byte {
	b, _ := json.Marshal(map[string]string{
		"type": typ, "challenge": base64.RawURLEncoding.EncodeToString(challenge), "origin": origin,
	})
	return b
}

func authData(rpID string, flags byte, count uint32, credID, coseKey []byte) []byte {
	h := sha256.Sum256([]byte(rpID))
	out := append([]byte{}, h[:]...)
	out = append(out, flags, byte(count>>24), byte(count>>16), byte(count>>8), byte(count))
	if credID != nil {
		out = append(out, make([]byte, 16)...)
		out = append(out, byte(len(credID)>>8), byte(len(credID)))
		out = append(out, credID...)
		out = append(out, coseKey...)
	}
	return out
}

type kv struct{ k, v []byte }

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

func cborMap(items ...kv) []byte {
	h := cborUint(uint64(len(items)))
	h[0] |= 0xa0
	for _, it := range items {
		h = append(h, it.k...)
		h = append(h, it.v...)
	}
	return h
}
