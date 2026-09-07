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
	"github.com/jeffbstewart/curtilage/internal/mediacache"
	"github.com/jeffbstewart/curtilage/internal/policy"
)

// chunkSize is one GetMedia stream message; well under gRPC's 4 MiB
// default receive limit.
const chunkSize = 64 << 10

// clipMargin pads a clip on both sides, so the approach and the
// walking-away are in frame.
const clipMargin = 5 * time.Second

// warmLadder is where a running event's clip may be cut: the panes
// serve the newest boundary from cache instead of a fresh cut to
// now, trading up to one rung of staleness for an instant load.
// Past the last rung the view holds at four minutes until the event
// ends -- the household chose not to optimize longer (read-ahead can
// come later if it ever matters).
var warmLadder = []time.Duration{15 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute}

// warmGrace is how long after a boundary its cut is trusted: Frigate
// needs a moment to have the recording on disk.
var warmGrace = 5 * time.Second

// latestCheckpoint is the newest ladder boundary usable at now for an
// event started then; zero while the event is too young for any.
func latestCheckpoint(started, now time.Time) time.Time {
	var best time.Time
	for _, b := range warmLadder {
		if now.Sub(started) >= b+warmGrace {
			best = started.Add(b)
		}
	}
	return best
}

// clipCut is the clip's bounds for e at now.  stable means the bytes
// are a fixed, repeatable cut: an ended event always, and -- when
// checkpoints are on (the cache exists) -- a running event cut at
// its newest checkpoint instead of now.
func clipCut(e policy.Event, now time.Time, checkpoints bool) (start, end time.Time, stable bool) {
	start = e.StartedAt.Add(-clipMargin)
	if !e.EndedAt.IsZero() {
		return start, e.EndedAt.Add(clipMargin), true
	}
	if checkpoints {
		if cp := latestCheckpoint(e.StartedAt, now); !cp.IsZero() {
			return start, cp, true
		}
	}
	return start, now, false
}

// cutTag is the strong ETag naming one cut.
func cutTag(camera string, start, end time.Time) string {
	return fmt.Sprintf("%q", fmt.Sprintf("%s-%d-%d", camera, start.Unix(), end.Unix()))
}

// clipKey is the cache identity of one cut; snapKey of one ended
// event's snapshot.
func clipKey(camera string, start, end time.Time) mediacache.Key {
	return mediacache.Key{Media: "clip", Ref: camera, Start: start.Unix(), End: end.Unix()}
}

func snapKey(e policy.Event) mediacache.Key {
	return mediacache.Key{Media: "snapshot", Ref: e.SourceID}
}

// Media counters for /metrics: the abuse signal for the public door.
var (
	linksMinted   atomic.Uint64
	linksOpened   atomic.Uint64
	linksInvalid  atomic.Uint64 // malformed, bad signature, unknown key
	linksExpired  atomic.Uint64
	mediaFetches  atomic.Uint64
	mediaBytes    atomic.Uint64
	mediaFailures atomic.Uint64 // Frigate said no, or was unreachable
	cacheHits     atomic.Uint64 // cuts served without touching Frigate
	cacheMisses   atomic.Uint64
)

// MediaStats is a snapshot of the counters.
type MediaStats struct {
	LinksMinted, LinksOpened, LinksInvalid, LinksExpired uint64
	MediaFetches, MediaBytes, MediaFailures              uint64
	CacheHits, CacheMisses                               uint64
}

// Stats returns the media counters.
func Stats() MediaStats {
	return MediaStats{
		LinksMinted: linksMinted.Load(), LinksOpened: linksOpened.Load(),
		LinksInvalid: linksInvalid.Load(), LinksExpired: linksExpired.Load(),
		MediaFetches: mediaFetches.Load(), MediaBytes: mediaBytes.Load(), MediaFailures: mediaFailures.Load(),
		CacheHits: cacheHits.Load(), CacheMisses: cacheMisses.Load(),
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

// resolveEvent maps an event id, media kind and optional camera to
// the event whose media is asked for, with every failure a client may
// see collapsed to NotFound.  camera narrows a clip to one of the
// event's cameras; "" is the leading one.
func (s *Server) resolveEvent(eventID string, media curtilagev1.Media, camera string) (policy.Event, error) {
	e, ok := s.Store.Get(eventID)
	if !ok {
		return policy.Event{}, status.Error(codes.NotFound, "no such event or media")
	}
	if camera != "" {
		if media != curtilagev1.Media_MEDIA_CLIP || (camera != e.Camera && !slices.Contains(e.Cameras, camera)) {
			return policy.Event{}, status.Error(codes.NotFound, "no such event or media")
		}
		e.Camera = camera // this camera's view of the same window
	}
	switch media {
	case curtilagev1.Media_MEDIA_SNAPSHOT:
		if !e.HasSnapshot {
			return policy.Event{}, status.Error(codes.NotFound, "no such event or media")
		}
	case curtilagev1.Media_MEDIA_CLIP:
	default:
		return policy.Event{}, status.Errorf(codes.InvalidArgument, "media %v is not one this server serves", media)
	}
	return e, nil
}

// fetch resolves and fetches straight from Frigate: the GetMedia path
// and the cacheless fallback.  etag is non-empty only when the bytes
// are a stable cut (an ended event's clip).
func (s *Server) fetch(ctx context.Context, eventID string, media curtilagev1.Media, camera string) (m *frigate.Media, etag string, err error) {
	e, err := s.resolveEvent(eventID, media, camera)
	if err != nil {
		return nil, "", err
	}
	switch media {
	case curtilagev1.Media_MEDIA_SNAPSHOT:
		m, err = s.Frigate.Snapshot(ctx, e.SourceID)
	case curtilagev1.Media_MEDIA_CLIP:
		// The recording-range clip from the leading camera: playable
		// the moment the event exists, growing until it ends.
		start, end, stable := clipCut(e, time.Now(), false)
		if stable {
			etag = cutTag(e.Camera, start, end)
		}
		m, err = s.Frigate.Clip(ctx, e.Camera, start, end)
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
		if s.Cache != nil && s.serveCached(w, r, claims) {
			return
		}
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

// spoolSlots bounds how many spool files exist at once: a multi-pane
// event page opens one clip per camera, so a slot per camera with
// room for a second viewer.  When every slot is busy the next request
// streams instead (serveRanged returns false) -- degraded, never
// queued.
var spoolSlots = make(chan struct{}, 16)

// serveCached answers from the cut cache when the request names an
// immutable cut: an ended event's clip or snapshot, or a running
// event's clip at its newest checkpoint (clipCut).  Returns false --
// having written nothing -- when the cache cannot answer (unstable
// bytes, a fill failure); the caller's direct path then serves or
// produces the proper error.
func (s *Server) serveCached(w http.ResponseWriter, r *http.Request, claims captoken.Claims) bool {
	e, err := s.resolveEvent(claims.EventID, curtilagev1.Media(claims.Media), claims.Camera)
	if err != nil {
		return false // the direct path 404s identically
	}
	var key mediacache.Key
	var etag, ctype string
	var fetch func(context.Context) (io.ReadCloser, error)
	switch curtilagev1.Media(claims.Media) {
	case curtilagev1.Media_MEDIA_SNAPSHOT:
		if e.EndedAt.IsZero() {
			return false // Frigate still updates it
		}
		key, ctype = snapKey(e), "image/jpeg"
		etag = fmt.Sprintf("%q", e.SourceID+"-snap")
		fetch = s.snapshotFetch(e.SourceID)
	case curtilagev1.Media_MEDIA_CLIP:
		now := time.Now()
		if !e.EndedAt.IsZero() && now.Before(e.EndedAt.Add(clipMargin+warmGrace)) {
			return false // the final cut has not settled on disk yet:
			// caching now would freeze a truncated clip under its key
		}
		start, end, stable := clipCut(e, now, true)
		if !stable {
			return false // too young for any checkpoint: stream live
		}
		key, ctype = clipKey(e.Camera, start, end), "video/mp4"
		etag = cutTag(e.Camera, start, end)
		fetch = s.clipFetch(e.Camera, start, end)
	default:
		return false
	}
	path, ok := s.Cache.Get(key)
	if ok {
		cacheHits.Add(1)
	} else {
		cacheMisses.Add(1)
		if path, err = s.Cache.Fill(r.Context(), key, fetch); err != nil {
			log.Printf("media: cache fill %s: %v", claims.EventID, err)
			return false
		}
	}
	f, err := os.Open(path)
	if err != nil { // evicted between lookup and open
		return false
	}
	defer f.Close()
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", "private, max-age="+strconv.Itoa(int(time.Until(claims.Expires)/time.Second)))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("ETag", etag)
	http.ServeContent(&countingWriter{ResponseWriter: w}, r, "", time.Time{}, f)
	return true
}

// clipFetch and snapshotFetch adapt Frigate fetches to cache fills,
// carrying the fetch counters with them.
func (s *Server) clipFetch(camera string, start, end time.Time) func(context.Context) (io.ReadCloser, error) {
	return func(ctx context.Context) (io.ReadCloser, error) {
		m, err := s.Frigate.Clip(ctx, camera, start, end)
		if err != nil {
			mediaFailures.Add(1)
			return nil, err
		}
		mediaFetches.Add(1)
		return m.Body, nil
	}
}

func (s *Server) snapshotFetch(sourceID string) func(context.Context) (io.ReadCloser, error) {
	return func(ctx context.Context) (io.ReadCloser, error) {
		m, err := s.Frigate.Snapshot(ctx, sourceID)
		if err != nil {
			mediaFailures.Add(1)
			return nil, err
		}
		mediaFetches.Add(1)
		return m.Body, nil
	}
}

// serveRanged spools one stable clip cut to a temp file and serves it
// with http.ServeContent: Range requests are how a browser reads mp4
// metadata and seeks without downloading the whole clip, and the
// spool is what makes Frigate's one-way stream seekable and the
// Content-Length exact.  The etag names the cut (camera and bounds,
// which Frigate re-cuts to the same recording bytes), so a client
// resuming with If-Range never splices bytes from a different cut.
// Returns false when no spool file could be made or every spool slot
// is busy -- the caller streams instead; every other outcome is
// answered here.
func serveRanged(w http.ResponseWriter, r *http.Request, etag string, body io.Reader) bool {
	select {
	case spoolSlots <- struct{}{}:
		defer func() { <-spoolSlots }()
	default:
		return false
	}
	f, err := os.CreateTemp("", "curtilage-media-*")
	if err != nil {
		log.Printf("media: spool: %v", err)
		return false
	}
	defer f.Close()
	// Unlink at birth: the open fd keeps the bytes readable and the
	// kernel reclaims them when it closes, so no spool outlives its
	// request -- not even through a crash.  (Windows dev boxes honor
	// this too: Go opens with FILE_SHARE_DELETE and modern NTFS
	// deletes POSIX-style.)
	if err := os.Remove(f.Name()); err != nil {
		log.Printf("media: spool unlink: %v", err)
		defer os.Remove(f.Name()) // second try on the way out
	}
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
	return fmt.Sprintf("links minted=%d opened=%d invalid=%d expired=%d; media fetches=%d bytes=%d failures=%d; cache hits=%d misses=%d",
		st.LinksMinted, st.LinksOpened, st.LinksInvalid, st.LinksExpired, st.MediaFetches, st.MediaBytes, st.MediaFailures, st.CacheHits, st.CacheMisses)
}
