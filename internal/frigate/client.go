package frigate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client fetches media from Frigate's HTTP API.  Curtilage is the
// only thing that talks to it; nothing it returns is passed through
// unexamined (docs/DESIGN.md "Exposure").
type Client struct {
	base *url.URL
	http *http.Client
}

// ErrNotFound is returned when Frigate has no such event or media.
var ErrNotFound = errors.New("frigate: not found")

// NewClient points at Frigate, e.g. http://frigate.cameras.svc.cluster.local:5000.
func NewClient(base string) (*Client, error) {
	u, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("frigate url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("frigate url %q: need http(s)://host[:port]", base)
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawQuery, u.Fragment = "", ""
	return &Client{base: u, http: &http.Client{Timeout: 30 * time.Second}}, nil
}

// Media is one fetched piece of media; the caller closes Body.
type Media struct {
	ContentType string
	// Size in bytes, or -1 when Frigate did not say.
	Size int64
	Body io.ReadCloser
}

// Snapshot is the event's still with its bounding box, JPEG.
func (c *Client) Snapshot(ctx context.Context, eventID string) (*Media, error) {
	if !validID(eventID) {
		return nil, ErrNotFound // never let an odd id shape a URL
	}
	return c.get(ctx, "/api/events/"+url.PathEscape(eventID)+"/snapshot.jpg", "image/jpeg")
}

// Clip is video cut from camera's continuous recordings over
// [start, end], MP4.  It exists within seconds of the moment, long
// before Frigate cuts an event's own clip, so it can be asked for
// while the event is still running.
func (c *Client) Clip(ctx context.Context, camera string, start, end time.Time) (*Media, error) {
	if !validID(camera) || !end.After(start) {
		return nil, ErrNotFound
	}
	path := fmt.Sprintf("/api/%s/start/%d/end/%d/clip.mp4", url.PathEscape(camera), start.Unix(), end.Unix())
	return c.get(ctx, path, "video/mp4")
}

func (c *Client) get(ctx context.Context, path, wantType string) (*Media, error) {
	u := *c.base
	u.Path += path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("frigate: %w", err)
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		resp.Body.Close()
		return nil, ErrNotFound
	case resp.StatusCode != http.StatusOK:
		resp.Body.Close()
		return nil, fmt.Errorf("frigate: %s: %s", path, resp.Status)
	}
	ct := resp.Header.Get("Content-Type")
	if mt, _, _ := strings.Cut(ct, ";"); strings.TrimSpace(mt) != wantType {
		resp.Body.Close()
		return nil, fmt.Errorf("frigate: %s: content-type %q, want %s", path, ct, wantType)
	}
	return &Media{ContentType: wantType, Size: resp.ContentLength, Body: resp.Body}, nil
}

// validID is the shape of a Frigate event id: "<unix>.<frac>-<6 alnum>".
// Anything else is not an id and never reaches Frigate.
func validID(id string) bool {
	if len(id) < 3 || len(id) > 64 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '.', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}
