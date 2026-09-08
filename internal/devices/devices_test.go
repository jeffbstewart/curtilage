package devices

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEnrollAuthenticateRevoke(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.textproto")
	r, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 9, 7, 22, 0, 0, 0, time.UTC)
	// Unarmed while empty: the yes-to-everything rule lives in the
	// interceptor (server/auth.go), keyed on Armed; the registry
	// itself never vouches for a token it does not know.
	if r.Armed() {
		t.Fatal("armed while empty")
	}
	if _, ok := r.Authenticate("anything", t0); ok {
		t.Fatal("unknown token accepted")
	}

	secret := r.MintEnrollment(t0)
	d, token, err := r.Enroll(secret, "Jeff's iPhone", t0)
	if err != nil || token == "" || d.Name != "Jeff's iPhone" {
		t.Fatalf("enroll: %+v %q %v", d, token, err)
	}
	if !r.Armed() {
		t.Fatal("not armed after enrollment")
	}
	if got, ok := r.Authenticate(token, t0.Add(time.Minute)); !ok || got.ID != d.ID {
		t.Fatalf("authenticate: %+v %v", got, ok)
	}
	if _, ok := r.Authenticate("wrong-token", t0); ok {
		t.Fatal("wrong token accepted while armed")
	}
	// The secret was one use.
	if _, _, err := r.Enroll(secret, "again", t0); err == nil {
		t.Fatal("secret worked twice")
	}
	// An expired secret is dead too.
	stale := r.MintEnrollment(t0)
	if _, _, err := r.Enroll(stale, "late", t0.Add(enrollTTL+time.Second)); err == nil {
		t.Fatal("expired secret worked")
	}

	// Persistence: a reload knows the device and its hash, not the token.
	r2, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	if !r2.Armed() {
		t.Fatal("reload lost the device")
	}
	if got, ok := r2.Authenticate(token, t0); !ok || got.ID != d.ID {
		t.Fatalf("reload authenticate: %+v %v", got, ok)
	}
	// The file never holds the token itself.
	b, _ := readFile(t, path)
	if strings.Contains(b, token) {
		t.Fatal("the bearer token is written to disk")
	}

	// Revoke ends it, and survives reload.
	if !r2.Revoke(d.ID, t0) || r2.Revoke("nope", t0) {
		t.Fatal("revoke bookkeeping")
	}
	if _, ok := r2.Authenticate(token, t0); ok {
		t.Fatal("revoked token accepted")
	}
	if r2.Armed() {
		t.Fatal("armed with every device revoked")
	}
	r3, _ := New(path)
	if r3.Armed() {
		t.Fatal("revocation did not survive reload")
	}
	if list := r3.List(); len(list) != 1 || !list[0].Revoked {
		t.Fatalf("list: %+v", list)
	}
}

// Enrollment secrets have a horizon and cannot accumulate: expired
// ones are swept on every mint and every enrollment attempt, and the
// live set is capped -- past the cap, minting evicts the oldest.
func TestEnrollmentSecretsCannotAccumulate(t *testing.T) {
	r, _ := New("")
	t0 := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	stale := r.MintEnrollment(t0)
	if n := len(r.pending); n != 1 {
		t.Fatalf("pending: %d", n)
	}
	// Long after expiry, ANY enrollment attempt sweeps it out.
	if _, _, err := r.Enroll("wrong", "x", t0.Add(time.Hour)); err == nil {
		t.Fatal("bogus secret enrolled")
	}
	if n := len(r.pending); n != 0 {
		t.Fatalf("expired secret survived the sweep: %d pending", n)
	}
	if _, _, err := r.Enroll(stale, "late", t0.Add(time.Hour)); err == nil {
		t.Fatal("swept secret still worked")
	}
	// The live set is capped: minting past it evicts the oldest, and
	// the newest cap's worth still work.
	var secrets []string
	for i := range maxPending + 3 {
		secrets = append(secrets, r.MintEnrollment(t0.Add(time.Duration(i)*time.Second)))
	}
	if n := len(r.pending); n != maxPending {
		t.Fatalf("pending after burst: %d, cap %d", n, maxPending)
	}
	if _, _, err := r.Enroll(secrets[0], "evicted", t0.Add(time.Minute)); err == nil {
		t.Fatal("evicted secret worked")
	}
	if _, _, err := r.Enroll(secrets[len(secrets)-1], "newest", t0.Add(time.Minute)); err != nil {
		t.Fatalf("newest secret: %v", err)
	}
}

func TestForget(t *testing.T) {
	r, _ := New("")
	t0 := time.Date(2026, 9, 7, 22, 0, 0, 0, time.UTC)
	_, token, err := r.Enroll(r.MintEnrollment(t0), "phone", t0)
	if err != nil {
		t.Fatal(err)
	}
	r.Forget("not-a-token", t0) // quiet no-op
	r.Forget(token, t0)
	if r.Armed() {
		t.Fatal("armed after the only device forgot itself")
	}
	r.Forget(token, t0) // twice: still quiet
}

func readFile(t *testing.T, path string) (string, error) {
	t.Helper()
	b, err := os.ReadFile(path)
	return string(b), err
}
