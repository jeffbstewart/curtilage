package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	curtilagev1 "github.com/jeffbstewart/curtilage/gen/curtilage/v1"
	"github.com/jeffbstewart/curtilage/internal/captoken"
	"github.com/jeffbstewart/curtilage/internal/frigate"
	"github.com/jeffbstewart/curtilage/internal/policy"
)

// chunkSize is one GetMedia stream message; well under gRPC's 4 MiB
// default receive limit.
const chunkSize = 64 << 10

// clipMargin pads a clip on both sides, so the approach and the
// walking-away are in frame.
const clipMargin = 5 * time.Second

// Media counters for /metrics: the abuse signal for the public door.
var (
	linksMinted   atomic.Uint64
	linksOpened   atomic.Uint64
	linksInvalid  atomic.Uint64 // malformed, bad signature, unknown key
	linksExpired  atomic.Uint64
	mediaFetches  atomic.Uint64
	mediaBytes    atomic.Uint64
	mediaFailures atomic.Uint64 // Frigate said no, or was unreachable
)

// MediaStats is a snapshot of the counters.
type MediaStats struct {
	LinksMinted, LinksOpened, LinksInvalid, LinksExpired uint64
	MediaFetches, MediaBytes, MediaFailures              uint64
}

// Stats returns the media counters.
func Stats() MediaStats {
	return MediaStats{
		LinksMinted: linksMinted.Load(), LinksOpened: linksOpened.Load(),
		LinksInvalid: linksInvalid.Load(), LinksExpired: linksExpired.Load(),
		MediaFetches: mediaFetches.Load(), MediaBytes: mediaBytes.Load(), MediaFailures: mediaFailures.Load(),
	}
}

// GetMedia streams one piece of an event's media.  On the LAN this is
// by event id alone (docs/DESIGN.md: enrolled devices come later);
// unknown ids and ids without that media both answer NotFound.
func (s *Server) GetMedia(req *curtilagev1.GetMediaRequest, stream grpc.ServerStreamingServer[curtilagev1.GetMediaResponse]) error {
	if s.Frigate == nil {
		return status.Error(codes.Unavailable, "media is not configured on this server (frigate.url)")
	}
	m, _, err := s.fetch(stream.Context(), req.GetEventId(), req.GetMedia(), req.GetCamera())
	if err != nil {
		return err
	}
	defer m.Body.Close()
	info := &curtilagev1.MediaInfo{ContentType: m.ContentType}
	if m.Size > 0 {
		info.Size = uint64(m.Size)
	}
	if err := stream.Send(&curtilagev1.GetMediaResponse{Payload: &curtilagev1.GetMediaResponse_Info{Info: info}}); err != nil {
		return err
	}
	buf := make([]byte, chunkSize)
	for {
		n, err := m.Body.Read(buf)
		if n > 0 {
			mediaBytes.Add(uint64(n))
			if err := stream.Send(&curtilagev1.GetMediaResponse{Payload: &curtilagev1.GetMediaResponse_Chunk{Chunk: buf[:n]}}); err != nil {
				return err
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			mediaFailures.Add(1)
			return status.Error(codes.Unavailable, "media stream from Frigate broke")
		}
	}
}

// fetch resolves an event id, media kind and optional camera to a
// Frigate fetch, with every failure a client may see collapsed to
// NotFound.  camera narrows a clip to one of the event's cameras; ""
// is the leading one.  etag is non-empty only when the bytes are a
// stable cut -- a clip of an ENDED event, whose bounds no longer move
// -- and names that cut; a growing live clip and a snapshot (which
// Frigate updates while the event runs) have none.
func (s *Server) fetch(ctx context.Context, eventID string, media curtilagev1.Media, camera string) (m *frigate.Media, etag string, err error) {
	e, ok := s.Store.Get(eventID)
	if !ok {
		return nil, "", status.Error(codes.NotFound, "no such event or media")
	}
	if camera != "" {
		if media != curtilagev1.Media_MEDIA_CLIP || (camera != e.Camera && !slices.Contains(e.Cameras, camera)) {
			return nil, "", status.Error(codes.NotFound, "no such event or media")
		}
		e.Camera = camera // this camera's view of the same window
	}
	switch media {
	case curtilagev1.Media_MEDIA_SNAPSHOT:
		if !e.HasSnapshot {
			return nil, "", status.Error(codes.NotFound, "no such event or media")
		}
		m, err = s.Frigate.Snapshot(ctx, e.SourceID)
	case curtilagev1.Media_MEDIA_CLIP:
		// The recording-range clip from the leading camera: playable
		// the moment the event exists, growing until it ends.
		start := e.StartedAt.Add(-clipMargin)
		end := e.EndedAt
		if end.IsZero() {
			end = time.Now()
		} else {
			end = end.Add(clipMargin)
			etag = fmt.Sprintf("%q", fmt.Sprintf("%s-%d-%d", e.Camera, start.Unix(), end.Unix()))
		}
		m, err = s.Frigate.Clip(ctx, e.Camera, start, end)
	default:
		return nil, "", status.Errorf(codes.InvalidArgument, "media %v is not one this server serves", media)
	}
	switch {
	case errors.Is(err, frigate.ErrNotFound):
		mediaFailures.Add(1)
		return nil, "", status.Error(codes.NotFound, "no such event or media")
	case err != nil:
		mediaFailures.Add(1)
		log.Printf("media: %s %v: %v", eventID, media, err)
		return nil, "", status.Error(codes.Unavailable, "Frigate did not answer")
	}
	mediaFetches.Add(1)
	return m, etag, nil
}

// Link mints a capability path for one piece of an event's media:
// "/media/<token>", valid for LinkTTL from now.  It is the thing a
// notification or the web page carries; the host is the caller's.
func (s *Server) Link(e policy.Event, media curtilagev1.Media, now time.Time) (string, error) {
	return s.CameraLink(e, media, "", now)
}

// CameraLink is Link narrowed to one camera's view (MEDIA_CLIP on a
// multi-camera event); "" is the leading camera.
func (s *Server) CameraLink(e policy.Event, media curtilagev1.Media, camera string, now time.Time) (string, error) {
	if s.Keys == nil {
		return "", errors.New("media links are not configured (CURTILAGE_MEDIA_KEY)")
	}
	linksMinted.Add(1)
	return "/media/" + s.Keys.Mint(captoken.Claims{EventID: e.ID, Media: uint8(media), Camera: camera, Expires: now.Add(s.LinkTTL)}), nil
}

// MediaHandler serves GET /media/<token>: the capability URL.  Every
// failure is a 404 with the same body -- a probe learns nothing
// about which part was wrong; the counters know.  Clips of ended
// events answer Range requests (see serveRanged); live clips and
// snapshots stream whole, since their bytes change between requests.
func (s *Server) MediaHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		notFound := func() { http.Error(w, "not found", http.StatusNotFound) }
		if s.Keys == nil || s.Frigate == nil {
			notFound()
			return
		}
		token := strings.TrimPrefix(r.URL.Path, "/media/")
		claims, err := s.Keys.Verify(token, time.Now())
		switch {
		case errors.Is(err, captoken.ErrExpired):
			linksExpired.Add(1)
			notFound()
			return
		case err != nil:
			linksInvalid.Add(1)
			notFound()
			return
		}
		linksOpened.Add(1)
		m, etag, err := s.fetch(r.Context(), claims.EventID, curtilagev1.Media(claims.Media), claims.Camera)
		if err != nil {
			if status.Code(err) == codes.Unavailable {
				http.Error(w, "media source unavailable", http.StatusBadGateway)
				return
			}
			notFound()
			return
		}
		defer m.Body.Close()
		w.Header().Set("Content-Type", m.ContentType)
		w.Header().Set("Cache-Control", "private, max-age="+strconv.Itoa(int(time.Until(claims.Expires)/time.Second)))
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.Method == http.MethodHead {
			if etag != "" {
				w.Header().Set("ETag", etag)
				w.Header().Set("Accept-Ranges", "bytes")
			}
			if m.Size >= 0 {
				w.Header().Set("Content-Length", strconv.FormatInt(m.Size, 10))
			}
			return
		}
		if etag != "" && serveRanged(w, r, etag, m.Body) {
			return
		}
		// Unstable bytes (a live clip grows, a snapshot updates while
		// the event runs) -- a byte range could splice two different
		// files, so refuse ranges and stream whole.  Also the fallback
		// when no spool file could be made.
		w.Header().Set("Accept-Ranges", "none")
		if m.Size >= 0 {
			w.Header().Set("Content-Length", strconv.FormatInt(m.Size, 10))
		}
		n, err := io.Copy(w, m.Body)
		mediaBytes.Add(uint64(n))
		if err != nil {
			mediaFailures.Add(1)
		}
	})
}

// serveRanged spools one stable clip cut to a temp file and serves it
// with http.ServeContent: Range requests are how a browser reads mp4
// metadata and seeks without downloading the whole clip, and the
// spool is what makes Frigate's one-way stream seekable and the
// Content-Length exact.  The etag names the cut (camera and bounds,
// which Frigate re-cuts to the same recording bytes), so a client
// resuming with If-Range never splices bytes from a different cut.
// Returns false when no spool file could be made -- the caller
// streams instead; every other outcome is answered here.
func serveRanged(w http.ResponseWriter, r *http.Request, etag string, body io.Reader) bool {
	f, err := os.CreateTemp("", "curtilage-media-*")
	if err != nil {
		log.Printf("media: spool: %v", err)
		return false
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err := io.Copy(f, body); err != nil {
		mediaFailures.Add(1)
		http.Error(w, "media source unavailable", http.StatusBadGateway)
		return true
	}
	w.Header().Set("ETag", etag)
	http.ServeContent(&countingWriter{ResponseWriter: w}, r, "", time.Time{}, f)
	return true
}

// countingWriter feeds the bytes ServeContent actually sends into
// mediaBytes, the served-volume counter.
type countingWriter struct{ http.ResponseWriter }

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.ResponseWriter.Write(p)
	mediaBytes.Add(uint64(n))
	return n, err
}

// String form for logs.
func (st MediaStats) String() string {
	return fmt.Sprintf("links minted=%d opened=%d invalid=%d expired=%d; media fetches=%d bytes=%d failures=%d",
		st.LinksMinted, st.LinksOpened, st.LinksInvalid, st.LinksExpired, st.MediaFetches, st.MediaBytes, st.MediaFailures)
}
