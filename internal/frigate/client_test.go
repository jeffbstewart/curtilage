package frigate

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func frigateStub(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/events/1788049526.709964-hbpbil/snapshot.jpg":
			w.Header().Set("Content-Type", "image/jpeg")
			w.Write([]byte("\xff\xd8jpeg-bytes"))
		case "/api/events/html-1/snapshot.jpg":
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte("<html>login</html>"))
		case "/api/events/boom-1/snapshot.jpg":
			http.Error(w, "nope", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestSnapshot(t *testing.T) {
	srv := frigateStub(t)
	defer srv.Close()
	c, err := NewClient(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	m, err := c.Snapshot(ctx, "1788049526.709964-hbpbil")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Body.Close()
	b, _ := io.ReadAll(m.Body)
	if m.ContentType != "image/jpeg" || m.Size != int64(len(b)) || string(b[:2]) != "\xff\xd8" {
		t.Errorf("snapshot = %+v, %d bytes", m, len(b))
	}
	if _, err := c.Snapshot(ctx, "1788049526.709964-zzzzzz"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing -> %v", err)
	}
	if _, err := c.Snapshot(ctx, "html-1"); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("wrong content type -> %v", err)
	}
	if _, err := c.Snapshot(ctx, "boom-1"); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("500 -> %v", err)
	}
	// Ids that are not ids never become URLs.
	for _, bad := range []string{"", "..", "a/b", "../../admin", "x y", "id?x=1", "\x00"} {
		if _, err := c.Snapshot(ctx, bad); !errors.Is(err, ErrNotFound) {
			t.Errorf("Snapshot(%q) -> %v, want ErrNotFound", bad, err)
		}
	}
}

func TestClip(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if r.URL.Path == "/api/porch-north/start/1788116840/end/1788116990/clip.mp4" {
			w.Header().Set("Content-Type", "video/mp4")
			w.Write([]byte("mp4-bytes"))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	c, _ := NewClient(srv.URL)
	ctx := context.Background()
	start, end := time.Unix(1788116840, 0), time.Unix(1788116990, 0)
	m, err := c.Clip(ctx, "porch-north", start, end)
	if err != nil {
		t.Fatalf("%v (path %s)", err, gotPath)
	}
	b, _ := io.ReadAll(m.Body)
	m.Body.Close()
	if m.ContentType != "video/mp4" || string(b) != "mp4-bytes" {
		t.Errorf("clip = %+v %q", m, b)
	}
	if _, err := c.Clip(ctx, "nope", start, end); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown camera -> %v", err)
	}
	if _, err := c.Clip(ctx, "porch/../admin", start, end); !errors.Is(err, ErrNotFound) {
		t.Errorf("hostile camera -> %v", err)
	}
	if _, err := c.Clip(ctx, "porch-north", end, start); !errors.Is(err, ErrNotFound) {
		t.Errorf("inverted range -> %v", err)
	}
}

func TestNewClientValidatesURL(t *testing.T) {
	for _, bad := range []string{"", "frigate:5000", "ftp://x", "http://", "://"} {
		if _, err := NewClient(bad); err == nil {
			t.Errorf("NewClient(%q) accepted", bad)
		}
	}
	c, err := NewClient("http://frigate:5000/prefix/?q=1#f")
	if err != nil || c.base.String() != "http://frigate:5000/prefix" {
		t.Errorf("NewClient normalised to %v, %v", c.base, err)
	}
}
