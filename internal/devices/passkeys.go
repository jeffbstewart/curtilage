// The admin passkeys, sharing the device registry's file: WebAuthn
// credentials that open the admin session (docs/DESIGN.md "gRPC
// authentication").  Bootstrap rule: registration is open only while
// NO passkey exists; the first one closes that door -- the admin
// area enforces it via HasPasskeys.
package devices

import (
	"bytes"
	"fmt"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	curtilagev1 "github.com/jeffbstewart/curtilage/gen/curtilage/v1"
	"github.com/jeffbstewart/curtilage/internal/webauthn"
)

// Passkey is one registered admin credential.
type Passkey struct {
	ID, Name   string
	Registered time.Time
	Credential webauthn.Credential
}

// HasPasskeys reports whether any admin passkey is registered: the
// bootstrap door is open exactly while this is false.
func (r *Registry) HasPasskeys() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.passkeys) > 0
}

// AddPasskey registers one admin credential.
func (r *Registry) AddPasskey(name string, cred webauthn.Credential, now time.Time) (Passkey, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.passkeys {
		if bytes.Equal(p.CredentialId, cred.ID) {
			return Passkey{}, fmt.Errorf("devices: that passkey is already registered")
		}
	}
	p := &curtilagev1.PasskeyState{
		Id:           randToken(6),
		Name:         name,
		CredentialId: cred.ID,
		PublicKey:    cred.COSE,
		SignCount:    cred.SignCount,
		RegisteredAt: timestamppb.New(now),
	}
	r.passkeys = append(r.passkeys, p)
	if err := r.save(now); err != nil {
		return Passkey{}, err
	}
	return passkey(p)
}

// PasskeyByCredentialID finds the stored credential an assertion
// names.
func (r *Registry) PasskeyByCredentialID(credID []byte) (Passkey, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.passkeys {
		if bytes.Equal(p.CredentialId, credID) {
			pk, err := passkey(p)
			return pk, err == nil
		}
	}
	return Passkey{}, false
}

// SetPasskeySignCount records the authenticator's counter after a
// verified assertion.
func (r *Registry) SetPasskeySignCount(id string, count uint32, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.passkeys {
		if p.Id == id {
			p.SignCount = count
			r.save(now)
			return
		}
	}
}

// Passkeys lists every registered admin credential, oldest first.
func (r *Registry) Passkeys() []Passkey {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Passkey, 0, len(r.passkeys))
	for _, p := range r.passkeys {
		if pk, err := passkey(p); err == nil {
			out = append(out, pk)
		}
	}
	return out
}

// RemovePasskey deletes one admin credential; the LAST one cannot be
// removed -- lock the door, keep a key.
func (r *Registry) RemovePasskey(id string, now time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, p := range r.passkeys {
		if p.Id != id {
			continue
		}
		if len(r.passkeys) == 1 {
			return fmt.Errorf("devices: the last passkey cannot be removed")
		}
		r.passkeys = append(r.passkeys[:i:i], r.passkeys[i+1:]...)
		return r.save(now)
	}
	return fmt.Errorf("devices: no such passkey")
}

func passkey(p *curtilagev1.PasskeyState) (Passkey, error) {
	alg, key, err := webauthn.ParseCOSEKey(p.PublicKey)
	if err != nil {
		return Passkey{}, err
	}
	return Passkey{ID: p.Id, Name: p.Name, Registered: p.RegisteredAt.AsTime(),
		Credential: webauthn.Credential{ID: p.CredentialId, Alg: alg, Key: key, COSE: p.PublicKey, SignCount: p.SignCount}}, nil
}
