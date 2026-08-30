package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jeffbstewart/curtilage/internal/record"
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
	srv := httptest.NewServer(adminMux(rot))
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
	srv := httptest.NewServer(adminMux(nil))
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
