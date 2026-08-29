package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/didvc/uptime-mon/internal/lineproto"
	"github.com/didvc/uptime-mon/internal/model"
)

func sample(name string, at time.Time, up bool, rtt time.Duration) model.Result {
	st := model.StatusDown
	code := 500
	errText := "status 500 (want 200-299)"
	if up {
		st, code, errText = model.StatusUp, 200, ""
	}
	return model.Result{
		Target: name, Kind: model.KindHTTP, Host: "example.com", Group: "g",
		At: at, Status: st, Code: code, RTT: rtt, Attempt: 1, Err: errText,
	}
}

func TestWriteRotateCompressAndRead(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(Config{
		Dir:           dir,
		FlushInterval: time.Hour, // never fires; we flush explicitly
		Compress:      true,
		CompressLevel: 1,
		UTC:           true,
	})
	if err != nil {
		t.Fatal(err)
	}
	w.OnError = func(e error) { t.Errorf("background error: %v", e) }

	day1 := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)

	for i := range 5 {
		w.Add(sample("a", day1.Add(time.Duration(i)*time.Minute), i != 2, time.Duration(10+i)*time.Millisecond))
	}
	// A sample stamped with the next day seals day one.
	w.Add(sample("a", day2, true, 20*time.Millisecond))

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	files, err := List(dir, "uptime")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("expected 2 day files, got %+v", files)
	}
	if !files[0].Compressed || files[0].Day != "2026-08-27" {
		t.Errorf("day 1 should be sealed: %+v", files[0])
	}
	if files[1].Compressed || files[1].Day != "2026-08-28" {
		t.Errorf("day 2 should still be plain: %+v", files[1])
	}
	if _, err := os.Stat(filepath.Join(dir, "uptime-2026-08-27.lp")); !os.IsNotExist(err) {
		t.Error("uncompressed day 1 should have been removed")
	}

	// Read everything back, across the compression boundary.
	r := &Reader{Dir: dir, Precision: lineproto.Nanoseconds}
	var got []model.Result
	err = r.Scan(day1.Add(-24*time.Hour), day2.Add(24*time.Hour), func(res model.Result) error {
		got = append(got, res)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 6 {
		t.Fatalf("expected 6 samples, got %d", len(got))
	}
	down := 0
	for _, g := range got {
		if g.Status == model.StatusDown {
			down++
			if g.Err == "" {
				t.Error("down sample lost its error text")
			}
		}
		if g.Target != "a" || g.Host != "example.com" || g.Group != "g" {
			t.Errorf("tags lost: %+v", g)
		}
	}
	if down != 1 {
		t.Errorf("expected 1 down sample, got %d", down)
	}
}

func TestScanTimeWindowFilters(t *testing.T) {
	dir := t.TempDir()
	w, _ := Open(Config{Dir: dir, FlushInterval: time.Hour, UTC: true})
	base := time.Date(2026, 8, 27, 0, 0, 0, 0, time.UTC)
	for i := range 60 {
		w.Add(sample("a", base.Add(time.Duration(i)*time.Minute), true, time.Millisecond))
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	r := &Reader{Dir: dir}
	n := 0
	err := r.Scan(base.Add(10*time.Minute), base.Add(20*time.Minute), func(model.Result) error {
		n++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != 10 { // [10, 20)
		t.Fatalf("expected 10 samples in window, got %d", n)
	}
}

func TestFlushBytesValve(t *testing.T) {
	dir := t.TempDir()
	w, _ := Open(Config{Dir: dir, FlushInterval: time.Hour, FlushBytes: 512, UTC: true})
	defer w.Close()

	at := time.Now()
	for i := range 200 {
		w.Add(sample("valve", at.Add(time.Duration(i)*time.Millisecond), true, time.Millisecond))
	}
	if b := w.Buffered(); b >= 512 {
		t.Fatalf("size valve did not fire: %d bytes buffered", b)
	}
	if w.Written() != 200 {
		t.Fatalf("written = %d", w.Written())
	}
}

func TestSealStaleDaysOnOpen(t *testing.T) {
	dir := t.TempDir()
	// Simulate a previous run killed before rotation.
	stale := filepath.Join(dir, "uptime-2020-01-01.lp")
	if err := os.WriteFile(stale, []byte("uptime,endpoint=x up=1i,rtt=1.000 1577880000000000000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w, err := Open(Config{Dir: dir, FlushInterval: time.Hour, Compress: true, UTC: true})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("stale plain file should have been compressed away")
	}
	if _, err := os.Stat(filepath.Join(dir, "uptime-2020-01-01.lp.zst")); err != nil {
		t.Errorf("expected sealed file: %v", err)
	}
}

func TestRetention(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().AddDate(0, 0, -10).Format("2006-01-02")
	recent := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	for _, d := range []string{old, recent} {
		if err := os.WriteFile(filepath.Join(dir, "uptime-"+d+".lp.zst"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	w, err := Open(Config{Dir: dir, FlushInterval: time.Hour, RetentionDays: 5})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	files, _ := List(dir, "uptime")
	if len(files) != 1 || files[0].Day != recent {
		t.Fatalf("retention kept the wrong files: %+v", files)
	}
}

func TestKeepUncompressedReadsEachDayOnce(t *testing.T) {
	dir := t.TempDir()
	w, _ := Open(Config{
		Dir: dir, FlushInterval: time.Hour, Compress: true,
		KeepUncompressed: true, CompressLevel: 1, UTC: true,
	})
	day1 := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	w.Add(sample("a", day1, true, time.Millisecond))
	w.Add(sample("a", day1.AddDate(0, 0, 1), true, time.Millisecond))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	n := 0
	r := &Reader{Dir: dir}
	if err := r.Scan(day1.Add(-time.Hour), day1.AddDate(0, 0, 2), func(model.Result) error {
		n++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("expected each day counted once, got %d samples", n)
	}
}
