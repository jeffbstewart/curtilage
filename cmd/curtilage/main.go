// Command curtilage is the server: it watches Frigate over MQTT,
// decides which events are news, and tells the household
// (docs/DESIGN.md).  Phase 1: ingest + record.
//
//	curtilage run -config curtilage.textproto
//	curtilage replay -file curtilage-20260829T120000Z.mcap
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
	"sort"
	"syscall"
	"time"

	"github.com/jeffbstewart/curtilage/internal/config"
	"github.com/jeffbstewart/curtilage/internal/metrics"
	"github.com/jeffbstewart/curtilage/internal/mqtt"
	"github.com/jeffbstewart/curtilage/internal/record"
)

// version is set by the build (-ldflags "-X main.version=...").
var version = "dev"

func usage() {
	fmt.Fprint(os.Stderr, `usage: curtilage <command> [flags]

  run     -config F          watch the broker; record when configured
  replay  -file F.mcap       summarize a recording
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
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgPath == "" {
		fs.Usage()
		return fmt.Errorf("-config is required")
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	creds, err := config.CredentialsFromEnv()
	if err != nil {
		return err
	}
	record.Library = versionString()

	// SIGTERM (kubernetes, docker stop) and SIGINT cancel the root
	// context; everything below drains and closes in order before exit.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	records := make(chan *record.Record, 1024)
	recErr := make(chan error, 1)
	var rotator *record.Rotator
	if dir := cfg.Recording.GetDir(); dir != "" {
		rotator = record.NewRotator()
		go func() {
			// Not ctx: the recorder must outlive the cancellation and
			// finish the file on the closed channel.
			recErr <- record.Write(context.Background(), record.Options{
				Dir: dir, RotateEvery: cfg.Recording.RotateEvery.AsDuration(), Rotate: rotator.Chan(),
			}, records)
		}()
		log.Printf("recording to %s (rotate every %s)", dir, cfg.Recording.RotateEvery.AsDuration())
	} else {
		go func() {
			for range records {
			}
			recErr <- nil
		}()
	}

	srv := &http.Server{Addr: cfg.Listen, Handler: adminMux(rotator)}
	go func() {
		log.Printf("metrics on %s/metrics", cfg.Listen)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("http: %v", err)
		}
	}()

	m := cfg.Mqtt
	mqttErr := mqtt.Run(ctx, mqtt.Options{
		Host: m.Host, Port: m.Port, ClientID: m.ClientId,
		User: creds.User, Password: creds.Password,
		Keepalive: m.Keepalive.AsDuration(), Subscriptions: m.Subscriptions,
		PublishPrefix: m.PublishPrefix,
	}, records) // Run closes records when it returns

	// Order matters: broker session gone -> channel closed -> recorder
	// drains and closes its MCAP -> then the HTTP server -> exit.
	werr := <-recErr
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	if mqttErr != nil {
		return mqttErr
	}
	if werr != nil {
		return fmt.Errorf("recorder: %w", werr)
	}
	log.Print("clean shutdown")
	return nil
}

// adminMux serves metrics, health, and the one mutating endpoint:
// POST /admin/rotate closes the current recording so it is complete
// (indexed, with a footer) and readable right now.  Unauthenticated --
// the listener is LAN only (docs/DESIGN.md).  rotator is nil when
// recording is off.
func adminMux(rotator *record.Rotator) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("/metrics", metrics.Handler(version))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })
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
