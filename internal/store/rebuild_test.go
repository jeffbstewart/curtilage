package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jeffbstewart/curtilage/internal/fixture"
	"github.com/jeffbstewart/curtilage/internal/policy"
	"github.com/jeffbstewart/curtilage/internal/record"
)

func copyFixture(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(fixture.Path(t, fixture.Driveway))
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRebuildFromRecordingsAndPrunesOld(t *testing.T) {
	dir := t.TempDir()
	// The fixture is 2026-08-30 11:00-13:00; name a copy as that day's
	// file, and another as a week-old file that must be deleted unread.
	fresh := copyFixture(t, dir, record.FileName(time.Date(2026, 8, 30, 11, 0, 0, 0, time.UTC)))
	stale := copyFixture(t, dir, record.FileName(time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)))
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("ignored"), 0o644)

	s := New(7 * 24 * time.Hour)
	now := time.Date(2026, 8, 30, 13, 0, 0, 0, time.UTC)
	res, errs := Rebuild(context.Background(), s, policy.NewPassthrough(), dir, now)
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	if res.Files != 1 || res.Deleted != 1 || res.Records == 0 || res.Truncated != 0 {
		t.Fatalf("rebuilt %+v", res)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("stale recording not deleted")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("fresh recording deleted")
	}
	st := s.Stats()
	if st.Events != 84 || st.Live != 4 {
		t.Errorf("after rebuild: %+v (fixture has 84 objects, 4 still live)", st)
	}
	if cams := s.Cameras(); len(cams) < 4 {
		t.Errorf("cameras discovered: %v", cams)
	}
	page, _, _ := s.List(nil, 1, "")
	if len(page) != 1 || page[0].StartedAt.Before(time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("newest event %+v", page)
	}
}

func TestRebuildToleratesTruncatedAndMissingDir(t *testing.T) {
	dir := t.TempDir()
	p := copyFixture(t, dir, record.FileName(time.Date(2026, 8, 30, 11, 0, 0, 0, time.UTC)))
	b, _ := os.ReadFile(p)
	os.WriteFile(p, b[:len(b)*2/3], 0o644) // torn: the file being written right now
	s := New(7 * 24 * time.Hour)
	res, errs := Rebuild(context.Background(), s, policy.NewPassthrough(), dir, time.Date(2026, 8, 30, 13, 0, 0, 0, time.UTC))
	if len(errs) != 0 || res.Truncated != 1 || res.Files != 1 || res.Records == 0 {
		t.Fatalf("torn file: %+v %v", res, errs)
	}
	if s.Stats().Events == 0 {
		t.Error("nothing recovered from the torn file")
	}

	res, errs = Rebuild(context.Background(), New(time.Hour), policy.NewPassthrough(), filepath.Join(dir, "nope"), time.Now())
	if len(errs) != 0 || res.Files != 0 {
		t.Errorf("missing dir: %+v %v", res, errs)
	}
}

func TestPruneFiles(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 8, 30, 13, 0, 0, 0, time.UTC)
	keep := copyFixture(t, dir, record.FileName(now.Add(-6*24*time.Hour)))
	old := copyFixture(t, dir, record.FileName(now.Add(-8*24*time.Hour)))
	n, errs := PruneFiles(dir, 7*24*time.Hour, now)
	if n != 1 || len(errs) != 0 {
		t.Fatalf("pruned %d, %v", n, errs)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("old file kept")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Error("recent file deleted")
	}
}
