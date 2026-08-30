package record

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	curtilagev1 "github.com/jeffbstewart/curtilage/gen/curtilage/v1"
)

func sample(n int, base time.Time) []*Record {
	topics := []string{"frigate/events", "frigate/garage/person", "frigate/available"}
	var recs []*Record
	for i := 0; i < n; i++ {
		recs = append(recs, &Record{
			ReceivedAt: timestamppb.New(base.Add(time.Duration(i) * time.Millisecond)),
			Topic:      topics[i%len(topics)],
			Retained:   i%3 == 2,
			Qos:        curtilagev1.Qos(i%3 + 1),                 // the three real levels, never UNSPECIFIED
			Payload:    []byte{byte(i), 0xff, 0x00, byte(i * 7)}, // arbitrary bytes, not text
		})
		recs[i].PayloadCrc32C = Checksum(recs[i].Payload)
	}
	return recs
}

func TestWriteThenReadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	want := sample(50, base)

	in := make(chan *Record)
	errc := make(chan error, 1)
	go func() { errc <- Write(context.Background(), Options{Dir: dir, RotateEvery: 24 * time.Hour}, in) }()
	for _, r := range want {
		in <- r
	}
	close(in) // the clean-shutdown contract: producer closes, writer finishes the file
	if err := <-errc; err != nil {
		t.Fatal(err)
	}

	files, _ := filepath.Glob(filepath.Join(dir, "curtilage-*.mcap"))
	if len(files) != 1 {
		t.Fatalf("want 1 file, got %v", files)
	}
	got, err := ReadAll(context.Background(), files[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d records, want %d", len(got), len(want))
	}
	for i := range want {
		if !proto.Equal(got[i], want[i]) {
			t.Errorf("record %d differs:\n got %v\nwant %v", i, got[i], want[i])
		}
	}
	w, f, e := Stats()
	if w < 50 || f < 1 || e != 0 {
		t.Errorf("stats = %d %d %d", w, f, e)
	}
}

func TestRotation(t *testing.T) {
	dir := t.TempDir()
	in := make(chan *Record)
	errc := make(chan error, 1)
	go func() { errc <- Write(context.Background(), Options{Dir: dir, RotateEvery: 50 * time.Millisecond}, in) }()
	in <- &Record{ReceivedAt: timestamppb.Now(), Topic: "a", Payload: []byte("1")}
	time.Sleep(120 * time.Millisecond)
	in <- &Record{ReceivedAt: timestamppb.Now(), Topic: "a", Payload: []byte("2")}
	close(in)
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "curtilage-*.mcap"))
	if len(files) != 2 {
		t.Fatalf("want 2 files after rotation, got %v", files)
	}
	for _, f := range files {
		recs, err := ReadAll(context.Background(), f)
		if err != nil || len(recs) != 1 {
			t.Errorf("%s: %d records, %v", f, len(recs), err)
		}
	}
}

func TestIdleRotation(t *testing.T) {
	// No traffic after the first record: the timer alone must close
	// the over-age file, and the next record must open a new one.
	dir := t.TempDir()
	in := make(chan *Record)
	errc := make(chan error, 1)
	go func() { errc <- Write(context.Background(), Options{Dir: dir, RotateEvery: 50 * time.Millisecond}, in) }()
	in <- &Record{ReceivedAt: timestamppb.Now(), Topic: "a", Payload: []byte("1")}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if recs, err := ReadAll(context.Background(), firstFile(t, dir)); err == nil && len(recs) == 1 {
			break // complete (footer present) without any further write
		}
		if time.Now().After(deadline) {
			t.Fatal("idle file was never rotated")
		}
		time.Sleep(10 * time.Millisecond)
	}
	in <- &Record{ReceivedAt: timestamppb.Now(), Topic: "a", Payload: []byte("2")}
	close(in)
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if files, _ := filepath.Glob(filepath.Join(dir, "curtilage-*.mcap")); len(files) != 2 {
		t.Fatalf("want 2 files, got %v", files)
	}
}

func TestForcedRotation(t *testing.T) {
	dir := t.TempDir()
	in := make(chan *Record)
	errc := make(chan error, 1)
	rot := NewRotator()
	go func() {
		errc <- Write(context.Background(), Options{Dir: dir, RotateEvery: time.Hour, Rotate: rot.Chan()}, in)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if p, err := rot.Rotate(ctx); err != nil || p != "" {
		t.Fatalf("rotate with no file open = %q, %v; want \"\", nil", p, err)
	}
	in <- &Record{ReceivedAt: timestamppb.Now(), Topic: "a", Payload: []byte("1")}
	p, err := rot.Rotate(ctx)
	if err != nil || p == "" {
		t.Fatalf("rotate = %q, %v; want the closed file's path", p, err)
	}
	if recs, err := ReadAll(context.Background(), p); err != nil || len(recs) != 1 {
		t.Fatalf("%s after forced rotation: %d records, %v", p, len(recs), err)
	}
	in <- &Record{ReceivedAt: timestamppb.Now(), Topic: "a", Payload: []byte("2")}
	close(in)
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if files, _ := filepath.Glob(filepath.Join(dir, "curtilage-*.mcap")); len(files) != 2 {
		t.Fatalf("want 2 files, got %v", files)
	}
}

func firstFile(t *testing.T, dir string) string {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(dir, "curtilage-*.mcap"))
	if len(files) == 0 {
		t.Fatal("no recording yet")
	}
	return files[0]
}

func TestCancelStillClosesFile(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	in := make(chan *Record)
	errc := make(chan error, 1)
	go func() { errc <- Write(ctx, Options{Dir: dir, RotateEvery: time.Hour}, in) }()
	in <- &Record{ReceivedAt: timestamppb.Now(), Topic: "a", Payload: []byte("x")}
	cancel() // abort path: no close(in), but the file must still be complete
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "curtilage-*.mcap"))
	if len(files) != 1 {
		t.Fatalf("want 1 file, got %v", files)
	}
	if recs, err := ReadAll(context.Background(), files[0]); err != nil || len(recs) != 1 {
		t.Errorf("file not readable after cancel: %d records, %v", len(recs), err)
	}
}

func TestWriteFileRoundTrip(t *testing.T) {
	want := sample(30, time.Date(2026, 8, 30, 11, 0, 0, 0, time.UTC))
	for _, r := range want {
		r.PayloadCrc32C = 0 // WriteFile stamps it
	}
	p := filepath.Join(t.TempDir(), "trimmed.mcap")
	if err := WriteFile(p, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadAll(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d records, want %d", len(got), len(want))
	}
	for i := range want {
		if !proto.Equal(got[i], want[i]) {
			t.Errorf("record %d differs:\n got %v\nwant %v", i, got[i], want[i])
		}
	}
	// Overwrites rather than appends.
	if err := WriteFile(p, want[:3]); err != nil {
		t.Fatal(err)
	}
	if got, _ := ReadAll(context.Background(), p); len(got) != 3 {
		t.Errorf("after rewrite: %d records, want 3", len(got))
	}
}

func TestFileName(t *testing.T) {
	got := FileName(time.Date(2026, 8, 29, 22, 5, 9, 0, time.FixedZone("x", -4*3600)))
	if got != "curtilage-20260830T020509Z.mcap" {
		t.Errorf("FileName = %q (must be UTC)", got)
	}
	want := time.Date(2026, 8, 30, 2, 5, 9, 0, time.UTC)
	for _, name := range []string{got, "curtilage-20260830T020509Z-1.mcap"} {
		if start, ok := ParseFileName(name); !ok || !start.Equal(want) {
			t.Errorf("ParseFileName(%q) = %v, %v", name, start, ok)
		}
	}
	for _, name := range []string{"cut.mcap", "curtilage-2026.mcap", "curtilage-20260830T020509Z.txt", "notes.txt"} {
		if _, ok := ParseFileName(name); ok {
			t.Errorf("ParseFileName(%q) accepted", name)
		}
	}
}

func TestReadRejectsGarbage(t *testing.T) {
	p := filepath.Join(t.TempDir(), "junk.mcap")
	os.WriteFile(p, []byte("not an mcap"), 0o644)
	if _, err := ReadAll(context.Background(), p); err == nil {
		t.Error("expected an error for a non-MCAP file")
	}
}

func TestTruncatedFileStillYieldsRecords(t *testing.T) {
	dir := t.TempDir()
	in := make(chan *Record)
	errc := make(chan error, 1)
	go func() { errc <- Write(context.Background(), Options{Dir: dir, RotateEvery: time.Hour}, in) }()
	for _, r := range sample(20, time.Now()) {
		in <- r
	}
	close(in)
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "curtilage-*.mcap"))
	b, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	// Chop the summary and footer off, as a SIGKILL mid-write would.
	cut := filepath.Join(dir, "cut.mcap")
	if err := os.WriteFile(cut, b[:len(b)-len(b)/3], 0o644); err != nil {
		t.Fatal(err)
	}
	recs, err := ReadAll(context.Background(), cut)
	if err == nil || !errors.Is(err, ErrTruncated) {
		t.Fatalf("want ErrTruncated, got %v", err)
	}
	if len(recs) == 0 {
		t.Fatal("no records recovered from the truncated file")
	}
	t.Logf("recovered %d of 20 records from a file cut to 2/3", len(recs))
}

func TestChecksumIsCRC32C(t *testing.T) {
	// The CRC-32C check value from RFC 3720 (iSCSI) for "123456789".
	if got := Checksum([]byte("123456789")); got != 0xE3069283 {
		t.Fatalf("Checksum = %#x, want 0xE3069283 (Castagnoli)", got)
	}
}

func TestCorruptRecordIsSkippedAndReported(t *testing.T) {
	dir := t.TempDir()
	f, err := open(dir, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	good := &Record{ReceivedAt: timestamppb.Now(), Topic: "t", Payload: []byte("good")}
	good.PayloadCrc32C = Checksum(good.Payload)
	bad := &Record{ReceivedAt: timestamppb.Now(), Topic: "t", Payload: []byte("bad"), PayloadCrc32C: 0xdeadbeef}
	for _, r := range []*Record{good, bad, good} {
		if err := f.write(r); err != nil { // bypasses Write's stamping on purpose
			t.Fatal(err)
		}
	}
	if err := f.close(); err != nil {
		t.Fatal(err)
	}
	recs, err := ReadAll(context.Background(), f.path)
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("want ErrCorrupt, got %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("want the 2 good records delivered, got %d", len(recs))
	}
	if strings.Contains(err.Error(), "truncated") {
		t.Errorf("a complete file must not also report truncation: %v", err)
	}
}
