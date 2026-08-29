package main

import (
	"fmt"
	"os"
	"time"

	"github.com/didvc/uptime-mon/internal/lineproto"
	"github.com/didvc/uptime-mon/internal/model"
	"github.com/didvc/uptime-mon/internal/store"
)

func cmdData(args []string) error {
	fs := newFlagSet("data", "Inspect and maintain the data directory.\n\n"+
		"With no action flags it lists the day files. -compact seals any\n"+
		"uncompressed day older than today, and -prune applies a retention\n"+
		"cutoff. Both are also done automatically by `run`; this is for\n"+
		"tidying up after an interrupted one.")

	dir := fs.String("data", "./data", "data directory")
	prefix := fs.String("prefix", "uptime", "day-file prefix")
	precision := fs.String("precision", "ns", "timestamp precision the files were written with")
	compact := fs.Bool("compact", false, "zstd-compress completed days that are still plain text")
	compressLevel := fs.Int("compress-level", 3, "zstd level for -compact")
	prune := fs.Int("prune", 0, "delete day files older than this many days")
	count := fs.Bool("count", false, "count samples per day (reads every file)")
	yes := fs.Bool("y", false, "do not ask before deleting files")
	if err := fs.Parse(args); err != nil {
		return err
	}

	prec, err := lineproto.ParsePrecision(*precision)
	if err != nil {
		return err
	}

	if *compact {
		if err := compactDir(*dir, *prefix, *compressLevel); err != nil {
			return err
		}
	}
	if *prune > 0 {
		if err := pruneDir(*dir, *prefix, *prune, *yes); err != nil {
			return err
		}
	}

	files, err := store.List(*dir, *prefix)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		fmt.Printf("no day files in %s\n", *dir)
		return nil
	}

	var totalSize int64
	var totalSamples int
	fmt.Printf("%-12s %-6s %10s %12s\n", "DAY", "FORM", "SIZE", "SAMPLES")
	for _, f := range files {
		form := "plain"
		if f.Compressed {
			form = "zstd"
		}
		samples := ""
		if *count {
			n, err := countSamples(*dir, *prefix, f, prec)
			if err != nil {
				return err
			}
			totalSamples += n
			samples = thousands(n)
		}
		totalSize += f.Size
		fmt.Printf("%-12s %-6s %10s %12s\n", f.Day, form, humanBytes(f.Size), samples)
	}

	fmt.Printf("\n%d file(s), %s on disk", len(files), humanBytes(totalSize))
	if *count && totalSamples > 0 {
		fmt.Printf(", %s samples, %.1f bytes/sample",
			thousands(totalSamples), float64(totalSize)/float64(totalSamples))
	}
	fmt.Println()
	return nil
}

// countSamples counts the samples timestamped within one day. It goes through
// the reader's normal path, bounded to that day, so decompression and parsing
// live in exactly one place.
func countSamples(dir, prefix string, f store.DayFile, prec lineproto.Precision) (int, error) {
	day, err := time.ParseInLocation("2006-01-02", f.Day, time.Local)
	if err != nil {
		return 0, err
	}
	r := &store.Reader{Dir: dir, Prefix: prefix, Precision: prec}
	n := 0
	err = r.Scan(day, day.AddDate(0, 0, 1), func(model.Result) error {
		n++
		return nil
	})
	return n, err
}

func compactDir(dir, prefix string, level int) error {
	// Opening a writer runs the same seal-stale-days pass that `run` does on
	// startup, so there is exactly one implementation of "compress yesterday".
	w, err := store.Open(store.Config{
		Dir: dir, Prefix: prefix, Compress: true, CompressLevel: level,
		FlushInterval: time.Hour,
	})
	if err != nil {
		return err
	}
	w.OnError = func(e error) { fmt.Fprintf(os.Stderr, "uptime-mon: %v\n", e) }
	return w.Close()
}

func pruneDir(dir, prefix string, days int, assumeYes bool) error {
	files, err := store.List(dir, prefix)
	if err != nil {
		return err
	}
	cutoff := time.Now().AddDate(0, 0, -days).Format("2006-01-02")

	var doomed []store.DayFile
	var bytes int64
	for _, f := range files {
		if f.Day < cutoff {
			doomed = append(doomed, f)
			bytes += f.Size
		}
	}
	if len(doomed) == 0 {
		fmt.Printf("nothing older than %s\n", cutoff)
		return nil
	}

	fmt.Printf("will delete %d file(s) older than %s, freeing %s:\n",
		len(doomed), cutoff, humanBytes(bytes))
	for _, f := range doomed {
		fmt.Printf("  %s\n", f.Path)
	}
	if !assumeYes {
		// Deleting recorded history is not undoable, so it is confirmed by
		// default rather than by a flag the user has to remember to add.
		fmt.Print("proceed? [y/N] ")
		var answer string
		fmt.Scanln(&answer)
		if answer != "y" && answer != "Y" && answer != "yes" {
			return fmt.Errorf("aborted")
		}
	}
	for _, f := range doomed {
		if err := os.Remove(f.Path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	fmt.Printf("deleted %d file(s)\n", len(doomed))
	return nil
}
