// Package captoken mints and verifies capability tokens: a signed
// {event, media, expiry} that authorises one piece of one event's
// media for a while and nothing else (docs/DESIGN.md "Identity").  A
// leaked link leaks one event.
//
// Keys and rotation.  The signing key is configured, not generated,
// so every instance behind a load balancer mints and verifies the
// same tokens.  A Keyring holds the CURRENT key, which mints, and an
// optional PRIOR key, which only verifies.  To rotate: move current to
// prior, put the new key in current, roll the pool.  Tokens minted
// before the roll keep working until the operator drops the prior key
// -- after the token lifetime has elapsed, so nothing outstanding is
// still signed by it.  The token names its key by id, so verification
// never guesses.
//
// Token = base64url( kid[4] || claims || mac[16] ), mac = HMAC-SHA256
// over kid||claims, truncated.  Claims are big-endian: expiry (int64
// unix seconds), media (uint8), camera length (uint8) and camera
// (which camera's view, for multi-camera events; empty means the
// event's leading camera), event id (the rest).  The format is
// internal and tokens live hours: changing it (as adding camera did)
// just 404s links minted before the deploy.
package captoken

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"time"
)

// Errors from Verify.  Callers must not tell a client which one it
// hit (unsigned ids 404 identically); they are for metrics and logs.
var (
	ErrMalformed  = errors.New("captoken: malformed token")
	ErrUnknownKey = errors.New("captoken: token signed by a key this server does not hold")
	ErrBadMAC     = errors.New("captoken: signature mismatch")
	ErrExpired    = errors.New("captoken: token expired")
)

// MinKeyLen is the shortest key accepted: 32 random bytes.
const MinKeyLen = 32

const (
	kidLen   = 4
	macLen   = 16
	fixedLen = 8 + 1 + 1 // expiry + media + camera length
)

// Claims is what a token authorises.
type Claims struct {
	EventID string
	// Media kind, as the API's Media enum number.
	Media uint8
	// Which camera's view, for multi-camera events; "" is the event's
	// leading camera.  At most 255 bytes.
	Camera string
	// Expiry, whole seconds.
	Expires time.Time
}

type key struct {
	id     [kidLen]byte
	secret []byte
}

// Keyring holds the current key and, during a rotation, the prior one.
type Keyring struct {
	current key
	prior   *key
}

// New builds a Keyring.  current is required; prior may be nil or
// empty (no rotation in progress).  Keys are raw bytes, at least
// MinKeyLen long; see ParseKey for the configured form.
func New(current, prior []byte) (*Keyring, error) {
	if len(current) < MinKeyLen {
		return nil, fmt.Errorf("captoken: current key is %d bytes, need at least %d", len(current), MinKeyLen)
	}
	kr := &Keyring{current: mkKey(current)}
	if len(prior) > 0 {
		if len(prior) < MinKeyLen {
			return nil, fmt.Errorf("captoken: prior key is %d bytes, need at least %d", len(prior), MinKeyLen)
		}
		p := mkKey(prior)
		if p.id == kr.current.id {
			return nil, errors.New("captoken: prior key is the same as current")
		}
		kr.prior = &p
	}
	return kr, nil
}

// ParseKey decodes a configured key: standard base64 of at least
// MinKeyLen random bytes, e.g. `openssl rand -base64 32`.
func ParseKey(s string) ([]byte, error) {
	if s == "" {
		return nil, nil
	}
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("captoken: key is not base64: %w", err)
	}
	if len(b) < MinKeyLen {
		return nil, fmt.Errorf("captoken: key is %d bytes, need at least %d (openssl rand -base64 32)", len(b), MinKeyLen)
	}
	return b, nil
}

// mkKey derives the key id from the secret, so it never needs
// configuring and two instances with the same secret agree on it.
func mkKey(secret []byte) key {
	sum := sha256.Sum256(append([]byte("curtilage/captoken/kid/"), secret...))
	var k key
	copy(k.id[:], sum[:kidLen])
	k.secret = append([]byte(nil), secret...)
	return k
}

// CurrentKeyID identifies the minting key, for logs and /metrics.
func (kr *Keyring) CurrentKeyID() string { return fmt.Sprintf("%x", kr.current.id) }

// HasPrior reports whether a rotation is in progress.
func (kr *Keyring) HasPrior() bool { return kr.prior != nil }

// Mint signs claims with the current key.  Expires is rounded down to
// the second.
func (kr *Keyring) Mint(c Claims) string {
	if len(c.Camera) > 255 {
		c.Camera = "" // cannot be a real camera; the leading one then
	}
	body := make([]byte, 0, kidLen+fixedLen+len(c.Camera)+len(c.EventID)+macLen)
	body = append(body, kr.current.id[:]...)
	body = binary.BigEndian.AppendUint64(body, uint64(c.Expires.Unix()))
	body = append(body, c.Media, byte(len(c.Camera)))
	body = append(body, c.Camera...)
	body = append(body, c.EventID...)
	body = append(body, mac(kr.current.secret, body)...)
	return base64.RawURLEncoding.EncodeToString(body)
}

// Verify checks a token against the ring at now and returns its
// claims.  Signature is checked before expiry, so a forged token
// never learns whether its expiry was plausible.
func (kr *Keyring) Verify(token string, now time.Time) (Claims, error) {
	body, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(body) < kidLen+fixedLen+1+macLen {
		return Claims{}, ErrMalformed
	}
	k := kr.lookup(body[:kidLen])
	if k == nil {
		return Claims{}, ErrUnknownKey
	}
	signed, sig := body[:len(body)-macLen], body[len(body)-macLen:]
	if subtle.ConstantTimeCompare(sig, mac(k.secret, signed)) != 1 {
		return Claims{}, ErrBadMAC
	}
	claims := signed[kidLen:]
	camLen := int(claims[9])
	if len(claims) < fixedLen+camLen+1 {
		return Claims{}, ErrMalformed
	}
	c := Claims{
		Expires: time.Unix(int64(binary.BigEndian.Uint64(claims[:8])), 0).UTC(),
		Media:   claims[8],
		Camera:  string(claims[fixedLen : fixedLen+camLen]),
		EventID: string(claims[fixedLen+camLen:]),
	}
	if !now.Before(c.Expires) {
		return c, ErrExpired
	}
	return c, nil
}

func (kr *Keyring) lookup(id []byte) *key {
	if subtle.ConstantTimeCompare(id, kr.current.id[:]) == 1 {
		return &kr.current
	}
	if kr.prior != nil && subtle.ConstantTimeCompare(id, kr.prior.id[:]) == 1 {
		return kr.prior
	}
	return nil
}

func mac(secret, data []byte) []byte {
	h := hmac.New(sha256.New, secret)
	h.Write(data)
	return h.Sum(nil)[:macLen]
}
