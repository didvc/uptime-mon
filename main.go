// Command uptime-mon watches HTTP endpoints and writes the results to daily,
// zstd-compressed line-protocol files.
//
// It does one job. There is no web server, no database, no notification
// integrations and no alerting — by design, because those are the parts that
// rot, and the parts that turn a monitor into something that itself needs
// monitoring. What is left is a probe loop, an append-only text file and a
// terminal UI to read it with.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"runtime/debug"
	"strings"
	"time"
)

// version is stamped at build time for release binaries:
//
//	go build -ldflags "-X main.version=1.2.3"
//
// When it is not stamped, which is what happens with `go install
// module@version`, the module version recorded in the binary is used instead,
// so an installed copy still reports something truthful.
var version = ""

func init() {
	if version != "" {
		return
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		if v := bi.Main.Version; v != "" && v != "(devel)" {
			version = v
			return
		}
	}
	version = "dev"
}

type command struct {
	name    string
	summary string
	run     func(args []string) error
}

func main() {
	cmds := []command{
		{"run", "probe endpoints and record results", cmdRun},
		{"tui", "browse recorded data in the terminal UI", cmdTUI},
		{"stats", "print statistics for a time window", cmdStats},
		{"check", "probe every endpoint once and print the result", cmdCheck},
		{"import", "convert an Uptime Kuma backup into endpoints.txt", cmdImport},
		{"data", "list, compact and prune the data directory", cmdData},
		{"version", "print the version", cmdVersion},
	}

	if len(os.Args) < 2 {
		usage(cmds)
		os.Exit(2)
	}
	name := os.Args[1]
	if name == "-h" || name == "--help" || name == "help" {
		usage(cmds)
		return
	}
	for _, c := range cmds {
		if c.name == name {
			if err := c.run(os.Args[2:]); err != nil {
				if errors.Is(err, flag.ErrHelp) {
					os.Exit(2)
				}
				fmt.Fprintf(os.Stderr, "uptime-mon %s: %v\n", name, err)
				os.Exit(1)
			}
			return
		}
	}
	fmt.Fprintf(os.Stderr, "uptime-mon: unknown command %q\n\n", name)
	usage(cmds)
	os.Exit(2)
}

func usage(cmds []command) {
	w := os.Stderr
	fmt.Fprintf(w, `uptime-mon %s — lightweight endpoint uptime monitoring

usage: uptime-mon <command> [flags]

commands:
`, version)
	for _, c := range cmds {
		fmt.Fprintf(w, "  %-9s %s\n", c.name, c.summary)
	}
	fmt.Fprint(w, `
Run "uptime-mon <command> -h" for the flags of a command.

getting started:
  uptime-mon import -in backup.json -out endpoints.txt
  uptime-mon check -endpoints endpoints.txt
  uptime-mon run   -endpoints endpoints.txt -data ./data -tui
  uptime-mon stats -data ./data -window 24h
`)
}

func cmdVersion([]string) error {
	fmt.Printf("uptime-mon %s\n", version)
	return nil
}

// ---------------------------------------------------------------------------
// shared flag helpers
// ---------------------------------------------------------------------------

// kvFlag collects repeatable key=value flags, e.g. -tag host=laptop.
type kvFlag []struct{ K, V string }

func (f *kvFlag) String() string {
	parts := make([]string, 0, len(*f))
	for _, kv := range *f {
		parts = append(parts, kv.K+"="+kv.V)
	}
	return strings.Join(parts, ",")
}

func (f *kvFlag) Set(s string) error {
	k, v, ok := strings.Cut(s, "=")
	if !ok || k == "" {
		return fmt.Errorf("expected key=value, got %q", s)
	}
	*f = append(*f, struct{ K, V string }{k, v})
	return nil
}

// newFlagSet builds a FlagSet that prints a description above its flags.
func newFlagSet(name, desc string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: uptime-mon %s [flags]\n\n%s\n\nflags:\n", name, desc)
		fs.PrintDefaults()
	}
	return fs
}

// parseWhen accepts a duration ago ("24h", "7d"), an RFC3339 timestamp, a
// date ("2026-08-29"), or "now". Relative forms are what a person actually
// types; absolute ones are what a script needs.
func parseWhen(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	switch strings.ToLower(s) {
	case "", "now":
		return now, nil
	}
	// "7d" is not a Go duration, but it is the obvious thing to type.
	if strings.HasSuffix(s, "d") {
		if n, err := time.ParseDuration(strings.TrimSuffix(s, "d") + "h"); err == nil {
			return now.Add(-n * 24), nil
		}
	}
	if d, err := time.ParseDuration(s); err == nil {
		if d > 0 {
			d = -d // "24h" means 24 hours ago
		}
		return now.Add(d), nil
	}
	for _, layout := range []string{
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
		"2006-01-02",
	} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("cannot parse time %q (try 24h, 7d, 2026-08-29, or an RFC3339 timestamp)", s)
}

// colorEnabled decides whether to emit ANSI colour, honouring the NO_COLOR
// convention and whether stdout is actually a terminal.
func colorEnabled(disable bool, needTTY bool) bool {
	if disable {
		return false
	}
	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		return false
	}
	if os.Getenv("TERM") == "dumb" {
		return false
	}
	if needTTY {
		return isTerminal()
	}
	return true
}
