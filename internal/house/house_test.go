package house

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jeffbstewart/curtilage/internal/captoken"
	"github.com/jeffbstewart/curtilage/internal/config"
	"github.com/jeffbstewart/curtilage/internal/policy"
	"github.com/jeffbstewart/curtilage/internal/server"
	"github.com/jeffbstewart/curtilage/internal/store"
)

func handler(t *testing.T) *Handler {
	t.Helper()
	allow, err := config.ParseCIDRs([]string{"192.168.1.0/24", "198.51.100.0/24"})
	if err != nil {
		t.Fatal(err)
	}
	proxies, _ := config.ParseCIDRs([]string{"198.51.100.9"})
	kr, _ := captoken.New(bytes.Repeat([]byte{1}, captoken.MinKeyLen), nil)
	st := store.New(7 * 24 * time.Hour)
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	ev := func(id string, kind policy.Kind, age time.Duration, snap bool) policy.Event {
		return policy.Event{ID: id, Camera: "porch-east", Label: "car", Kind: kind, StartedAt: now.Add(-age),
			EndedAt: now.Add(-age).Add(time.Minute), HasSnapshot: snap, SourceID: "src-" + id, Zones: []string{"pad"}}
	}
	st.Apply(now, policy.Change{Op: policy.OpEnded, Event: ev("det-new", policy.KindDetection, time.Hour, true)})
	unzoned := ev("det-unzoned", policy.KindDetection, time.Hour, true)
	unzoned.Zones = nil
	st.Apply(now, policy.Change{Op: policy.OpEnded, Event: unzoned})
	st.Apply(now, policy.Change{Op: policy.OpEnded, Event: ev("arr-new", policy.KindArrival, 2*time.Hour, true)})
	st.Apply(now, policy.Change{Op: policy.OpEnded, Event: ev("pkg-old", policy.KindPackage, 30*time.Hour, false)})
	live := ev("live", policy.KindArrival, 5*time.Minute, true)
	live.EndedAt = time.Time{}
	st.Apply(now, policy.Change{Op: policy.OpStarted, Event: live})
	// An activity that evolved: three sentences, one repeated.
	act := policy.Event{ID: "walk", Kind: policy.KindActivity, Camera: "porch-west", Cameras: []string{"porch-west"},
		Label: "person", Objects: map[string]int{"person": 1}, Path: []string{"porch"}, Zones: []string{"porch"},
		StartedAt: now.Add(-10 * time.Minute), HasSnapshot: true, SourceID: "src-walk-1", SourceIDs: []string{"src-walk-1"}}
	st.Apply(now.Add(-10*time.Minute), policy.Change{Op: policy.OpStarted, Event: act})
	act.Cameras = append(act.Cameras, "porch-east")
	act.SourceIDs = append(act.SourceIDs, "src-walk-2")
	st.Apply(now.Add(-9*time.Minute), policy.Change{Op: policy.OpUpdated, Event: act}) // same sentence: folded
	act.Objects = map[string]int{"person": 1, "dog": 1}
	st.Apply(now.Add(-8*time.Minute), policy.Change{Op: policy.OpUpdated, Event: act})
	act.Path, act.Zones = []string{"porch", "yard"}, []string{"porch", "yard"}
	act.EndedAt = now.Add(-7 * time.Minute)
	st.Apply(now.Add(-7*time.Minute), policy.Change{Op: policy.OpEnded, Event: act})
	// A sighting: no zone at all, still household news.
	bear := policy.Event{ID: "bear", Camera: "backyard-gate", Label: "bear", Kind: policy.KindSighting,
		StartedAt: now.Add(-20 * time.Minute), EndedAt: now.Add(-19 * time.Minute), HasSnapshot: true, SourceID: "src-bear"}
	st.Apply(now, policy.Change{Op: policy.OpEnded, Event: bear})
	occ := policy.NewOccupancy([]policy.Watch{{Zone: "side_parking"}})
	sts := policy.NewStates([]policy.StateModel{{Model: "bmw_garage_door", Hold: 20 * time.Second}})
	sts.Observe(now.Add(-2*time.Hour), "frigate/garage/classification/bmw_garage_door", []byte("gmw_garage_door_closed"))
	sts.Observe(now.Add(-time.Minute), "frigate/garage/classification/bmw_garage_door", []byte("bmw_garage_door_open"))
	sts.Observe(now.Add(-30*time.Second), "frigate/garage/status/detect", []byte("ON")) // believed after the hold
	return &Handler{
		Store: st, API: &server.Server{Store: st, Keys: kr, LinkTTL: time.Hour},
		Allow: allow, Proxies: proxies, DisplayName: "test house", Now: func() time.Time { return now },
		Version: strings.Repeat("ab12", 10), PR: "20", Built: "2026-08-31T01:00:00Z",
		Occupancy: occ, States: sts,
	}
}

func get(t *testing.T, h *Handler, remote, xff, query string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/house/"+query, nil)
	req.RemoteAddr = remote
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body, _ := io.ReadAll(rec.Result().Body)
	return rec.Code, string(body)
}

func TestCallerAndAllow(t *testing.T) {
	h := handler(t)
	cases := []struct {
		remote, xff string
		want        bool
	}{
		{"192.168.1.50:1234", "", true},
		{"198.51.100.26:1234", "", true},
		{"203.0.113.7:1234", "", false},
		// Header from an untrusted peer is ignored.
		{"203.0.113.7:1234", "192.168.1.50", false},
		{"192.168.1.50:1234", "203.0.113.7", true},
		// Trusted proxy: rightmost non-proxy address is the caller.
		{"198.51.100.9:1234", "192.168.1.50", true},
		{"198.51.100.9:1234", "203.0.113.7", false},
		{"198.51.100.9:1234", "203.0.113.7, 192.168.1.50", true},  // LAN client via proxy, forged prefix
		{"198.51.100.9:1234", "192.168.1.50, 203.0.113.7", false}, // outsider via proxy, forged prefix
		{"198.51.100.9:1234", "192.168.1.50, 198.51.100.9", true}, // proxy listed itself
		{"198.51.100.9:1234", "", true},                           // the proxy itself (in allow)
		{"198.51.100.9:1234", "garbage", false},
		{"nonsense", "", false},
	}
	for _, c := range cases {
		code, _ := get(t, h, c.remote, c.xff, "")
		if got := code == 200; got != c.want {
			t.Errorf("remote=%s xff=%q -> %d, want allowed=%v", c.remote, c.xff, code, c.want)
		}
	}
	// No allow list: nobody, not even loopback.
	h.Allow = nil
	if code, _ := get(t, h, "127.0.0.1:1", "", ""); code != 404 {
		t.Errorf("no allow list -> %d", code)
	}
}

func TestPageViews(t *testing.T) {
	h := handler(t)
	code, body := get(t, h, "192.168.1.50:1", "", "")
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	// Default: sent only. Window 24h: pkg-old (30h) is out.
	for _, want := range []string{"6 events", "1 still running", "2</b> unsent or unzoned are hidden", "src-arr-new", "src-live", "household: 4", "nobody (list only): 2", "/media/", `<span class="live">live</span>`,
		// The unzoned bear: a sighting is shown, zone or no zone.
		"src-bear", "A bear sighted (backyard-gate)",
		// The activity: its final sentence, then how it evolved, newest
		// first, with the no-change revision folded away.
		`<b><a class="ev" href="/house/event/walk">Person and a dog started on the porch, moved to the yard (3m0s)</a></b>`,
		"<li>11:52:00  Person and a dog on the porch</li><li>11:50:00  Person on the porch</li>",
		"porch-west, porch-east", "2 objects src-walk-1",
		`<a href="/media/`,
		`[multi-camera view]`,
		"<b>State</b>bmw garage door: open since Sun 11:59", // handler renders UTC here
		// The build badge: shortened, linked, with the build time.
		`<a href="https://github.com/jeffbstewart/curtilage/pull/20">PR #20</a> <a href="https://github.com/jeffbstewart/curtilage/commit/ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12">ab12ab12a</a> built 2026-08-31T01:00:00Z`} {
		if !strings.Contains(body, want) {
			t.Errorf("sent view lacks %q", want)
		}
	}
	for _, no := range []string{"src-det-new", "src-pkg-old"} {
		if strings.Contains(body, no) {
			t.Errorf("sent view shows %q", no)
		}
	}
	_, body = get(t, h, "192.168.1.50:1", "", "?view=all")
	if !strings.Contains(body, "src-det-new") || !strings.Contains(body, "Showing <b>everything</b>") {
		t.Error("all view lacks the unsent detection")
	}
	// No zone, no interest: elided even from the everything view.
	if strings.Contains(body, "src-det-unzoned") || !strings.Contains(body, "(1 unzoned hidden)") {
		t.Error("unzoned event not elided from the all view")
	}
	_, body = get(t, h, "192.168.1.50:1", "", "?hours=48&view=all")
	if !strings.Contains(body, "src-pkg-old") || !strings.Contains(body, "last 48 hours") {
		t.Error("48h window lacks the 30h-old package")
	}
	// Times render in the household's zone: 12:00Z is 08:00 in New York
	// on that date, and the zone is named.
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	h.Location = ny
	_, body = get(t, h, "192.168.1.50:1", "", "?view=all")
	if !strings.Contains(body, "(America/New_York)") || !strings.Contains(body, "Sun 07:55:00") { // "live" started 5m before noon UTC
		t.Errorf("times not in America/New_York:\n%s", body)
	}
	h.Location = nil

	// The event page: sentence, one video per camera, the spans JSON,
	// and the same 404s for strangers and unknown ids.
	code, body = get(t, h, "192.168.1.50:1", "", "event/walk")
	if code != 200 {
		t.Fatalf("event page -> %d", code)
	}
	for _, want := range []string{"Person and a dog started on the porch", "porch-west", "porch-east", "<video", "const spans", "red outline",
		`id="tabfollow" class="on"`, `id="tabgrid"`, `<body class="follow">`,
		"const windowDur =  190 ;"} { // html/template's JS context pads the number
		if !strings.Contains(body, want) {
			t.Errorf("event page lacks %q", want)
		}
	}
	if n := strings.Count(body, "<video"); n != 2 {
		t.Errorf("%d videos, want one per camera (2)", n)
	}
	if code, _ = get(t, h, "192.168.1.50:1", "", "event/nope"); code != 404 {
		t.Errorf("unknown event -> %d", code)
	}
	if code, _ = get(t, h, "203.0.113.7:1", "", "event/walk"); code != 404 {
		t.Errorf("outsider on event page -> %d", code)
	}

	// Window is capped at retention.
	_, body = get(t, h, "192.168.1.50:1", "", "?hours=9999")
	if !strings.Contains(body, "last 168 hours") {
		t.Error("window not capped to retention")
	}
	// Thumbnails are absent without keys.
	h.API.Keys = nil
	_, body = get(t, h, "192.168.1.50:1", "", "")
	if strings.Contains(body, "/media/") {
		t.Error("links minted without keys")
	}
	if code, _ := get(t, h, "192.168.1.50:1", "", ""); code != 200 {
		t.Error("page needs keys")
	}
}
