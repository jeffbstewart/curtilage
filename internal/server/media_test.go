package server

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	curtilagev1 "github.com/jeffbstewart/curtilage/gen/curtilage/v1"
	"github.com/jeffbstewart/curtilage/internal/captoken"
	"github.com/jeffbstewart/curtilage/internal/frigate"
	"github.com/jeffbstewart/curtilage/internal/policy"
	"github.com/jeffbstewart/curtilage/internal/store"
)

var jpeg = append([]byte("\xff\xd8"), bytes.Repeat([]byte("x"), 3*chunkSize+17)...)

// mediaServer is a Server with a stub Frigate and two events: one
// with a snapshot, one without.
func mediaServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	fr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/events/src-with/snapshot.jpg" {
			w.Header().Set("Content-Type", "image/jpeg")
			w.Header().Set("Content-Length", strconv.Itoa(len(jpeg)))
			w.Write(jpeg)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/cam-a/start/") && strings.HasSuffix(r.URL.Path, "/clip.mp4") {
			w.Header().Set("Content-Type", "video/mp4")
			w.Write([]byte("mp4" + r.URL.Path))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(fr.Close)
	fc, err := frigate.NewClient(fr.URL)
	if err != nil {
		t.Fatal(err)
	}
	kr, err := captoken.New(bytes.Repeat([]byte{1}, captoken.MinKeyLen), nil)
	if err != nil {
		t.Fatal(err)
	}
	st := store.New(time.Hour)
	now := time.Now()
	st.Apply(now, policy.Change{Op: policy.OpStarted, Event: policy.Event{ID: "with", Camera: "cam-a", Label: "car", Kind: policy.KindDetection, StartedAt: now, HasSnapshot: true, SourceID: "src-with"}})
	st.Apply(now, policy.Change{Op: policy.OpStarted, Event: policy.Event{ID: "without", Camera: "cam-a", Label: "car", Kind: policy.KindDetection, StartedAt: now, SourceID: "src-without"}})
	return &Server{Version: "test", Store: st, Frigate: fc, Keys: kr, LinkTTL: time.Hour}, fr
}

func TestGetMediaStreamsSnapshot(t *testing.T) {
	s, _ := mediaServer(t)
	c := client(t, s)
	ctx := context.Background()
	stream, err := c.GetMedia(ctx, &curtilagev1.GetMediaRequest{EventId: "with", Media: curtilagev1.Media_MEDIA_SNAPSHOT})
	if err != nil {
		t.Fatal(err)
	}
	first, err := stream.Recv()
	if err != nil || first.GetInfo() == nil || first.GetInfo().GetContentType() != "image/jpeg" || first.GetInfo().GetSize() != uint64(len(jpeg)) {
		t.Fatalf("first message = %v, %v", first, err)
	}
	var got []byte
	chunks := 0
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if msg.GetInfo() != nil {
			t.Fatal("info after the first message")
		}
		if len(msg.GetChunk()) > chunkSize {
			t.Fatalf("chunk of %d bytes exceeds %d", len(msg.GetChunk()), chunkSize)
		}
		got = append(got, msg.GetChunk()...)
		chunks++
	}
	// Chunk count depends on how the socket delivers the body (short
	// reads are normal); at least four are needed for this size.
	if !bytes.Equal(got, jpeg) || chunks < 4 {
		t.Errorf("got %d bytes in %d chunks, want %d in >= 4", len(got), chunks, len(jpeg))
	}

	for name, req := range map[string]*curtilagev1.GetMediaRequest{
		"unknown event":      {EventId: "nope", Media: curtilagev1.Media_MEDIA_SNAPSHOT},
		"event w/o snapshot": {EventId: "without", Media: curtilagev1.Media_MEDIA_SNAPSHOT},
	} {
		stream, err := c.GetMedia(ctx, req)
		if err == nil {
			_, err = stream.Recv()
		}
		if status.Code(err) != codes.NotFound {
			t.Errorf("%s -> %v, want NotFound", name, err)
		}
	}
	stream, _ = c.GetMedia(ctx, &curtilagev1.GetMediaRequest{EventId: "with"})
	if _, err := stream.Recv(); status.Code(err) != codes.InvalidArgument {
		t.Errorf("MEDIA_UNKNOWN -> %v, want InvalidArgument", err)
	}
}

func TestGetMediaClip(t *testing.T) {
	s, _ := mediaServer(t)
	c := client(t, s)
	stream, err := c.GetMedia(context.Background(), &curtilagev1.GetMediaRequest{EventId: "with", Media: curtilagev1.Media_MEDIA_CLIP})
	if err != nil {
		t.Fatal(err)
	}
	first, err := stream.Recv()
	if err != nil || first.GetInfo().GetContentType() != "video/mp4" {
		t.Fatalf("info = %v, %v", first, err)
	}
	var got []byte
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, msg.GetChunk()...)
	}
	// The stub echoes the path: leading camera "a", padded range.
	if !strings.HasPrefix(string(got), "mp4/api/cam-a/start/") {
		t.Errorf("clip body %q", got)
	}
	// The capability path serves the same clip.
	e, _ := s.Store.Get("with")
	link, err := s.Link(e, curtilagev1.Media_MEDIA_CLIP, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	web := httptest.NewServer(s.MediaHandler())
	defer web.Close()
	resp, err := http.Get(web.URL + link)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "video/mp4" || !strings.HasPrefix(string(body), "mp4/api/cam-a/") {
		t.Errorf("clip link -> %d %s %q", resp.StatusCode, resp.Header.Get("Content-Type"), body[:min(len(body), 40)])
	}
}

func TestGetMediaClipPerCamera(t *testing.T) {
	s, _ := mediaServer(t)
	e, _ := s.Store.Get("with")
	e.Cameras = []string{"cam-a", "cam-b"}
	s.Store.Apply(time.Now(), policy.Change{Op: policy.OpUpdated, Event: e})
	c := client(t, s)
	ctx := context.Background()
	// cam-b is one of the event's cameras but the stub only knows cam-a:
	// the request reaches /api/cam-b/... and 404s -> NotFound.
	stream, _ := c.GetMedia(ctx, &curtilagev1.GetMediaRequest{EventId: "with", Media: curtilagev1.Media_MEDIA_CLIP, Camera: "cam-b"})
	if _, err := stream.Recv(); status.Code(err) != codes.NotFound {
		t.Errorf("cam-b (unknown to frigate) -> %v", err)
	}
	// A camera the event never saw is refused before Frigate is asked.
	stream, _ = c.GetMedia(ctx, &curtilagev1.GetMediaRequest{EventId: "with", Media: curtilagev1.Media_MEDIA_CLIP, Camera: "cam-zz"})
	if _, err := stream.Recv(); status.Code(err) != codes.NotFound {
		t.Errorf("foreign camera -> %v", err)
	}
	// A camera on a snapshot request is refused too.
	stream, _ = c.GetMedia(ctx, &curtilagev1.GetMediaRequest{EventId: "with", Media: curtilagev1.Media_MEDIA_SNAPSHOT, Camera: "cam-a"})
	if _, err := stream.Recv(); status.Code(err) != codes.NotFound {
		t.Errorf("camera on snapshot -> %v", err)
	}
	// The camera-bound capability link serves that camera's view.
	link, err := s.CameraLink(e, curtilagev1.Media_MEDIA_CLIP, "cam-a", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	web := httptest.NewServer(s.MediaHandler())
	defer web.Close()
	resp, err := http.Get(web.URL + link)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(string(body), "mp4/api/cam-a/") {
		t.Errorf("camera link -> %d %q", resp.StatusCode, body[:min(len(body), 40)])
	}
}

func TestGetMediaWithoutFrigate(t *testing.T) {
	c := client(t, &Server{Version: "test", Store: store.New(time.Hour)})
	stream, _ := c.GetMedia(context.Background(), &curtilagev1.GetMediaRequest{EventId: "x", Media: curtilagev1.Media_MEDIA_SNAPSHOT})
	if _, err := stream.Recv(); status.Code(err) != codes.Unavailable {
		t.Errorf("no frigate -> %v, want Unavailable", err)
	}
}

func TestMediaHandlerCapabilityLinks(t *testing.T) {
	s, _ := mediaServer(t)
	web := httptest.NewServer(s.MediaHandler())
	defer web.Close()
	before := Stats()
	e, _ := s.Store.Get("with")
	link, err := s.Link(e, curtilagev1.Media_MEDIA_SNAPSHOT, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(web.URL + link)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/jpeg" || !bytes.Equal(body, jpeg) {
		t.Fatalf("link -> %d %s %d bytes", resp.StatusCode, resp.Header.Get("Content-Type"), len(body))
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" || resp.Header.Get("Cache-Control") == "" {
		t.Errorf("headers: %v", resp.Header)
	}

	// Every failure is the same 404.
	expired := "/media/" + s.Keys.Mint(captoken.Claims{EventID: "with", Media: 1, Expires: time.Now().Add(-time.Second)})
	gone := "/media/" + s.Keys.Mint(captoken.Claims{EventID: "nope", Media: 1, Expires: time.Now().Add(time.Hour)})
	var bodies []string
	for _, p := range []string{"/media/garbage", "/media/", expired, gone, link + "x"} {
		resp, err := http.Get(web.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 404 {
			t.Errorf("%s -> %d", p, resp.StatusCode)
		}
		bodies = append(bodies, string(b))
	}
	for _, b := range bodies[1:] {
		if b != bodies[0] {
			t.Errorf("404 bodies differ: %q vs %q", bodies[0], b)
		}
	}
	if resp, _ := http.Post(web.URL+link, "", nil); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST -> %d", resp.StatusCode)
	}
	after := Stats()
	if after.LinksMinted-before.LinksMinted != 1 || after.LinksOpened-before.LinksOpened != 2 ||
		after.LinksExpired-before.LinksExpired != 1 || after.LinksInvalid-before.LinksInvalid != 3 {
		t.Errorf("counters: before %+v after %+v", before, after)
	}
}

func TestLinkNeedsKeys(t *testing.T) {
	s := &Server{Store: store.New(time.Hour)}
	if _, err := s.Link(policy.Event{ID: "x"}, curtilagev1.Media_MEDIA_SNAPSHOT, time.Now()); err == nil {
		t.Error("Link without keys succeeded")
	}
}
