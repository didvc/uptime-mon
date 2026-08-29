// Package store persists Results as daily line-protocol files and reads them
// back.
//
// The storage model is deliberately dumb: one append-only text file per day,
// compressed with zstd once the day is over.
//
//	data/uptime-2026-08-29.lp      today, open, uncompressed
//	data/uptime-2026-08-28.lp.zst  yesterday, sealed
//
// There is no index, no WAL and no background compaction, because there is no
// query pattern that needs one — every question this tool answers is "scan a
// date range", and a day of 90 endpoints at 60s is about 130k samples, which
// scans in well under a second and compresses to a few hundred kilobytes.
//
// Writes are batched (default: ten minutes) so a monitor running for months
// does not turn into a continuous stream of small writes to a spinning disk or
// an SSD's erase blocks. The trade is explicit: a crash loses at most one
// flush interval of samples. Lower -flush-interval, or set -fsync, to trade
// that back for I/O.
package store

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/didvc/uptime-mon/internal/lineproto"
	"github.com/didvc/uptime-mon/internal/model"
	"github.com/klauspost/compress/zstd"
)

const (
	// Extensions used on disk.
	extPlain = ".lp"
	extZstd  = ".lp.zst"

	dayLayout = "2006-01-02"
)

// Config controls the writer. Zero values are replaced with the defaults
// documented on each field.
type Config struct {
	Dir    string // required
	Prefix string // filename prefix, default "uptime"

	Measurement string              // default "uptime"
	Precision   lineproto.Precision // default ns
	ExtraTags   []model.Header      // static tags on every point

	// FlushInterval is the batching window. Default 10m.
	FlushInterval time.Duration
	// FlushBytes forces an early flush when the buffer grows past this.
	// Default 4 MiB — a safety valve, not the normal path.
	FlushBytes int
	// Fsync calls File.Sync after every flush. Off by default: the OS page
	// cache survives a process crash, and only a machine crash loses it.
	Fsync bool

	// Compress rotates completed days through zstd. Default true.
	Compress         bool
	CompressLevel    int  // zstd level 1..19, default 3
	KeepUncompressed bool // keep the .lp alongside the .zst

	// RetentionDays deletes day files older than this. 0 keeps everything.
	RetentionDays int

	// UTC puts day boundaries at UTC midnight instead of local midnight.
	UTC bool
}

func (c *Config) applyDefaults() {
	if c.Prefix == "" {
		c.Prefix = "uptime"
	}
	if c.Measurement == "" {
		c.Measurement = lineproto.DefaultMeasurement
	}
	if c.Precision == "" {
		c.Precision = lineproto.Nanoseconds
	}
	if c.FlushInterval <= 0 {
		c.FlushInterval = 10 * time.Minute
	}
	if c.FlushBytes <= 0 {
		c.FlushBytes = 4 << 20
	}
	if c.CompressLevel <= 0 {
		c.CompressLevel = 3
	}
}

// Writer batches Results into the current day's file.
type Writer struct {
	cfg Config
	enc *lineproto.Encoder

	mu     sync.Mutex
	buf    []byte
	day    string   // day key of the currently open file
	file   *os.File //
	closed bool

	// Written counts samples accepted since start, for the status line.
	written uint64

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}

	// compressWG lets Close wait for background compression to finish so the
	// process does not exit mid-rotation and leave a truncated .zst.
	compressWG sync.WaitGroup

	// OnError receives background failures (flush, rotate, compress). The
	// collector logs them; nothing else can be done usefully at that point.
	OnError func(error)
}

// Open prepares the data directory and starts the flush loop.
func Open(cfg Config) (*Writer, error) {
	cfg.applyDefaults()
	if cfg.Dir == "" {
		return nil, errors.New("store: Dir is required")
	}
	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		return nil, err
	}

	w := &Writer{
		cfg:  cfg,
		enc:  lineproto.NewEncoder(cfg.Measurement, cfg.Precision, cfg.ExtraTags),
		buf:  make([]byte, 0, 64<<10),
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}

	// A previous run may have been killed before it could seal its day files.
	// Do that now, before opening today's, so the directory is always in the
	// expected shape.
	w.sealStaleDays()
	if err := w.gc(); err != nil {
		w.reportError(err)
	}

	go w.loop()
	return w, nil
}

func (w *Writer) reportError(err error) {
	if err == nil {
		return
	}
	if w.OnError != nil {
		w.OnError(err)
	}
}

// dayKey returns the file-naming day for t, in the configured timezone.
func (w *Writer) dayKey(t time.Time) string {
	if w.cfg.UTC {
		return t.UTC().Format(dayLayout)
	}
	return t.Local().Format(dayLayout)
}

// Add buffers one result. It never blocks on disk I/O; the flush loop and the
// size valve do the writing.
func (w *Writer) Add(r model.Result) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}

	// Rotation is driven by the data, not by a wall-clock timer: the first
	// sample stamped with a new day seals the old file. Only ever move
	// forward, so a straggler probe that started before midnight and finished
	// after it cannot re-open yesterday.
	day := w.dayKey(r.At)
	if w.day == "" {
		// Name the file after the data it will hold, not after the moment the
		// writer happened to be constructed.
		w.day = day
	}
	if day > w.day {
		if err := w.flushLocked(); err != nil {
			w.reportError(err)
		}
		if err := w.rotateLocked(day); err != nil {
			w.reportError(err)
		}
	}

	w.buf = w.enc.AppendResult(w.buf, r)
	w.written++

	if len(w.buf) >= w.cfg.FlushBytes {
		if err := w.flushLocked(); err != nil {
			w.reportError(err)
		}
	}
}

// Written reports how many samples have been accepted.
func (w *Writer) Written() uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.written
}

// Buffered reports how many bytes are waiting for the next flush. The TUI
// shows this so the batching is visible rather than mysterious.
func (w *Writer) Buffered() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.buf)
}

// Flush writes buffered samples to disk immediately.
func (w *Writer) Flush() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.flushLocked()
}

func (w *Writer) flushLocked() error {
	if len(w.buf) == 0 {
		return nil
	}
	if w.file == nil {
		// Open lazily against the day of the data we are about to write.
		if w.day == "" {
			w.day = w.dayKey(time.Now())
		}
		if err := w.openLocked(w.day); err != nil {
			return err
		}
	}
	if _, err := w.file.Write(w.buf); err != nil {
		return fmt.Errorf("store: write %s: %w", w.file.Name(), err)
	}
	w.buf = w.buf[:0]
	if w.cfg.Fsync {
		if err := w.file.Sync(); err != nil {
			return fmt.Errorf("store: fsync: %w", err)
		}
	}
	return nil
}

func (w *Writer) openLocked(day string) error {
	path := filepath.Join(w.cfg.Dir, w.cfg.Prefix+"-"+day+extPlain)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("store: open %s: %w", path, err)
	}
	w.file, w.day = f, day
	return nil
}

// rotateLocked closes the current day and hands it to the compressor.
func (w *Writer) rotateLocked(newDay string) error {
	old := w.file
	oldDay := w.day
	w.file, w.day = nil, newDay
	if old == nil {
		return nil
	}
	name := old.Name()
	if err := old.Close(); err != nil {
		return fmt.Errorf("store: close %s: %w", name, err)
	}
	if w.cfg.Compress {
		w.compressWG.Add(1)
		go func() {
			defer w.compressWG.Done()
			if err := w.compressFile(name); err != nil {
				w.reportError(fmt.Errorf("store: compress %s (%s): %w", name, oldDay, err))
			}
			if err := w.gc(); err != nil {
				w.reportError(err)
			}
		}()
	}
	return nil
}

// loop flushes on a timer and, when a day passes with no samples at all,
// still seals the previous day so compression is not deferred indefinitely.
func (w *Writer) loop() {
	defer close(w.done)
	t := time.NewTicker(w.cfg.FlushInterval)
	defer t.Stop()

	// A slower housekeeping tick handles the idle-rotation and retention case.
	house := time.NewTicker(time.Hour)
	defer house.Stop()

	for {
		select {
		case <-w.stop:
			return
		case <-t.C:
			if err := w.Flush(); err != nil {
				w.reportError(err)
			}
		case <-house.C:
			w.mu.Lock()
			today := w.dayKey(time.Now())
			if w.day != "" && today > w.day {
				if err := w.flushLocked(); err != nil {
					w.reportError(err)
				}
				if err := w.rotateLocked(today); err != nil {
					w.reportError(err)
				}
			}
			w.mu.Unlock()
			if err := w.gc(); err != nil {
				w.reportError(err)
			}
		}
	}
}

// Close flushes, seals today's file if asked, and waits for compression.
func (w *Writer) Close() error {
	var err error
	w.stopOnce.Do(func() {
		close(w.stop)
		<-w.done

		w.mu.Lock()
		err = w.flushLocked()
		if w.file != nil {
			if cerr := w.file.Close(); cerr != nil && err == nil {
				err = cerr
			}
			w.file = nil
		}
		w.closed = true
		w.mu.Unlock()

		w.compressWG.Wait()
	})
	return err
}

// sealStaleDays compresses any uncompressed day file older than today, which
// is what a previous run leaves behind if it was killed.
func (w *Writer) sealStaleDays() {
	if !w.cfg.Compress {
		return
	}
	today := w.dayKey(time.Now())
	files, err := w.list()
	if err != nil {
		w.reportError(err)
		return
	}
	for _, f := range files {
		if f.Compressed || f.Day >= today {
			continue
		}
		if err := w.compressFile(f.Path); err != nil {
			w.reportError(fmt.Errorf("store: seal %s: %w", f.Path, err))
		}
	}
}

// compressFile writes path as path+".zst" via a temp file, then removes the
// original unless KeepUncompressed is set. Writing to a temp file and renaming
// means an interrupted compression never leaves a half-written .zst that a
// later read would treat as real data.
func (w *Writer) compressFile(path string) error {
	if !strings.HasSuffix(path, extPlain) {
		return fmt.Errorf("refusing to compress %q", path)
	}
	src, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // already handled by another pass
		}
		return err
	}
	defer src.Close()

	// An empty day file is noise; drop it rather than storing a 13-byte frame.
	if st, serr := src.Stat(); serr == nil && st.Size() == 0 {
		src.Close()
		if !w.cfg.KeepUncompressed {
			return os.Remove(path)
		}
		return nil
	}

	final := strings.TrimSuffix(path, extPlain) + extZstd
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*.zst")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName) // no-op once renamed
	}()

	level := zstd.EncoderLevelFromZstd(w.cfg.CompressLevel)
	zw, err := zstd.NewWriter(tmp, zstd.WithEncoderLevel(level))
	if err != nil {
		return err
	}
	if _, err := io.Copy(zw, bufio.NewReaderSize(src, 256<<10)); err != nil {
		zw.Close()
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, final); err != nil {
		return err
	}
	if !w.cfg.KeepUncompressed {
		return os.Remove(path)
	}
	return nil
}

// gc enforces RetentionDays.
func (w *Writer) gc() error {
	if w.cfg.RetentionDays <= 0 {
		return nil
	}
	cutoff := time.Now().AddDate(0, 0, -w.cfg.RetentionDays)
	cutoffDay := w.dayKey(cutoff)

	files, err := w.list()
	if err != nil {
		return err
	}
	var errs []string
	for _, f := range files {
		if f.Day >= cutoffDay {
			continue
		}
		if err := os.Remove(f.Path); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("store: retention: %s", strings.Join(errs, "; "))
	}
	return nil
}

func (w *Writer) list() ([]DayFile, error) {
	return List(w.cfg.Dir, w.cfg.Prefix)
}

// ---------------------------------------------------------------------------
// Listing and reading
// ---------------------------------------------------------------------------

// DayFile describes one file in the data directory.
type DayFile struct {
	Path       string
	Day        string // YYYY-MM-DD
	Compressed bool
	Size       int64
}

var dayRe = regexp.MustCompile(`^(.+)-(\d{4}-\d{2}-\d{2})\.lp(\.zst)?$`)

// List returns the day files in dir with the given prefix, oldest first.
func List(dir, prefix string) ([]DayFile, error) {
	if prefix == "" {
		prefix = "uptime"
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []DayFile
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		m := dayRe.FindStringSubmatch(e.Name())
		if m == nil || m[1] != prefix {
			continue
		}
		var size int64
		if info, ierr := e.Info(); ierr == nil {
			size = info.Size()
		}
		out = append(out, DayFile{
			Path:       filepath.Join(dir, e.Name()),
			Day:        m[2],
			Compressed: m[3] != "",
			Size:       size,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Day != out[j].Day {
			return out[i].Day < out[j].Day
		}
		// Prefer the uncompressed copy when both exist (KeepUncompressed);
		// reading it avoids a decode and they hold identical data.
		return !out[i].Compressed && out[j].Compressed
	})
	return out, nil
}

// Reader scans stored samples.
type Reader struct {
	Dir       string
	Prefix    string
	Precision lineproto.Precision
}

// Scan calls fn for every sample in [from, to). Samples are delivered in file
// order, which is chronological except for probes that straddle midnight.
//
// The day range is widened by one day on each side before filtering on the
// real timestamp, because a probe that starts at 23:59:59 and is buffered past
// midnight can legitimately land in the neighbouring file.
func (r *Reader) Scan(from, to time.Time, fn func(model.Result) error) error {
	prec := r.Precision
	if prec == "" {
		prec = lineproto.Nanoseconds
	}
	files, err := List(r.Dir, r.Prefix)
	if err != nil {
		return err
	}
	lo := from.AddDate(0, 0, -1).Format(dayLayout)
	hi := to.AddDate(0, 0, 1).Format(dayLayout)

	seenDay := map[string]bool{}
	for _, f := range files {
		if f.Day < lo || f.Day > hi {
			continue
		}
		// With KeepUncompressed both copies exist; read each day once.
		if seenDay[f.Day] {
			continue
		}
		seenDay[f.Day] = true

		if err := scanFile(f, prec, from, to, fn); err != nil {
			return err
		}
	}
	return nil
}

func scanFile(f DayFile, prec lineproto.Precision, from, to time.Time, fn func(model.Result) error) error {
	fh, err := os.Open(f.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // rotated out from under us
		}
		return err
	}
	defer fh.Close()

	var src io.Reader = bufio.NewReaderSize(fh, 256<<10)
	if f.Compressed {
		zr, zerr := zstd.NewReader(src)
		if zerr != nil {
			return fmt.Errorf("store: open %s: %w", f.Path, zerr)
		}
		defer zr.Close()
		src = zr.IOReadCloser()
	}

	sc := bufio.NewScanner(src)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		res, perr := lineproto.ParseLine(sc.Text(), prec)
		if perr != nil {
			// A truncated final line is expected if the process was killed
			// mid-write; skipping it is better than refusing the whole day.
			continue
		}
		if res.At.Before(from) || !res.At.Before(to) {
			continue
		}
		if err := fn(res); err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("store: read %s: %w", f.Path, err)
	}
	return nil
}
