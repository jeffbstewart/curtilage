// Package record writes received MQTT messages to MCAP files and reads
// them back (docs/DESIGN.md "Instrument before tuning").
//
// Write's interface is a channel: the producer sends *Record values
// and CLOSES the channel on shutdown; Write drains, closes the current
// file (an MCAP file is only complete once its summary and footer are
// written) and returns.  One MCAP channel per MQTT topic, protobuf
// encoded with the curtilage.v1.Record schema embedded, zstd chunks.
package record

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/foxglove/mcap/go/mcap"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"

	curtilagev1 "github.com/jeffbstewart/curtilage/gen/curtilage/v1"
)

// Record is the message type recorded and replayed.
type Record = curtilagev1.Record

// Library is written into every file's header; set from main.
var Library = "curtilage"

const (
	schemaName  = "curtilage.v1.Record"
	fileProfile = "" // no MCAP profile: not ROS, just protobuf channels
)

// Counters for /metrics.
var (
	recordsWritten atomic.Uint64
	filesOpened    atomic.Uint64
	writeErrors    atomic.Uint64
)

// Stats returns the counters for /metrics.
func Stats() (records, files, errors uint64) {
	return recordsWritten.Load(), filesOpened.Load(), writeErrors.Load()
}

// Write drains in into MCAP files under dir, starting a new file every
// rotateEvery, until in is closed (clean shutdown) or ctx is cancelled
// (abort: the current file is still closed properly).  Returns the
// first fatal error; a file that cannot be opened is fatal, a message
// that cannot be marshalled is counted and skipped.
func Write(ctx context.Context, dir string, rotateEvery time.Duration, in <-chan *Record) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	var f *file
	closeCurrent := func() error {
		if f == nil {
			return nil
		}
		err := f.close()
		f = nil
		return err
	}
	for {
		var rec *Record
		var ok bool
		select {
		case <-ctx.Done():
			return closeCurrent()
		case rec, ok = <-in:
			if !ok {
				return closeCurrent()
			}
		}
		now := time.Now()
		if f != nil && now.Sub(f.started) >= rotateEvery {
			if err := closeCurrent(); err != nil {
				return err
			}
		}
		if f == nil {
			nf, err := open(dir, now)
			if err != nil {
				return err
			}
			f = nf
		}
		if err := f.write(rec); err != nil {
			writeErrors.Add(1)
			continue
		}
		recordsWritten.Add(1)
	}
}

// file is one open MCAP file.
type file struct {
	started  time.Time
	path     string
	osf      *os.File
	w        *mcap.Writer
	channels map[string]uint16 // MQTT topic -> MCAP channel id
	seq      map[uint16]uint32
}

// FileName is the name of the file started at t.
func FileName(t time.Time) string {
	return "curtilage-" + t.UTC().Format("20060102T150405Z") + ".mcap"
}

func open(dir string, now time.Time) (*file, error) {
	// Names have second resolution; a rotation within the same second
	// (tests, or a tiny rotate_every) gets a numeric suffix rather than
	// a collision.  O_EXCL makes the check atomic.
	base := FileName(now)
	var path string
	var osf *os.File
	for i := 0; ; i++ {
		name := base
		if i > 0 {
			name = fmt.Sprintf("%s-%d.mcap", base[:len(base)-len(".mcap")], i)
		}
		path = filepath.Join(dir, name)
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			osf = f
			break
		}
		if !errors.Is(err, os.ErrExist) || i >= 1000 {
			return nil, err
		}
	}
	w, err := mcap.NewWriter(osf, &mcap.WriterOptions{
		Chunked:     true,
		Compression: mcap.CompressionZSTD,
		IncludeCRC:  true,
	})
	if err != nil {
		osf.Close()
		return nil, err
	}
	if err := w.WriteHeader(&mcap.Header{Profile: fileProfile, Library: Library}); err != nil {
		osf.Close()
		return nil, err
	}
	fds, err := schemaDescriptorSet()
	if err != nil {
		osf.Close()
		return nil, err
	}
	if err := w.WriteSchema(&mcap.Schema{ID: 1, Name: schemaName, Encoding: "protobuf", Data: fds}); err != nil {
		osf.Close()
		return nil, err
	}
	filesOpened.Add(1)
	return &file{started: now, path: path, osf: osf, w: w, channels: map[string]uint16{}, seq: map[uint16]uint32{}}, nil
}

func (f *file) channel(topic string) (uint16, error) {
	if id, ok := f.channels[topic]; ok {
		return id, nil
	}
	id := uint16(len(f.channels) + 1)
	if err := f.w.WriteChannel(&mcap.Channel{ID: id, SchemaID: 1, Topic: topic, MessageEncoding: "protobuf"}); err != nil {
		return 0, err
	}
	f.channels[topic] = id
	return id, nil
}

func (f *file) write(rec *Record) error {
	data, err := proto.Marshal(rec)
	if err != nil {
		return err
	}
	id, err := f.channel(rec.GetTopic())
	if err != nil {
		return err
	}
	f.seq[id]++
	ns := uint64(rec.GetReceivedAt().AsTime().UnixNano())
	return f.w.WriteMessage(&mcap.Message{ChannelID: id, Sequence: f.seq[id], LogTime: ns, PublishTime: ns, Data: data})
}

// close finishes the MCAP (summary, footer) and the OS file.
func (f *file) close() error {
	werr := f.w.Close()
	cerr := f.osf.Close()
	if werr != nil {
		return fmt.Errorf("close %s: %w", f.path, werr)
	}
	if cerr != nil {
		return fmt.Errorf("close %s: %w", f.path, cerr)
	}
	return nil
}

// schemaDescriptorSet is the MCAP protobuf schema convention: a
// serialized FileDescriptorSet holding the message's file and every
// transitive dependency, so any reader can decode without our code.
func schemaDescriptorSet() ([]byte, error) {
	set := &descriptorpb.FileDescriptorSet{}
	seen := map[string]bool{}
	var add func(fd protoreflect.FileDescriptor)
	add = func(fd protoreflect.FileDescriptor) {
		if seen[fd.Path()] {
			return
		}
		seen[fd.Path()] = true
		imports := fd.Imports()
		for i := 0; i < imports.Len(); i++ {
			add(imports.Get(i).FileDescriptor)
		}
		set.File = append(set.File, protodesc.ToFileDescriptorProto(fd))
	}
	add(curtilagev1.File_curtilage_v1_record_proto)
	return proto.Marshal(set)
}

// ErrTruncated is returned by Read when the file has no valid summary
// or footer (the writer was killed before Close): every record that
// was fully written has still been delivered, in file order.
var ErrTruncated = errors.New("mcap file is truncated (no footer); records recovered in file order")

// Read replays every record in one MCAP file, in log-time order, to
// out; it does not close out (the caller may replay several files).
// A truncated file is read in file order as far as it goes and Read
// then returns ErrTruncated.
func Read(ctx context.Context, path string, out chan<- *Record) error {
	osf, err := os.Open(path)
	if err != nil {
		return err
	}
	defer osf.Close()
	r, err := mcap.NewReader(osf)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	defer r.Close()
	truncated := false
	it, err := r.Messages(mcap.UsingIndex(true), mcap.InOrder(mcap.LogTimeOrder))
	if err != nil {
		// No usable summary/index: the writer never closed the file.
		// The data section is still a valid stream; read it linearly.
		truncated = true
		if _, err := osf.Seek(0, io.SeekStart); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if r, err = mcap.NewReader(osf); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if it, err = r.Messages(mcap.UsingIndex(false), mcap.InOrder(mcap.FileOrder)); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	var msg mcap.Message
	for {
		schema, _, m, err := it.NextInto(&msg)
		if err != nil {
			if errors.Is(err, io.EOF) {
				if truncated {
					return fmt.Errorf("%s: %w", path, ErrTruncated)
				}
				return nil
			}
			if truncated {
				// The torn tail: everything before it was delivered.
				return fmt.Errorf("%s: %w", path, ErrTruncated)
			}
			return fmt.Errorf("%s: %w", path, err)
		}
		if schema == nil || schema.Name != schemaName {
			continue // not ours; a foreign channel in a hand-made file
		}
		rec := &Record{}
		if err := proto.Unmarshal(m.Data, rec); err != nil {
			return fmt.Errorf("%s: record: %w", path, err)
		}
		select {
		case out <- rec:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// ReadAll is Read into a slice, for tests and the replay summary.
func ReadAll(ctx context.Context, path string) ([]*Record, error) {
	ch := make(chan *Record, 64)
	var recs []*Record
	done := make(chan struct{})
	go func() {
		for r := range ch {
			recs = append(recs, r)
		}
		close(done)
	}()
	err := Read(ctx, path, ch)
	close(ch)
	<-done
	return recs, err
}
