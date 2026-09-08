// The admin area, /house/admin/: the passkey-gated room where auth
// state changes (docs/DESIGN.md "gRPC authentication").  A WebAuthn
// assertion opens a short sliding session (cookie); inside it the
// admin mints enrollment secrets, revokes devices, and manages
// passkeys.  Bootstrap: while NO passkey exists, registration is
// open to the (already subnet-gated) LAN; the first key closes that
// door, and the page nags until a BACKUP key exists -- one passkey
// is a single point of lockout.
package house

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	qrcode "github.com/skip2/go-qrcode"

	"github.com/jeffbstewart/curtilage/internal/webauthn"
)

const (
	adminCookie     = "curtilage_admin"
	adminSessionTTL = 15 * time.Minute
	challengeTTL    = 5 * time.Minute
)

type adminState struct {
	sessions   map[string]time.Time // token -> expiry (sliding)
	challenges map[string]challenge // b64url(challenge) -> purpose+expiry
}

type challenge struct {
	purpose string // "register" or "login"
	expires time.Time
}

func (h *Handler) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

// rp is the WebAuthn relying party the config names; zero when the
// admin area is unconfigured.
func (h *Handler) rp() (webauthn.RelyingParty, bool) {
	if h.AdminOrigin == "" || h.API == nil || h.API.Devices == nil {
		return webauthn.RelyingParty{}, false
	}
	u, err := url.Parse(h.AdminOrigin)
	if err != nil || u.Scheme == "" || u.Hostname() == "" {
		return webauthn.RelyingParty{}, false
	}
	return webauthn.RelyingParty{Origin: h.AdminOrigin, ID: u.Hostname()}, true
}

// admin routes everything under /house/admin.
func (h *Handler) admin(w http.ResponseWriter, r *http.Request, rest string) {
	h.adminMu.Lock()
	if h.adminState == nil {
		h.adminState = &adminState{sessions: map[string]time.Time{}, challenges: map[string]challenge{}}
	}
	h.adminMu.Unlock()
	rest = strings.TrimPrefix(rest, "/")
	if r.Method == http.MethodGet && rest == "" {
		h.adminPage(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	switch rest {
	case "challenge":
		h.adminChallenge(w, r)
	case "passkey/register":
		h.adminRegister(w, r)
	case "passkey/login":
		h.adminLogin(w, r)
	case "passkey/remove":
		h.adminSessionOnly(w, r, func(req struct{ ID string }) error {
			return h.API.Devices.RemovePasskey(req.ID, h.now())
		})
	case "device/revoke":
		h.adminSessionOnly(w, r, func(req struct{ ID string }) error {
			if !h.API.Devices.Revoke(req.ID, h.now()) {
				return fmt.Errorf("no such device")
			}
			return nil
		})
	case "enroll":
		if !h.adminSession(w, r) {
			return
		}
		secret := h.API.Devices.MintEnrollment(h.now())
		// The QR carries <origin>/enroll#<secret>: the origin so the
		// app learns where home is, the secret in the FRAGMENT so it
		// never rides a request line into anyone's access log.  The
		// image travels inside this response as a data URI for the
		// same reason.
		enrollURL := h.AdminOrigin + "/enroll#" + secret
		png, err := qrcode.Encode(enrollURL, qrcode.Medium, 256)
		if err != nil {
			log.Printf("house: enroll qr: %v", err)
			http.Error(w, "qr encoding failed", http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]string{"secret": secret, "ttl": "5m", "url": enrollURL,
			"qr": "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)})
	case "session/logout":
		if c, err := r.Cookie(adminCookie); err == nil {
			h.adminMu.Lock()
			delete(h.adminState.sessions, c.Value)
			h.adminMu.Unlock()
		}
		http.SetCookie(w, &http.Cookie{Name: adminCookie, Value: "", Path: "/house/admin",
			MaxAge: -1, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
		writeJSON(w, map[string]bool{"ok": true})
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

// authed reports whether r carries a live admin session, sliding its
// expiry when it does.
func (h *Handler) authed(r *http.Request) bool {
	c, err := r.Cookie(adminCookie)
	if err != nil {
		return false
	}
	h.adminMu.Lock()
	defer h.adminMu.Unlock()
	exp, ok := h.adminState.sessions[c.Value]
	if !ok || h.now().After(exp) {
		delete(h.adminState.sessions, c.Value)
		return false
	}
	h.adminState.sessions[c.Value] = h.now().Add(adminSessionTTL)
	return true
}

// adminSession answers 403 when there is no session.
func (h *Handler) adminSession(w http.ResponseWriter, r *http.Request) bool {
	if _, ok := h.rp(); !ok {
		http.Error(w, "admin area is not configured", http.StatusNotFound)
		return false
	}
	if !h.authed(r) {
		http.Error(w, "an admin session is required", http.StatusForbidden)
		return false
	}
	return true
}

// adminSessionOnly wraps the one-field mutations.
func (h *Handler) adminSessionOnly(w http.ResponseWriter, r *http.Request, do func(struct{ ID string }) error) {
	if !h.adminSession(w, r) {
		return
	}
	var req struct{ ID string }
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	if err := do(req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// adminChallenge mints a one-use ceremony challenge.  For login it
// also names the registered credential ids -- mildly identifying,
// and fine behind the subnet gate.
func (h *Handler) adminChallenge(w http.ResponseWriter, r *http.Request) {
	rp, ok := h.rp()
	if !ok {
		http.Error(w, "admin area is not configured", http.StatusNotFound)
		return
	}
	var req struct{ Purpose string }
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	if req.Purpose != "register" && req.Purpose != "login" {
		http.Error(w, "purpose must be register or login", http.StatusBadRequest)
		return
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	key := base64.RawURLEncoding.EncodeToString(b)
	now := h.now()
	h.adminMu.Lock()
	for k, c := range h.adminState.challenges { // sweep on the way through
		if now.After(c.expires) {
			delete(h.adminState.challenges, k)
		}
	}
	h.adminState.challenges[key] = challenge{purpose: req.Purpose, expires: now.Add(challengeTTL)}
	h.adminMu.Unlock()
	resp := map[string]any{"challenge": key, "rpId": rp.ID}
	if req.Purpose == "login" {
		var ids []string
		for _, p := range h.API.Devices.Passkeys() {
			ids = append(ids, base64.RawURLEncoding.EncodeToString(p.Credential.ID))
		}
		resp["credentialIds"] = ids
	}
	writeJSON(w, resp)
}

// takeChallenge consumes the (one-use) challenge named inside
// clientDataJSON, if it was minted for purpose and still lives.
func (h *Handler) takeChallenge(clientDataJSON []byte, purpose string) ([]byte, error) {
	var cd struct {
		Challenge string `json:"challenge"`
	}
	if err := json.Unmarshal(clientDataJSON, &cd); err != nil {
		return nil, fmt.Errorf("client data: %w", err)
	}
	h.adminMu.Lock()
	defer h.adminMu.Unlock()
	c, ok := h.adminState.challenges[cd.Challenge]
	delete(h.adminState.challenges, cd.Challenge)
	if !ok || c.purpose != purpose || h.now().After(c.expires) {
		return nil, fmt.Errorf("challenge is not live")
	}
	return base64.RawURLEncoding.DecodeString(cd.Challenge)
}

// adminRegister finishes a create() ceremony.  Allowed while NO
// passkey exists (the bootstrap: the subnet gate is the authority) or
// inside a session (adding the backup key, or another admin's).
func (h *Handler) adminRegister(w http.ResponseWriter, r *http.Request) {
	rp, ok := h.rp()
	if !ok {
		http.Error(w, "admin area is not configured", http.StatusNotFound)
		return
	}
	if h.API.Devices.HasPasskeys() && !h.authed(r) {
		http.Error(w, "an admin session is required to add a passkey", http.StatusForbidden)
		return
	}
	var req struct{ Name, ClientDataJSON, AttestationObject string }
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" || len(name) > 64 {
		http.Error(w, "name must be 1-64 characters", http.StatusBadRequest)
		return
	}
	cdj, err1 := base64.RawURLEncoding.DecodeString(req.ClientDataJSON)
	att, err2 := base64.RawURLEncoding.DecodeString(req.AttestationObject)
	if err1 != nil || err2 != nil {
		http.Error(w, "fields must be base64url", http.StatusBadRequest)
		return
	}
	ch, err := h.takeChallenge(cdj, "register")
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	cred, err := rp.VerifyRegistration(cdj, att, ch)
	if err != nil {
		log.Printf("house: passkey register: %v", err)
		http.Error(w, "the ceremony did not verify", http.StatusForbidden)
		return
	}
	if _, err := h.API.Devices.AddPasskey(name, cred, h.now()); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	log.Printf("house: admin passkey %q registered", name)
	writeJSON(w, map[string]bool{"ok": true})
}

// adminLogin finishes a get() ceremony and opens the session.
func (h *Handler) adminLogin(w http.ResponseWriter, r *http.Request) {
	rp, ok := h.rp()
	if !ok {
		http.Error(w, "admin area is not configured", http.StatusNotFound)
		return
	}
	var req struct{ CredentialID, ClientDataJSON, AuthenticatorData, Signature string }
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	credID, e1 := base64.RawURLEncoding.DecodeString(req.CredentialID)
	cdj, e2 := base64.RawURLEncoding.DecodeString(req.ClientDataJSON)
	ad, e3 := base64.RawURLEncoding.DecodeString(req.AuthenticatorData)
	sig, e4 := base64.RawURLEncoding.DecodeString(req.Signature)
	if e1 != nil || e2 != nil || e3 != nil || e4 != nil {
		http.Error(w, "fields must be base64url", http.StatusBadRequest)
		return
	}
	pk, ok := h.API.Devices.PasskeyByCredentialID(credID)
	if !ok {
		http.Error(w, "unknown credential", http.StatusForbidden)
		return
	}
	ch, err := h.takeChallenge(cdj, "login")
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	count, err := rp.VerifyAssertion(pk.Credential, cdj, ad, sig, ch)
	if err != nil {
		log.Printf("house: passkey login: %v", err)
		http.Error(w, "the assertion did not verify", http.StatusForbidden)
		return
	}
	h.API.Devices.SetPasskeySignCount(pk.ID, count, h.now())
	tok := make([]byte, 32)
	if _, err := rand.Read(tok); err != nil {
		panic(err)
	}
	session := base64.RawURLEncoding.EncodeToString(tok)
	h.adminMu.Lock()
	h.adminState.sessions[session] = h.now().Add(adminSessionTTL)
	h.adminMu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: adminCookie, Value: session, Path: "/house/admin",
		Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	log.Printf("house: admin session opened by passkey %q", pk.Name)
	writeJSON(w, map[string]bool{"ok": true})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		http.Error(w, "bad request body", http.StatusBadRequest)
		return err
	}
	return nil
}

// adminPage renders the room in whichever of its states applies.
func (h *Handler) adminPage(w http.ResponseWriter, r *http.Request) {
	_, configured := h.rp()
	p := struct {
		DisplayName string
		Configured  bool
		Bootstrap   bool // no passkeys yet: registration open
		Authed      bool
		BackupNag   bool // exactly one passkey: single point of lockout
		Devices     []deviceRow
		Passkeys    []passkeyRow
		Badge       buildBadge
	}{DisplayName: h.DisplayName, Configured: configured, Badge: h.badge()}
	if configured {
		p.Bootstrap = !h.API.Devices.HasPasskeys()
		p.Authed = h.authed(r)
		pks := h.API.Devices.Passkeys()
		p.BackupNag = len(pks) == 1
		if p.Authed {
			for _, pk := range pks {
				p.Passkeys = append(p.Passkeys, passkeyRow{ID: pk.ID, Name: pk.Name,
					Registered: pk.Registered.In(h.loc()).Format("Jan 2 15:04")})
			}
			for _, d := range h.API.Devices.List() {
				p.Devices = append(p.Devices, deviceRow{ID: d.ID, Name: d.Name, Revoked: d.Revoked,
					Enrolled: d.Enrolled.In(h.loc()).Format("Jan 2 15:04"),
					LastSeen: d.LastSeen.In(h.loc()).Format("Jan 2 15:04")})
			}
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if err := adminTmpl.Execute(w, p); err != nil {
		log.Printf("house: admin render: %v", err)
	}
}

type deviceRow struct {
	ID, Name, Enrolled, LastSeen string
	Revoked                      bool
}

type passkeyRow struct{ ID, Name, Registered string }

var adminTmpl = template.Must(template.New("admin").Parse(`<!doctype html>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.DisplayName}}: admin</title>
<style>
 body { font: 14px/1.4 system-ui, sans-serif; margin: 1.5rem; color: #222; background: #fafafa; max-width: 42rem; }
 h1 { font-size: 1.2rem; margin: 0 0 .25rem; }
 h2 { font-size: 1rem; margin: 1.2rem 0 .3rem; }
 button { font: inherit; padding: .3rem .8rem; }
 table { border-collapse: collapse; }
 td, th { text-align: left; padding: .15rem .8rem .15rem 0; }
 .nag { background: #fff3cd; border: 1px solid #e0c060; padding: .5rem .8rem; border-radius: 6px; margin: .8rem 0; }
 .secret { font-family: ui-monospace, monospace; background: #eee; padding: .3rem .5rem; border-radius: 4px; }
 .dim { color: #888; }
 a { color: #06c; }
</style>
<h1>{{.DisplayName}}: admin</h1>
<div><a href="/house/">back to the house</a></div>
{{if not .Configured}}
<p>The admin area is not configured: set <code>house.admin_origin</code> to the TLS origin this server is fronted as.</p>
{{else if .Bootstrap}}
<p>No admin passkey exists yet.  Registration is open (to this network only) until the first one closes the door.</p>
<p><input id="pkname" placeholder="passkey name (Jeff's phone)"> <button onclick="register()">register admin passkey</button></p>
{{else if not .Authed}}
<p><button onclick="login()">unlock with passkey</button></p>
{{else}}
{{if .BackupNag}}<div class="nag">One passkey is a single point of lockout: register a <b>backup</b> (a hardware key in a drawer outlives phones and accounts).</div>{{end}}
<h2>Enroll a device</h2>
<p><button onclick="enroll()">mint enrollment secret</button></p>
<div id="secretbox"></div>
<p class="dim">One use, five minutes.  Scan the QR from the app's enrollment screen, or type the secret.</p>
<h2>Devices</h2>
{{if .Devices}}<table><tr><th>Name</th><th>Enrolled</th><th>Last seen</th><th></th></tr>
{{range .Devices}}<tr><td>{{.Name}}</td><td>{{.Enrolled}}</td><td>{{.LastSeen}}</td><td>{{if .Revoked}}<span class="dim">revoked</span>{{else}}<button onclick="revoke('{{.ID}}')">revoke</button>{{end}}</td></tr>
{{end}}</table>{{else}}<p class="dim">No devices enrolled.</p>{{end}}
<h2>Admin passkeys</h2>
<table><tr><th>Name</th><th>Registered</th><th></th></tr>
{{range .Passkeys}}<tr><td>{{.Name}}</td><td>{{.Registered}}</td><td><button onclick="removeKey('{{.ID}}')">remove</button></td></tr>
{{end}}</table>
<p><input id="pkname" placeholder="new passkey name"> <button onclick="register()">add passkey</button>
 <button onclick="logout()">lock</button></p>
{{end}}
<script>
function b64d(s){s=s.replace(/-/g,'+').replace(/_/g,'/');const b=atob(s);const a=new Uint8Array(b.length);for(let i=0;i<b.length;i++)a[i]=b.charCodeAt(i);return a.buffer}
function b64e(buf){const a=new Uint8Array(buf);let s='';for(const c of a)s+=String.fromCharCode(c);return btoa(s).replace(/\+/g,'-').replace(/\//g,'_').replace(/=+$/,'')}
async function post(path, body){
  const r = await fetch('/house/admin/'+path, {method:'POST', headers:{'Content-Type':'application/json'}, body: JSON.stringify(body||{})});
  if (!r.ok) throw new Error(await r.text());
  return r.json();
}
async function register(){
  try {
    const name = document.getElementById('pkname').value.trim();
    if (!name) { alert('name the passkey first'); return; }
    const ch = await post('challenge', {purpose:'register'});
    const cred = await navigator.credentials.create({publicKey:{
      challenge: b64d(ch.challenge), rp: {id: ch.rpId, name: 'curtilage'},
      user: {id: crypto.getRandomValues(new Uint8Array(16)), name: name, displayName: name},
      pubKeyCredParams: [{type:'public-key',alg:-7},{type:'public-key',alg:-257}],
      authenticatorSelection: {userVerification:'required', residentKey:'preferred'},
      attestation: 'none'}});
    await post('passkey/register', {name: name,
      clientDataJSON: b64e(cred.response.clientDataJSON),
      attestationObject: b64e(cred.response.attestationObject)});
    location.reload();
  } catch (e) { alert(e); }
}
async function login(){
  try {
    const ch = await post('challenge', {purpose:'login'});
    const cred = await navigator.credentials.get({publicKey:{
      challenge: b64d(ch.challenge), rpId: ch.rpId, userVerification: 'required',
      allowCredentials: (ch.credentialIds||[]).map(id => ({type:'public-key', id: b64d(id)}))}});
    await post('passkey/login', {credentialId: b64e(cred.rawId),
      clientDataJSON: b64e(cred.response.clientDataJSON),
      authenticatorData: b64e(cred.response.authenticatorData),
      signature: b64e(cred.response.signature)});
    location.reload();
  } catch (e) { alert(e); }
}
async function enroll(){
  try {
    const r = await post('enroll');
    document.getElementById('secretbox').innerHTML =
      '<div><img src="'+r.qr+'" alt="enrollment QR" width="256" height="256"></div>' +
      '<span class="secret">'+r.secret+'</span> (one use, '+r.ttl+')';
  } catch (e) { alert(e); }
}
async function revoke(id){ if (confirm('Revoke this device?')) { try { await post('device/revoke', {id}); location.reload(); } catch (e) { alert(e); } } }
async function removeKey(id){ if (confirm('Remove this passkey?')) { try { await post('passkey/remove', {id}); location.reload(); } catch (e) { alert(e); } } }
async function logout(){ try { await post('session/logout'); location.reload(); } catch (e) { alert(e); } }
</script>
`))
