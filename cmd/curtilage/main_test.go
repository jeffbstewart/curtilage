package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/timestamppb"

	curtilagev1 "github.com/jeffbstewart/curtilage/gen/curtilage/v1"
	"github.com/jeffbstewart/curtilage/internal/record"
	"github.com/jeffbstewart/curtilage/internal/store"
)

func TestVersionString(t *testing.T) {
	if got := versionString(); got != "curtilage dev" {
		t.Fatalf("versionString() = %q", got)
	}
}

func TestRotateEndpoint(t *testing.T) {
	in := make(chan *record.Record)
	rot := record.NewRotator()
	errc := make(chan error, 1)
	go func() {
		errc <- record.Write(context.Background(), record.Options{Dir: t.TempDir(), RotateEvery: time.Hour, Rotate: rot.Chan()}, in)
	}()
	srv := httptest.NewServer(adminMux(rot, nil, nil))
	defer srv.Close()

	post := func() (int, string) {
		t.Helper()
		resp, err := http.Post(srv.URL+"/admin/rotate", "", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, string(body)
	}

	if code, body := post(); code != 200 || !strings.Contains(body, "no file open") {
		t.Fatalf("rotate before any record: %d %q", code, body)
	}
	in <- &record.Record{ReceivedAt: timestamppb.Now(), Topic: "a", Payload: []byte("1")}
	if code, body := post(); code != 200 || !strings.HasPrefix(body, "closed ") {
		t.Fatalf("rotate with a file open: %d %q", code, body)
	}
	if resp, err := http.Get(srv.URL + "/admin/rotate"); err != nil || resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET /admin/rotate must be rejected: %v %v", resp, err)
	}
	close(in)
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
}

func TestRotateEndpointWithoutRecording(t *testing.T) {
	srv := httptest.NewServer(adminMux(nil, nil, nil))
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/admin/rotate", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409 when recording is off", resp.StatusCode)
	}
}

// One listener, two protocols: gRPC over h2c and plain HTTP/1.1.
func TestOneListenerServesGRPCAndHTTP(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &curtilagev1.Config{Listen: lis.Addr().String(), DisplayName: "house"}
	srv, gs := httpServer(cfg, store.New(time.Hour), nil, nil, nil, nil, nil)
	go srv.Serve(lis)
	t.Cleanup(func() { shutdown(srv, gs) })

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	info, err := curtilagev1.NewCurtilageServiceClient(conn).GetServerInfo(context.Background(),
		&curtilagev1.GetServerInfoRequest{ApiVersion: 1, Platform: curtilagev1.Platform_PLATFORM_IOS})
	if err != nil || info.GetDisplayName() != "house" {
		t.Fatalf("gRPC over h2c: %v, %v", info, err)
	}
	resp, err := http.Get("http://" + lis.Addr().String() + "/healthz")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("HTTP/1.1 on the same port: %v, %v", resp, err)
	}
	resp.Body.Close()
	resp, err = http.Get("http://" + lis.Addr().String() + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	// The root is a landing page; anything else unknown is still a 404.
	for path, want := range map[string]int{"/": 200, "/nope": 404, "/house": 404, "/media/x": 404} {
		resp, err := http.Get("http://" + lis.Addr().String() + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("GET %s -> %d, want %d", path, resp.StatusCode, want)
		}
		if path == "/" && (!strings.Contains(string(b), "<title>house</title>") || !strings.Contains(string(b), "/house/")) {
			t.Errorf("root page:\n%s", b)
		}
	}
	if !strings.Contains(string(body), "curtilage_events 0") || !strings.Contains(string(body), "curtilage_retention_seconds 3600") {
		t.Errorf("metrics lack store gauges:\n%s", body)
	}
}
