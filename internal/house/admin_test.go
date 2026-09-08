package house

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jeffbstewart/curtilage/internal/devices"
	"github.com/jeffbstewart/curtilage/internal/webauthn"
	"github.com/jeffbstewart/curtilage/internal/webauthn/webauthntest"
)

const adminOrigin = "https://curtilage.example.net"

var adminRP = webauthn.RelyingParty{Origin: adminOrigin, ID: "curtilage.example.net"}

func adminHandler(t *testing.T) *Handler {
	t.Helper()
	h := handler(t)
	dr, err := devices.New("")
	if err != nil {
		t.Fatal(err)
	}
	h.API.Devices = dr
	h.AdminOrigin = adminOrigin
	return h
}

// adminPost fires one admin POST from the allowed subnet.
func adminPost(t *testing.T, h *Handler, path string, body any, session string) (int, string, string) {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/house/admin/"+path, bytes.NewReader(b))
	req.RemoteAddr = "192.168.1.50:1"
	if session != "" {
		req.AddCookie(&http.Cookie{Name: adminCookie, Value: session})
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	newSession := ""
	for _, c := range rec.Result().Cookies() {
		if c.Name == adminCookie {
			newSession = c.Value
		}
	}
	return rec.Code, rec.Body.String(), newSession
}

// challengeFor mints a ceremony challenge and returns its raw bytes.
func challengeFor(t *testing.T, h *Handler, purpose, session string) []byte {
	t.Helper()
	code, body, _ := adminPost(t, h, "challenge", map[string]string{"purpose": purpose}, session)
	if code != 200 {
		t.Fatalf("challenge %s -> %d %s", purpose, code, body)
	}
	var resp struct{ Challenge, RpId string }
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.RpId != adminRP.ID {
		t.Fatalf("rpId %q", resp.RpId)
	}
	ch, err := base64.RawURLEncoding.DecodeString(resp.Challenge)
	if err != nil {
		t.Fatal(err)
	}
	return ch
}

func TestAdminLifecycle(t *testing.T) {
	h := adminHandler(t)

	// Bootstrap: the page offers registration; the first ceremony
	// closes the door.
	if _, body := get(t, h, "192.168.1.50:1", "", "admin/"); !strings.Contains(body, "register admin passkey") {
		t.Fatal("bootstrap page missing")
	}
	key := webauthntest.New("cred-a")
	cdj, att := key.Register(adminRP, challengeFor(t, h, "register", ""))
	code, body, _ := adminPost(t, h, "passkey/register", map[string]string{
		"name": "Jeff's phone", "clientDataJSON": base64.RawURLEncoding.EncodeToString(cdj),
		"attestationObject": base64.RawURLEncoding.EncodeToString(att)}, "")
	if code != 200 {
		t.Fatalf("bootstrap register -> %d %s", code, body)
	}
	if !h.API.Devices.HasPasskeys() {
		t.Fatal("door still open")
	}
	// A second cold registration is refused: the door closed.
	key2 := webauthntest.New("cred-b")
	cdj2, att2 := key2.Register(adminRP, challengeFor(t, h, "register", ""))
	if code, _, _ := adminPost(t, h, "passkey/register", map[string]string{
		"name": "intruder", "clientDataJSON": base64.RawURLEncoding.EncodeToString(cdj2),
		"attestationObject": base64.RawURLEncoding.EncodeToString(att2)}, ""); code != http.StatusForbidden {
		t.Fatalf("cold register after bootstrap -> %d", code)
	}

	// Unlock: assertion opens the session.
	acd, aad, sig := key.Assert(adminRP, challengeFor(t, h, "login", ""))
	code, body, session := adminPost(t, h, "passkey/login", map[string]string{
		"credentialId":      base64.RawURLEncoding.EncodeToString(key.CredID),
		"clientDataJSON":    base64.RawURLEncoding.EncodeToString(acd),
		"authenticatorData": base64.RawURLEncoding.EncodeToString(aad),
		"signature":         base64.RawURLEncoding.EncodeToString(sig)}, "")
	if code != 200 || session == "" {
		t.Fatalf("login -> %d %s (session %q)", code, body, session)
	}
	// A replayed ceremony fails: the challenge was one use.
	if code, _, _ := adminPost(t, h, "passkey/login", map[string]string{
		"credentialId":      base64.RawURLEncoding.EncodeToString(key.CredID),
		"clientDataJSON":    base64.RawURLEncoding.EncodeToString(acd),
		"authenticatorData": base64.RawURLEncoding.EncodeToString(aad),
		"signature":         base64.RawURLEncoding.EncodeToString(sig)}, ""); code != http.StatusForbidden {
		t.Fatalf("replayed login -> %d", code)
	}

	// The authed page nags for a backup until a second key exists.
	req := httptest.NewRequest(http.MethodGet, "/house/admin/", nil)
	req.RemoteAddr = "192.168.1.50:1"
	req.AddCookie(&http.Cookie{Name: adminCookie, Value: session})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "single point of lockout") {
		t.Error("backup nag missing with one key")
	}

	// Inside the session: mint an enrollment secret and prove it
	// enrolls a device.
	code, body, _ = adminPost(t, h, "enroll", nil, session)
	if code != 200 {
		t.Fatalf("enroll -> %d %s", code, body)
	}
	var minted struct{ Secret, URL, QR string }
	json.Unmarshal([]byte(body), &minted)
	// The QR is a real PNG data URI of <origin>/enroll#<secret> --
	// origin so the app learns home, the secret in the fragment so it
	// never rides a request line.
	if minted.URL != adminOrigin+"/enroll#"+minted.Secret {
		t.Fatalf("enroll url: %q", minted.URL)
	}
	png, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(minted.QR, "data:image/png;base64,"))
	if err != nil || len(png) < 8 || string(png[1:4]) != "PNG" {
		t.Fatalf("qr is not a png (%d bytes, %v)", len(png), err)
	}
	dev, _, err := h.API.Devices.Enroll(minted.Secret, "test phone", h.Now())
	if err != nil {
		t.Fatalf("minted secret did not enroll: %v", err)
	}
	// ...and revoke that device through the endpoint.
	if code, body, _ := adminPost(t, h, "device/revoke", map[string]string{"id": dev.ID}, session); code != 200 {
		t.Fatalf("revoke -> %d %s", code, body)
	}

	// The backup key registers inside the session; the nag lifts.
	cdj3, att3 := key2.Register(adminRP, challengeFor(t, h, "register", session))
	if code, body, _ := adminPost(t, h, "passkey/register", map[string]string{
		"name": "drawer yubikey", "clientDataJSON": base64.RawURLEncoding.EncodeToString(cdj3),
		"attestationObject": base64.RawURLEncoding.EncodeToString(att3)}, session); code != 200 {
		t.Fatalf("backup register -> %d %s", code, body)
	}
	pks := h.API.Devices.Passkeys()
	if len(pks) != 2 {
		t.Fatalf("passkeys: %+v", pks)
	}
	// Removing down to one works; removing the last does not.
	if code, _, _ := adminPost(t, h, "passkey/remove", map[string]string{"id": pks[1].ID}, session); code != 200 {
		t.Fatal("remove second key")
	}
	if code, _, _ := adminPost(t, h, "passkey/remove", map[string]string{"id": pks[0].ID}, session); code != http.StatusBadRequest {
		t.Fatal("removed the last key")
	}

	// Lock: the session dies; mutations are refused again.
	if code, _, _ := adminPost(t, h, "session/logout", nil, session); code != 200 {
		t.Fatal("logout")
	}
	if code, _, _ := adminPost(t, h, "enroll", nil, session); code != http.StatusForbidden {
		t.Fatal("session survived logout")
	}
}

func TestAdminUnconfigured(t *testing.T) {
	h := handler(t) // no AdminOrigin, no Devices
	if _, body := get(t, h, "192.168.1.50:1", "", "admin/"); !strings.Contains(body, "not configured") {
		t.Error("unconfigured page")
	}
	if code, _, _ := adminPost(t, h, "challenge", map[string]string{"purpose": "login"}, ""); code != http.StatusNotFound {
		t.Error("challenge on unconfigured handler")
	}
	// And mutations without a session on a CONFIGURED handler refuse.
	ch := adminHandler(t)
	if code, _, _ := adminPost(t, ch, "device/revoke", map[string]string{"id": "x"}, ""); code != http.StatusForbidden {
		t.Error("cold revoke")
	}
}
