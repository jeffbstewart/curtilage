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
	st.Apply(now, policy.Change{Op: policy.OpEnded, Event: ev("arr-new", policy.KindArrival, 2*time.Hour, true)})
	st.Apply(now, policy.Change{Op: policy.OpEnded, Event: ev("pkg-old", policy.KindPackage, 30*time.Hour, false)})
	live := ev("live", policy.KindArrival, 5*time.Minute, true)
	live.EndedAt = time.Time{}
	st.Apply(now, policy.Change{Op: policy.OpStarted, Event: live})
	return &Handler{
		Store: st, API: &server.Server{Store: st, Keys: kr, LinkTTL: time.Hour},
		Allow: allow, Proxies: proxies, DisplayName: "test house", Now: func() time.Time { return now },
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
		{"198.51.100.9:1234", "192.168.1.50, 198.51.100.9", true},   // proxy listed itself
		{"198.51.100.9:1234", "", true},                          // the proxy itself (in allow)
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
	for _, want := range []string{"3 events", "1 still running", "1</b> not sent are hidden", "src-arr-new", "src-live", "household: 2", "nobody (list only): 1", "/media/", `<span class="live">live</span>`} {
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
	_, body = get(t, h, "192.168.1.50:1", "", "?hours=48&view=all")
	if !strings.Contains(body, "src-pkg-old") || !strings.Contains(body, "last 48 hours") {
		t.Error("48h window lacks the 30h-old package")
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
