// Package webauthn is the minimal server side of two WebAuthn
// ceremonies -- registering a passkey and verifying an assertion --
// for the house page's admin gate (docs/DESIGN.md "gRPC
// authentication").
//
// Minimal on purpose, in touchvault's shape: a pure verification
// core, no transport, no browser, everything reachable from a test
// that synthesizes ceremonies with its own keys.  Attestation is
// deliberately not verified: platform passkeys (iCloud, Windows
// Hello) attest "none", and the admin gate's trust comes from WHERE
// registration happened -- the subnet-gated house page -- not from
// authenticator provenance.  ES256 and RS256 cover Apple and Windows
// Hello; user verification (the biometric or PIN) is REQUIRED, this
// being an admin gate.
package webauthn

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
)

// COSE algorithm identifiers.
const (
	algES256 = -7
	algRS256 = -257
)

// Credential is one registered passkey, as the registry stores it.
type Credential struct {
	// ID is the authenticator's credential id, raw bytes.
	ID []byte
	// Alg is the COSE algorithm (-7 ES256, -257 RS256).
	Alg int64
	// Key is the public key: *ecdsa.PublicKey or *rsa.PublicKey.
	Key any
	// COSE is the key's raw COSE bytes as registered: what a registry
	// persists, ParseCOSEKey being the way back.
	COSE []byte
	// SignCount is the authenticator's signature counter as last
	// seen; 0 forever on platforms that do not count (Apple).
	SignCount uint32
}

// RelyingParty binds ceremonies to one origin: the house page's
// exact scheme://host, from which the RP id (the host) follows.
type RelyingParty struct {
	Origin string // "https://curtilage.example.net"
	ID     string // "curtilage.example.net"
}

// authenticator data flags.
const (
	flagUP = 0x01 // user present
	flagUV = 0x04 // user verified (the biometric or PIN)
	flagAT = 0x40 // attested credential data follows
)

// clientData is the browser's signed statement of what it did.
type clientData struct {
	Type      string `json:"type"`
	Challenge string `json:"challenge"`
	Origin    string `json:"origin"`
}

func (rp RelyingParty) checkClientData(raw []byte, wantType string, challenge []byte) error {
	var cd clientData
	if err := json.Unmarshal(raw, &cd); err != nil {
		return fmt.Errorf("webauthn: client data: %w", err)
	}
	if cd.Type != wantType {
		return fmt.Errorf("webauthn: client data type %q, want %q", cd.Type, wantType)
	}
	got, err := base64.RawURLEncoding.DecodeString(cd.Challenge)
	if err != nil || subtle.ConstantTimeCompare(got, challenge) != 1 {
		return errors.New("webauthn: challenge mismatch")
	}
	if cd.Origin != rp.Origin {
		return fmt.Errorf("webauthn: origin %q, want %q", cd.Origin, rp.Origin)
	}
	return nil
}

// checkAuthData verifies the RP id hash and the presence/verification
// flags, returning flags and count.
func (rp RelyingParty) checkAuthData(authData []byte) (flags byte, count uint32, err error) {
	if len(authData) < 37 {
		return 0, 0, errors.New("webauthn: authenticator data too short")
	}
	want := sha256.Sum256([]byte(rp.ID))
	if subtle.ConstantTimeCompare(authData[:32], want[:]) != 1 {
		return 0, 0, errors.New("webauthn: rp id hash mismatch")
	}
	flags = authData[32]
	if flags&flagUP == 0 {
		return 0, 0, errors.New("webauthn: user not present")
	}
	if flags&flagUV == 0 {
		return 0, 0, errors.New("webauthn: user not verified (the admin gate requires the biometric or PIN)")
	}
	count = uint32(authData[33])<<24 | uint32(authData[34])<<16 | uint32(authData[35])<<8 | uint32(authData[36])
	return flags, count, nil
}

// VerifyRegistration checks a create() ceremony and returns the new
// credential.  Attestation statements are ignored by policy (see the
// package comment).
func (rp RelyingParty) VerifyRegistration(clientDataJSON, attestationObject, challenge []byte) (Credential, error) {
	if err := rp.checkClientData(clientDataJSON, "webauthn.create", challenge); err != nil {
		return Credential{}, err
	}
	obj, err := decodeCBOR(attestationObject)
	if err != nil {
		return Credential{}, err
	}
	m, ok := obj.(map[any]any)
	if !ok {
		return Credential{}, errors.New("webauthn: attestation object is not a map")
	}
	authData, ok := m["authData"].([]byte)
	if !ok {
		return Credential{}, errors.New("webauthn: attestation object without authData")
	}
	flags, count, err := rp.checkAuthData(authData)
	if err != nil {
		return Credential{}, err
	}
	if flags&flagAT == 0 {
		return Credential{}, errors.New("webauthn: no attested credential data")
	}
	// aaguid(16) credIdLen(2) credId coseKey(CBOR).
	rest := authData[37:]
	if len(rest) < 18 {
		return Credential{}, errors.New("webauthn: attested credential data too short")
	}
	idLen := int(rest[16])<<8 | int(rest[17])
	if idLen == 0 || idLen > 1023 || len(rest) < 18+idLen {
		return Credential{}, errors.New("webauthn: credential id length")
	}
	credID := bytes.Clone(rest[18 : 18+idLen])
	coseRaw := rest[18+idLen:]
	alg, key, n, err := parseCOSEKey(coseRaw)
	if err != nil {
		return Credential{}, err
	}
	return Credential{ID: credID, Alg: alg, Key: key, COSE: bytes.Clone(coseRaw[:n]), SignCount: count}, nil
}

// ParseCOSEKey rebuilds a stored credential's key from its COSE
// bytes: the registry's way back from disk.
func ParseCOSEKey(b []byte) (alg int64, key any, err error) {
	alg, key, _, err = parseCOSEKey(b)
	return alg, key, err
}

// VerifyAssertion checks a get() ceremony against a stored
// credential and returns the authenticator's new signature count.
func (rp RelyingParty) VerifyAssertion(cred Credential, clientDataJSON, authData, sig, challenge []byte) (uint32, error) {
	if err := rp.checkClientData(clientDataJSON, "webauthn.get", challenge); err != nil {
		return 0, err
	}
	_, count, err := rp.checkAuthData(authData)
	if err != nil {
		return 0, err
	}
	// A counter that goes backwards is a cloned credential -- unless
	// the authenticator never counts (both zero: Apple).
	if cred.SignCount > 0 && count <= cred.SignCount {
		return 0, errors.New("webauthn: signature counter went backwards (cloned credential?)")
	}
	cdHash := sha256.Sum256(clientDataJSON)
	signed := sha256.Sum256(append(bytes.Clone(authData), cdHash[:]...))
	switch key := cred.Key.(type) {
	case *ecdsa.PublicKey:
		if !ecdsa.VerifyASN1(key, signed[:], sig) {
			return 0, errors.New("webauthn: signature does not verify")
		}
	case *rsa.PublicKey:
		if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, signed[:], sig); err != nil {
			return 0, errors.New("webauthn: signature does not verify")
		}
	default:
		return 0, errors.New("webauthn: unsupported key type")
	}
	return count, nil
}

// parseCOSEKey reads an EC2 P-256 or RSA COSE public key, returning
// how many bytes it spanned (COSE keys in authenticator data are
// delimited only by their own encoding).
func parseCOSEKey(b []byte) (int64, any, int, error) {
	v, n, err := decodeCBORPrefix(b)
	if err != nil {
		return 0, nil, 0, err
	}
	m, ok := v.(map[any]any)
	if !ok {
		return 0, nil, 0, errors.New("webauthn: COSE key is not a map")
	}
	kty, _ := m[int64(1)].(int64)
	alg, _ := m[int64(3)].(int64)
	switch {
	case kty == 2 && alg == algES256: // EC2
		crv, _ := m[int64(-1)].(int64)
		x, _ := m[int64(-2)].([]byte)
		y, _ := m[int64(-3)].([]byte)
		if crv != 1 || len(x) != 32 || len(y) != 32 {
			return 0, nil, 0, errors.New("webauthn: EC2 key shape")
		}
		pub := &ecdsa.PublicKey{Curve: elliptic.P256(),
			X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
		if !pub.Curve.IsOnCurve(pub.X, pub.Y) {
			return 0, nil, 0, errors.New("webauthn: point not on curve")
		}
		return alg, pub, n, nil
	case kty == 3 && alg == algRS256: // RSA
		mod, _ := m[int64(-1)].([]byte)
		e, _ := m[int64(-2)].([]byte)
		if len(mod) < 256 || len(e) == 0 || len(e) > 4 {
			return 0, nil, 0, errors.New("webauthn: RSA key shape")
		}
		exp := 0
		for _, c := range e {
			exp = exp<<8 | int(c)
		}
		if exp < 3 {
			return 0, nil, 0, errors.New("webauthn: RSA exponent")
		}
		return alg, &rsa.PublicKey{N: new(big.Int).SetBytes(mod), E: exp}, n, nil
	}
	return 0, nil, 0, fmt.Errorf("webauthn: unsupported COSE key (kty %d alg %d)", kty, alg)
}
