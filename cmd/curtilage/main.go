// Command curtilage is the server: it watches Frigate over MQTT,
// decides which events are news, and tells the household
// (docs/DESIGN.md).  Phase 1: ingest, record, and serve the events.
//
//	curtilage run -config curtilage.textproto
//	curtilage run -config curtilage.textproto -replay F.mcap -speed 10
//	curtilage replay -file curtilage-20260829T120000Z.mcap
//	curtilage trim -file F.mcap -out G.mcap -from T -to T -keep RE -drop RE
//	curtilage version
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"google.golang.org/grpc"

	curtilagev1 "github.com/jeffbstewart/curtilage/gen/curtilage/v1"
	"github.com/jeffbstewart/curtilage/internal/captoken"
	"github.com/jeffbstewart/curtilage/internal/config"
	"github.com/jeffbstewart/curtilage/internal/frigate"
	"github.com/jeffbstewart/curtilage/internal/house"
	"github.com/jeffbstewart/curtilage/internal/metrics"
	"github.com/jeffbstewart/curtilage/internal/mqtt"
	"github.com/jeffbstewart/curtilage/internal/policy"
	"github.com/jeffbstewart/curtilage/internal/record"
	"github.com/jeffbstewart/curtilage/internal/server"
	"github.com/jeffbstewart/curtilage/internal/store"
)

// version is set by the build (-ldflags "-X main.version=...").
var version = "dev"

func usage() {
	fmt.Fprint(os.Stderr, `usage: curtilage <command> [flags]

  run     -config F          watch the broker; record when configured; serve the API
          -replay F.mcap     ... or serve from a recording instead (-speed N; 0 = no pacing)
  replay  -file F.mcap       summarize a recording (-topic, -dump N)
  trim    -file F -out G     cut a recording down (-from, -to, -keep, -drop)
  version                    print the version
`)
}

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "run":
		err = cmdRun(os.Args[2:])
	case "replay":
		err = cmdReplay(os.Args[2:])
	case "trim":
		err = cmdTrim(os.Args[2:])
	case "version", "-version", "--version":
		fmt.Println(versionString())
	case "help", "-h", "--help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			log.Printf("curtilage: %v", err)
		}
		os.Exit(1)
	}
}

func versionString() string { return "curtilage " + version }

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "configuration file (protobuf text format)")
	replay := fs.String("replay", "", "serve from this recording instead of the broker (nothing is recorded)")
	speed := fs.Float64("speed", 1, "with -replay: 1 is real time, 10 is ten times faster, 0 is no pacing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgPath == "" {
		fs.Usage()
		return fmt.Errorf("-config is required")
	}
	if *speed < 0 {
		return fmt.Errorf("-speed must be >= 0")
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	record.Library = versionString()

	// SIGTERM (kubernetes, docker stop) and SIGINT cancel the root
	// context; everything below drains and closes in order before exit.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	creds, err := config.CredentialsFromEnv()
	if err != nil {
		return err
	}
	fc, kr, err := mediaSetup(cfg, creds)
	if err != nil {
		return err
	}
	retention := cfg.Recording.GetRetention().AsDuration()
	st := store.New(retention)
	eng := policy.NewPassthrough()
	if *replay != "" {
		return runReplay(ctx, cfg, st, eng, fc, kr, *replay, *speed)
	}

	// The store is a view of the recordings: rebuild it from them
	// before taking live traffic, and delete the ones past retention.
	dir := cfg.Recording.GetDir()
	if dir != "" {
		res, errs := store.Rebuild(ctx, st, eng, dir, time.Now())
		for _, e := range errs {
			log.Printf("rebuild: %v", e)
		}
		s := st.Stats()
		log.Printf("rebuilt from %d recordings (%d records, %d torn, %d deleted): %d events, %d live",
			res.Files, res.Records, res.Truncated, res.Deleted, s.Events, s.Live)
	}

	// Broker -> records -> (engine -> store) and -> recorder.
	records := make(chan *record.Record, 1024)
	toRecorder := make(chan *record.Record, 1024)
	recErr := make(chan error, 1)
	var rotator *record.Rotator
	if dir != "" {
		rotator = record.NewRotator()
		go func() {
			// Not ctx: the recorder must outlive the cancellation and
			// finish the file on the closed channel.
			recErr <- record.Write(context.Background(), record.Options{
				Dir: dir, RotateEvery: cfg.Recording.RotateEvery.AsDuration(), Rotate: rotator.Chan(),
			}, toRecorder)
		}()
		log.Printf("recording to %s (rotate every %s, keep %s)", dir, cfg.Recording.RotateEvery.AsDuration(), retention)
	} else {
		go func() {
			for range toRecorder {
			}
			recErr <- nil
		}()
	}
	go func() {
		defer close(toRecorder)
		for r := range records {
			store.Feed(st, eng, r)
			toRecorder <- r
		}
	}()

	srv, gs := httpServer(cfg, st, rotator, fc, kr)
	go serve(srv, cfg.Listen)
	go pruneHourly(ctx, st, dir)

	m := cfg.Mqtt
	mqttErr := mqtt.Run(ctx, mqtt.Options{
		Host: m.Host, Port: m.Port, ClientID: m.ClientId,
		User: creds.User, Password: creds.Password,
		Keepalive: m.Keepalive.AsDuration(), Subscriptions: m.Subscriptions,
		PublishPrefix: m.PublishPrefix,
	}, records) // Run closes records when it returns

	// Order matters: broker session gone -> channel closed -> engine
	// fed -> recorder drains and closes its MCAP -> then the HTTP and
	// gRPC servers -> exit.
	werr := <-recErr
	shutdown(srv, gs)
	if mqttErr != nil {
		return mqttErr
	}
	if werr != nil {
		return fmt.Errorf("recorder: %w", werr)
	}
	log.Print("clean shutdown")
	return nil
}

// runReplay serves the API from one recording: the app can be built
// against real traffic with no broker and no house.  Records are fed
// at their original pace divided by speed, then the server stays up
// until interrupted.
func runReplay(ctx context.Context, cfg *curtilagev1.Config, st *store.Store, eng policy.Engine, fc *frigate.Client, kr *captoken.Keyring, path string, speed float64) error {
	recs, err := record.ReadAll(ctx, path)
	if err != nil && !errors.Is(err, record.ErrTruncated) && !errors.Is(err, record.ErrCorrupt) {
		return err
	}
	if err != nil {
		log.Printf("replay: %v", err)
	}
	srv, gs := httpServer(cfg, st, nil, fc, kr)
	go serve(srv, cfg.Listen)
	log.Printf("replaying %d records from %s at %gx", len(recs), path, speed)
	var prev time.Time
	for _, r := range recs {
		at := r.GetReceivedAt().AsTime()
		if speed > 0 && !prev.IsZero() {
			gap := time.Duration(float64(at.Sub(prev)) / speed)
			if gap > 0 {
				select {
				case <-time.After(gap):
				case <-ctx.Done():
				}
			}
		}
		if ctx.Err() != nil {
			break
		}
		prev = at
		store.Feed(st, eng, r)
	}
	if ctx.Err() == nil {
		s := st.Stats()
		log.Printf("replay finished: %d events, %d live; serving until interrupted", s.Events, s.Live)
		<-ctx.Done()
	}
	shutdown(srv, gs)
	log.Print("clean shutdown")
	return nil
}

// httpServer is the one listener: gRPC (h2c) for the API, plain HTTP
// for /metrics, /healthz and /admin.  MediaManager's pattern: one
// port, the reverse proxy in front.
func httpServer(cfg *curtilagev1.Config, st *store.Store, rotator *record.Rotator, fc *frigate.Client, kr *captoken.Keyring) (*http.Server, *grpc.Server) {
	gs := grpc.NewServer()
	api := &server.Server{Version: version, DisplayName: cfg.DisplayName, Store: st,
		Frigate: fc, Keys: kr, LinkTTL: cfg.GetLinks().GetTtl().AsDuration()}
	server.Register(gs, api)
	mux := adminMux(rotator, st, api)
	// The in-the-house page: subnet-gated, 404 to everyone else.
	allow, _ := config.ParseCIDRs(cfg.GetHouse().GetAllowCidrs()) // validated by config.Load
	proxies, _ := config.ParseCIDRs(cfg.GetHouse().GetTrustedProxies())
	if len(allow) > 0 {
		log.Printf("house page on /house/ for %v (trusted proxies %v)", cfg.GetHouse().GetAllowCidrs(), cfg.GetHouse().GetTrustedProxies())
	}
	mux.Handle("/house/", &house.Handler{Store: st, API: api, Allow: allow, Proxies: proxies, DisplayName: cfg.DisplayName})
	root := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor == 2 && strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
			gs.ServeHTTP(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	})
	return &http.Server{Addr: cfg.Listen, Handler: h2c.NewHandler(root, &http2.Server{})}, gs
}

// mediaSetup builds the Frigate client and the link keyring from
// config and environment.  No frigate.url: no media.  No
// CURTILAGE_MEDIA_KEY: GetMedia works on the LAN, no links are minted.
func mediaSetup(cfg *curtilagev1.Config, creds config.Credentials) (*frigate.Client, *captoken.Keyring, error) {
	u := cfg.GetFrigate().GetUrl()
	if u == "" {
		log.Print("media: off (no frigate.url)")
		return nil, nil, nil
	}
	fc, err := frigate.NewClient(u)
	if err != nil {
		return nil, nil, err
	}
	if creds.MediaKey == nil {
		log.Printf("media: snapshots from %s; no capability links (CURTILAGE_MEDIA_KEY unset)", u)
		return fc, nil, nil
	}
	kr, err := captoken.New(creds.MediaKey, creds.MediaKeyPrior)
	if err != nil {
		return nil, nil, err
	}
	rot := ""
	if kr.HasPrior() {
		rot = ", rotation in progress (prior key accepted)"
	}
	log.Printf("media: snapshots from %s; links signed by key %s for %s%s", u, kr.CurrentKeyID(), cfg.GetLinks().GetTtl().AsDuration(), rot)
	return fc, kr, nil
}

func serve(srv *http.Server, listen string) {
	log.Printf("api, metrics and admin on %s", listen)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("http: %v", err)
	}
}

func shutdown(srv *http.Server, gs *grpc.Server) {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx) // open WatchEvents streams hold it until the deadline
	gs.Stop()
}

// pruneHourly forgets events, and deletes recordings, past retention.
func pruneHourly(ctx context.Context, st *store.Store, dir string) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			n := st.Prune(now)
			files := 0
			if dir != "" {
				var errs []error
				files, errs = store.PruneFiles(dir, st.Retention(), now)
				for _, e := range errs {
					log.Printf("prune: %v", e)
				}
			}
			if n > 0 || files > 0 {
				log.Printf("pruned %d events, %d recordings", n, files)
			}
		}
	}
}

// adminMux serves metrics, health, and the one mutating endpoint:
// POST /admin/rotate closes the current recording so it is complete
// (indexed, with a footer) and readable right now.  Unauthenticated --
// the listener is LAN only (docs/DESIGN.md).  rotator is nil when
// recording is off.
func adminMux(rotator *record.Rotator, st *store.Store, api *server.Server) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("/metrics", metrics.Handler(version, st))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })
	if api != nil {
		mux.Handle("/media/", api.MediaHandler()) // capability links; 404 for everything not signed
	}
	mux.HandleFunc("POST /admin/rotate", func(w http.ResponseWriter, r *http.Request) {
		if rotator == nil {
			http.Error(w, "recording is not enabled", http.StatusConflict)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		closed, err := rotator.Rotate(ctx)
		if err != nil {
			http.Error(w, "rotate: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		if closed == "" {
			fmt.Fprintln(w, "no file open; the next message starts a new one")
			return
		}
		log.Printf("rotated on request: closed %s", closed)
		fmt.Fprintln(w, "closed "+closed)
	})
	return mux
}

func cmdReplay(args []string) error {
	fs := flag.NewFlagSet("replay", flag.ContinueOnError)
	file := fs.String("file", "", "MCAP recording to summarize")
	topic := fs.String("topic", "", "only records on this topic (exact match)")
	dump := fs.Int("dump", 0, "print the first N matching records' payloads")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *file == "" {
		fs.Usage()
		return fmt.Errorf("-file is required")
	}
	recs, err := record.ReadAll(context.Background(), *file)
	if err != nil {
		if !errors.Is(err, record.ErrTruncated) && !errors.Is(err, record.ErrCorrupt) {
			return err
		}
		fmt.Printf("WARNING: %v\n", err) // torn tail or bad checksums; what follows is what survived
	}
	if *topic != "" {
		kept := recs[:0]
		for _, r := range recs {
			if r.GetTopic() == *topic {
				kept = append(kept, r)
			}
		}
		recs = kept
	}
	for i, r := range recs {
		if i >= *dump {
			break
		}
		fmt.Printf("--- %s %s (%d bytes)\n%s\n", r.GetReceivedAt().AsTime().UTC().Format(time.RFC3339Nano), r.GetTopic(), len(r.GetPayload()), r.GetPayload())
	}
	if len(recs) == 0 {
		fmt.Println("no records")
		return nil
	}
	byTopic := map[string]int{}
	var bytes int
	for _, r := range recs {
		byTopic[r.GetTopic()]++
		bytes += len(r.GetPayload())
	}
	first, last := recs[0].GetReceivedAt().AsTime(), recs[len(recs)-1].GetReceivedAt().AsTime()
	fmt.Printf("%d records, %d payload bytes, %s .. %s (%s)\n", len(recs), bytes,
		first.UTC().Format(time.RFC3339), last.UTC().Format(time.RFC3339), last.Sub(first).Round(time.Second))
	topics := make([]string, 0, len(byTopic))
	for t := range byTopic {
		topics = append(topics, t)
	}
	sort.Slice(topics, func(i, j int) bool { return byTopic[topics[i]] > byTopic[topics[j]] })
	for _, t := range topics {
		fmt.Printf("%8d  %s\n", byTopic[t], t)
	}
	return nil
}

// cmdTrim cuts a recording down to a window and a set of topics: the
// way a day of driveway traffic becomes a checked-in fixture.
func cmdTrim(args []string) error {
	fs := flag.NewFlagSet("trim", flag.ContinueOnError)
	file := fs.String("file", "", "MCAP recording to read")
	out := fs.String("out", "", "MCAP file to write")
	from := fs.String("from", "", "keep records received at or after this time (RFC 3339)")
	to := fs.String("to", "", "keep records received before this time (RFC 3339)")
	keep := fs.String("keep", "", "keep only topics matching this regexp (Go syntax)")
	drop := fs.String("drop", "", "drop topics matching this regexp, after -keep")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *file == "" || *out == "" {
		fs.Usage()
		return fmt.Errorf("-file and -out are required")
	}
	var fromT, toT time.Time
	var err error
	if *from != "" {
		if fromT, err = time.Parse(time.RFC3339, *from); err != nil {
			return fmt.Errorf("-from: %w", err)
		}
	}
	if *to != "" {
		if toT, err = time.Parse(time.RFC3339, *to); err != nil {
			return fmt.Errorf("-to: %w", err)
		}
	}
	var keepRE, dropRE *regexp.Regexp
	if *keep != "" {
		if keepRE, err = regexp.Compile(*keep); err != nil {
			return fmt.Errorf("-keep: %w", err)
		}
	}
	if *drop != "" {
		if dropRE, err = regexp.Compile(*drop); err != nil {
			return fmt.Errorf("-drop: %w", err)
		}
	}
	recs, err := record.ReadAll(context.Background(), *file)
	if err != nil && !errors.Is(err, record.ErrTruncated) && !errors.Is(err, record.ErrCorrupt) {
		return err
	}
	var kept []*record.Record
	for _, r := range recs {
		t := r.GetReceivedAt().AsTime()
		if (!fromT.IsZero() && t.Before(fromT)) || (!toT.IsZero() && !t.Before(toT)) {
			continue
		}
		if keepRE != nil && !keepRE.MatchString(r.GetTopic()) {
			continue
		}
		if dropRE != nil && dropRE.MatchString(r.GetTopic()) {
			continue
		}
		kept = append(kept, r)
	}
	if err := record.WriteFile(*out, kept); err != nil {
		return err
	}
	fmt.Printf("%d of %d records -> %s\n", len(kept), len(recs), *out)
	return nil
}
