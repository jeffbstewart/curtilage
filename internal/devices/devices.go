// Package devices is the enrolled-device registry (docs/DESIGN.md
// "gRPC authentication"): who may call the API, minted at QR
// enrollment, revocable one at a time, persisted as prototext beside
// the recordings so an operator can read it and -- in an emergency --
// revoke with an editor.
//
// The registry also holds the one-use enrollment secrets the admin's
// passkey session mints: short-lived, consumed by the first Enroll
// that presents them.
//
// Auth ARMS when the first device enrolls: an empty registry answers
// every Authenticate with yes, so a fresh install keeps working until
// the household actually enrolls something -- the same
// first-registration-closes-the-door shape as the admin passkey
// bootstrap.
package devices

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"sync"
	"time"

	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/types/known/timestamppb"

	curtilagev1 "github.com/jeffbstewart/curtilage/gen/curtilage/v1"
)

// enrollTTL is how long a minted enrollment secret lives: long enough
// to walk a phone to the QR, no longer.
const enrollTTL = 5 * time.Minute

// lastSeenFlush is how stale the persisted last-seen may be: seeing a
// device is not worth a disk write per call.
const lastSeenFlush = 10 * time.Minute

// Device is one enrolled device, for display and revocation.
type Device struct {
	ID, Name           string
	Enrolled, LastSeen time.Time
	Revoked            bool
}

type pending struct {
	expires time.Time
}

// Registry is the store.  Safe for concurrent use.
type Registry struct {
	mu      sync.Mutex
	path    string                     // "" persists nothing (replay, tests)
	order   []*curtilagev1.DeviceState // file order, stable
	pending map[string]pending         // secret -> expiry
	saved   time.Time
}

// New loads the registry at path, which need not exist yet; "" is a
// memory-only registry.
func New(path string) (*Registry, error) {
	r := &Registry{path: path, pending: map[string]pending{}}
	if path == "" {
		return r, nil
	}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return r, nil
	}
	if err != nil {
		return nil, err
	}
	var st curtilagev1.RegistryState
	if err := prototext.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("devices: %s: %w", path, err)
	}
	for _, d := range st.Devices {
		r.order = append(r.order, d)
	}
	return r, nil
}

// Armed reports whether authentication is enforced: any unrevoked
// device exists.
func (r *Registry) Armed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, d := range r.order {
		if !d.Revoked {
			return true
		}
	}
	return false
}

// MintEnrollment creates a one-use enrollment secret, valid briefly.
// The admin's passkey session calls this; the secret rides the QR.
func (r *Registry) MintEnrollment(now time.Time) string {
	secret := randToken(16)
	r.mu.Lock()
	defer r.mu.Unlock()
	for s, p := range r.pending { // sweep the stale on the way through
		if now.After(p.expires) {
			delete(r.pending, s)
		}
	}
	r.pending[secret] = pending{expires: now.Add(enrollTTL)}
	return secret
}

// Enroll consumes a secret and mints the device's bearer token --
// returned exactly once, stored only as a hash.
func (r *Registry) Enroll(secret, name string, now time.Time) (Device, string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.pending[secret]
	if !ok || now.After(p.expires) {
		return Device{}, "", fmt.Errorf("devices: enrollment secret is not valid")
	}
	delete(r.pending, secret) // one use, even on later failure
	token := randToken(32)
	d := &curtilagev1.DeviceState{
		Id:          randToken(6),
		Name:        name,
		TokenSha256: HashToken(token),
		EnrolledAt:  timestamppb.New(now),
		LastSeen:    timestamppb.New(now),
	}
	r.order = append(r.order, d)
	if err := r.save(now); err != nil {
		return Device{}, "", err
	}
	return device(d), token, nil
}

// Authenticate reports whether token belongs to an unrevoked device,
// and remembers roughly when it was last seen.
func (r *Registry) Authenticate(token string, now time.Time) (Device, bool) {
	h := HashToken(token)
	r.mu.Lock()
	defer r.mu.Unlock()
	d := r.lookup(h)
	if d == nil || d.Revoked {
		return Device{}, false
	}
	d.LastSeen = timestamppb.New(now)
	if now.Sub(r.saved) > lastSeenFlush {
		r.save(now) // best effort; last-seen is not worth failing a call
	}
	return device(d), true
}

// Forget revokes the device presenting token: self-service
// unenrollment.  The caller was already authenticated; an unknown or
// already revoked token (an unarmed registry's pass-through) is a
// quiet no-op.
func (r *Registry) Forget(token string, now time.Time) {
	h := HashToken(token)
	r.mu.Lock()
	defer r.mu.Unlock()
	if d := r.lookup(h); d != nil && !d.Revoked {
		d.Revoked = true
		r.save(now)
	}
}

// Revoke marks one device revoked by id; false when unknown.
func (r *Registry) Revoke(id string, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, d := range r.order {
		if d.Id == id && !d.Revoked {
			d.Revoked = true
			r.save(now)
			return true
		}
	}
	return false
}

// List is every device, enrollment order, revoked included.
func (r *Registry) List() []Device {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Device, 0, len(r.order))
	for _, d := range r.order {
		out = append(out, device(d))
	}
	return out
}

// lookup is constant-time over the hash so a token probe learns
// nothing from timing.
func (r *Registry) lookup(hash string) *curtilagev1.DeviceState {
	var found *curtilagev1.DeviceState
	for _, d := range r.order {
		if subtle.ConstantTimeCompare([]byte(d.TokenSha256), []byte(hash)) == 1 {
			found = d
		}
	}
	return found
}

// save writes the file atomically (temp + rename); callers hold mu.
func (r *Registry) save(now time.Time) error {
	r.saved = now
	if r.path == "" {
		return nil
	}
	st := &curtilagev1.RegistryState{Devices: r.order}
	b, err := prototext.MarshalOptions{Multiline: true}.Marshal(st)
	if err != nil {
		return err
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, r.path)
}

func device(d *curtilagev1.DeviceState) Device {
	return Device{ID: d.Id, Name: d.Name, Enrolled: d.EnrolledAt.AsTime(),
		LastSeen: d.LastSeen.AsTime(), Revoked: d.Revoked}
}

func randToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // the platform's CSPRNG failing is not a condition to limp past
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// HashToken is the hex SHA-256 a token is stored and named by:
// what the registry file holds, and what ForgetRequest presents.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
