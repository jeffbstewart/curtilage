// Package house serves the in-the-house page: the last day of events
// with the policy engine's verdict and audience for each -- what was
// sent, or would have been, to whom.  It is the tool the rules are
// tuned with, so it shows the engine's reasoning, not just the
// pictures.
//
// Reachable only from configured subnets.  X-Forwarded-For is trusted
// only when the direct peer is a configured proxy, and then the caller
// is the rightmost forwarded address that is not itself a trusted
// proxy: a client cannot forge its way in by adding the header.
// Everyone else gets the same 404 an unsigned media link gets.
package house

import (
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	curtilagev1 "github.com/jeffbstewart/curtilage/gen/curtilage/v1"
	"github.com/jeffbstewart/curtilage/internal/policy"
	"github.com/jeffbstewart/curtilage/internal/server"
	"github.com/jeffbstewart/curtilage/internal/store"
)

// Handler serves GET /house/.
type Handler struct {
	Store       *store.Store
	API         *server.Server // for media links; Keys may be nil
	Allow       []*net.IPNet
	Proxies     []*net.IPNet
	DisplayName string
	// Location is the household's time zone (config timezone); nil
	// means UTC.  Nobody wants to do time zone math on this page.
	Location *time.Location
	// Now is time.Now unless a test says otherwise.
	Now func() time.Time
}

func (h *Handler) loc() *time.Location {
	if h.Location == nil {
		return time.UTC
	}
	return h.Location
}

// DefaultWindow is how far back the page looks unless asked (?hours=).
const DefaultWindow = 24 * time.Hour

// Caller resolves the client address of r under the proxy rules, or
// nil when it cannot be determined.
func (h *Handler) Caller(r *http.Request) net.IP {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer := net.ParseIP(host)
	if peer == nil {
		return nil
	}
	if !inAny(peer, h.Proxies) {
		return peer // the header, if any, is not ours to believe
	}
	// Walk the chain from the right, skipping proxies we trust; the
	// first address that is not one of ours is the caller.
	var chain []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		chain = append(chain, strings.Split(v, ",")...)
	}
	for i := len(chain) - 1; i >= 0; i-- {
		ip := net.ParseIP(strings.TrimSpace(chain[i]))
		if ip == nil {
			return nil // a malformed chain is nobody
		}
		if !inAny(ip, h.Proxies) {
			return ip
		}
	}
	return peer // only proxies in the chain: the proxy itself is calling
}

// Allowed reports whether r comes from inside the house.
func (h *Handler) Allowed(r *http.Request) bool {
	if len(h.Allow) == 0 {
		return false
	}
	ip := h.Caller(r)
	return ip != nil && inAny(ip, h.Allow)
}

func inAny(ip net.IP, nets []*net.IPNet) bool {
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// row is one event as the page shows it.
type row struct {
	When     string
	Camera   string
	Label    string
	Zones    string
	Duration string
	Verdict  string
	Audience string
	Clip     string
	Thumb    string // media link path, or ""
	Live     bool
	SourceID string
	// What is the one sentence (policy.Describe); History is every
	// earlier sentence, newest first, with when it was said -- how the
	// engine's understanding evolved.
	What    string
	History []string
}

type page struct {
	DisplayName string
	Hours       int
	All         bool // every event, not just the sent ones
	Since       string
	Now         string
	Zone        string // the time zone every time on the page is in
	Rows        []row
	Total       int // events in the window
	Hidden      int // of those, not shown (not sent; view=sent)
	Live        int
	ByAudience  []count
	ByVerdict   []count
}

type count struct {
	Name string
	N    int
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.Allowed(r) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	now := time.Now()
	if h.Now != nil {
		now = h.Now()
	}
	window := DefaultWindow
	if s := r.URL.Query().Get("hours"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			window = time.Duration(n) * time.Hour
		}
	}
	if window > h.Store.Retention() {
		window = h.Store.Retention()
	}
	since := now.Add(-window)
	all := r.URL.Query().Get("view") == "all"

	p := page{DisplayName: h.DisplayName, Hours: int(window / time.Hour), All: all,
		Since: since.In(h.loc()).Format(time.RFC1123), Now: now.In(h.loc()).Format(time.RFC1123), Zone: h.loc().String()}
	audiences, verdicts := map[string]int{}, map[string]int{}
	token := ""
	for {
		events, next, err := h.Store.List(nil, 500, token)
		if err != nil || len(events) == 0 {
			break
		}
		stop := false
		for _, e := range events {
			if e.StartedAt.Before(since) {
				stop = true
				break
			}
			// The summary counts everything; the table shows what was
			// sent unless asked for all.
			p.Total++
			if e.Running() {
				p.Live++
			}
			audiences[policy.Audience(e.Kind)]++
			verdicts[e.Kind.String()]++
			if !all && !policy.Sent(e.Kind) {
				p.Hidden++
				continue
			}
			p.Rows = append(p.Rows, h.row(e, now))
		}
		if stop {
			break
		}
		token = next
	}
	p.ByAudience, p.ByVerdict = counts(audiences), counts(verdicts)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if err := tmpl.Execute(w, p); err != nil {
		log.Printf("house: render: %v", err)
	}
}

func (h *Handler) row(e policy.Event, now time.Time) row {
	rw := row{
		When:     e.StartedAt.In(h.loc()).Format("Mon 15:04:05"),
		Camera:   e.Camera,
		Label:    e.Label,
		Zones:    strings.Join(e.Zones, ", "),
		Verdict:  e.Kind.String(),
		Audience: policy.Audience(e.Kind),
		Clip:     e.Clip.String(),
		Live:     e.Running(),
		SourceID: e.SourceID,
	}
	if e.SubLabel != "" {
		rw.Label += " (" + e.SubLabel + ")"
	}
	rw.What = policy.Describe(e)
	if e.Kind == policy.KindActivity {
		rw.Camera = strings.Join(e.Cameras, ", ")
		rw.Label = fmt.Sprintf("%d objects", len(e.SourceIDs))
		// Newest first; the current state is What.  A revision that
		// changed nothing a person would read (a camera, a snapshot)
		// is folded into the one before it.
		// Each line carries the time its wording was FIRST reached.
		hist := h.Store.History(e.ID)
		prev := rw.What
		for i := len(hist) - 2; i >= 0; i-- {
			text := policy.Describe(hist[i].Event)
			when := hist[i].At.In(h.loc()).Format("15:04:05")
			if text == prev {
				if n := len(rw.History); n > 0 {
					rw.History[n-1] = when + "  " + text // same words, earlier
				}
				continue
			}
			prev = text
			rw.History = append(rw.History, when+"  "+text)
		}
	}
	end := e.EndedAt
	if end.IsZero() {
		end = now
	}
	rw.Duration = end.Sub(e.StartedAt).Round(time.Second).String()
	if e.HasSnapshot && h.API != nil && h.API.Keys != nil {
		if link, err := h.API.Link(e, curtilagev1.Media_MEDIA_SNAPSHOT, now); err == nil {
			rw.Thumb = link
		}
	}
	return rw
}

func counts(m map[string]int) []count {
	out := make([]count, 0, len(m))
	for k, v := range m {
		out = append(out, count{k, v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].N > out[j].N || (out[i].N == out[j].N && out[i].Name < out[j].Name) })
	return out
}

var tmpl = template.Must(template.New("house").Parse(`<!doctype html>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.DisplayName}} -- last {{.Hours}}h</title>
<style>
 body { font: 14px/1.4 system-ui, sans-serif; margin: 1.5rem; color: #222; background: #fafafa; }
 h1 { font-size: 1.3rem; margin: 0 0 .25rem; }
 .sub { color: #666; margin-bottom: 1rem; }
 .sum { display: flex; gap: 2rem; flex-wrap: wrap; margin-bottom: 1rem; }
 .sum div { background: #fff; border: 1px solid #ddd; border-radius: 6px; padding: .5rem .75rem; }
 .sum b { display: block; color: #666; font-weight: 600; font-size: .8rem; text-transform: uppercase; }
 table { border-collapse: collapse; width: 100%; background: #fff; }
 th, td { text-align: left; padding: .4rem .6rem; border-bottom: 1px solid #eee; vertical-align: top; white-space: nowrap; }
 th { background: #f0f0f0; font-weight: 600; position: sticky; top: 0; }
 td.z { white-space: normal; }
 img { height: 64px; border-radius: 4px; display: block; }
 .live { color: #b00; font-weight: 600; }
 .aud-nobody { color: #888; }
 .aud-household { color: #060; font-weight: 600; }
 .src { color: #999; font-size: .75rem; font-family: ui-monospace, monospace; }
 ul.hist { margin: .2rem 0 0 1rem; padding: 0; color: #666; font-size: .85rem; }
 ul.hist li { margin: 0; white-space: normal; }
</style>
<h1>{{.DisplayName}}: the last {{.Hours}} hours</h1>
<div class="sub">{{.Since}} to {{.Now}} ({{.Zone}}). {{.Total}} events, {{.Live}} still running. Newest first. Audience is what the policy engine sent, or would have sent, to whom.
{{if .All}}Showing <b>everything</b>, including what was not sent. <a href="?hours={{.Hours}}">Show only what was sent</a>.{{else}}Showing what was sent; <b>{{.Hidden}}</b> not sent are hidden. <a href="?hours={{.Hours}}&amp;view=all">Show everything</a>.{{end}}</div>
<div class="sum">
 <div><b>By audience</b>{{range .ByAudience}}{{.Name}}: {{.N}}<br>{{end}}</div>
 <div><b>By verdict</b>{{range .ByVerdict}}{{.Name}}: {{.N}}<br>{{end}}</div>
</div>
<table>
<tr><th>When</th><th>Cameras</th><th>What</th><th>Zones</th><th>For</th><th>Clip</th><th>Verdict</th><th>Audience</th><th>Snapshot</th></tr>
{{range .Rows}}<tr>
 <td>{{.When}}{{if .Live}} <span class="live">live</span>{{end}}</td>
 <td class="z">{{.Camera}}</td>
 <td class="z"><b>{{.What}}</b>{{if .History}}<ul class="hist">{{range .History}}<li>{{.}}</li>{{end}}</ul>{{end}}<br><span class="src">{{.Label}} {{.SourceID}}</span></td>
 <td class="z">{{.Zones}}</td>
 <td>{{.Duration}}</td>
 <td>{{.Clip}}</td>
 <td>{{.Verdict}}</td>
 <td class="{{if eq .Audience "household"}}aud-household{{else}}aud-nobody{{end}}">{{.Audience}}</td>
 <td>{{if .Thumb}}<a href="{{.Thumb}}"><img src="{{.Thumb}}" alt="" loading="lazy"></a>{{end}}</td>
</tr>
{{end}}</table>
`))
