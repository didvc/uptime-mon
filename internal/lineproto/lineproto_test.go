package lineproto

import (
	"strings"
	"testing"
	"time"

	"github.com/didvc/uptime-mon/internal/model"
)

func TestRoundTrip(t *testing.T) {
	at := time.Unix(1724900000, 123456789)
	cases := []model.Result{
		{
			Target: "https://example.com/", Group: "prod", Kind: model.KindHTTP,
			Host: "example.com", At: at, Status: model.StatusUp, Code: 200,
			RTT: 42*time.Millisecond + 100*time.Microsecond,
			DNS: 1500 * time.Microsecond, Connect: 3 * time.Millisecond,
			TLS: 12 * time.Millisecond, TTFB: 40 * time.Millisecond,
			Bytes: 4096, Attempt: 1,
		},
		{
			// Spaces and commas in the error string are the interesting case:
			// a naive splitter loses the timestamp here.
			Target: "svc with space,comma", Kind: model.KindHTTP, Host: "h",
			At: at, Status: model.StatusDown, Code: 0, RTT: 10 * time.Second,
			Attempt: 3, Err: `dial tcp 1.2.3.4:443: connect: connection refused, "quoted"`,
		},
		{
			Target: "ping 8.8.8.8", Kind: model.KindPing, Host: "8.8.8.8",
			At: at, Status: model.StatusUp, RTT: 8 * time.Millisecond, Attempt: 1,
		},
	}

	enc := NewEncoder("uptime", Nanoseconds, nil)
	for _, want := range cases {
		line := string(enc.AppendResult(nil, want))
		if !strings.HasSuffix(line, "\n") {
			t.Fatalf("no trailing newline: %q", line)
		}
		if n := strings.Count(line, "\n"); n != 1 {
			t.Fatalf("embedded newline in %q", line)
		}

		got, err := ParseLine(line, Nanoseconds)
		if err != nil {
			t.Fatalf("ParseLine(%q): %v", line, err)
		}
		if got.Target != want.Target || got.Group != want.Group ||
			got.Host != want.Host || got.Kind != want.Kind {
			t.Errorf("tags: got %+v want %+v\nline: %s", got, want, line)
		}
		if got.Status != want.Status || got.Code != want.Code ||
			got.Bytes != want.Bytes || got.Attempt != want.Attempt {
			t.Errorf("fields: got %+v want %+v\nline: %s", got, want, line)
		}
		if got.Err != want.Err {
			t.Errorf("err: got %q want %q\nline: %s", got.Err, want.Err, line)
		}
		if !got.At.Equal(want.At) {
			t.Errorf("time: got %v want %v", got.At, want.At)
		}
		// Durations survive to microsecond precision (3 decimal places of ms).
		for _, d := range []struct {
			name     string
			got, exp time.Duration
		}{
			{"rtt", got.RTT, want.RTT}, {"dns", got.DNS, want.DNS},
			{"connect", got.Connect, want.Connect}, {"tls", got.TLS, want.TLS},
			{"ttfb", got.TTFB, want.TTFB},
		} {
			if diff := d.got - d.exp; diff > time.Microsecond || diff < -time.Microsecond {
				t.Errorf("%s: got %v want %v", d.name, d.got, d.exp)
			}
		}
	}
}

func TestPrecisionSeconds(t *testing.T) {
	at := time.Unix(1724900000, 999000000)
	enc := NewEncoder("", Seconds, nil)
	r := model.Result{Target: "x", At: at, Status: model.StatusUp, RTT: time.Millisecond}
	line := string(enc.AppendResult(nil, r))
	if !strings.HasSuffix(strings.TrimSpace(line), "1724900000") {
		t.Fatalf("expected second-precision timestamp, got %q", line)
	}
	got, err := ParseLine(line, Seconds)
	if err != nil {
		t.Fatal(err)
	}
	if got.At.Unix() != 1724900000 {
		t.Fatalf("got %v", got.At)
	}
}

func TestExtraTagsAndMeasurement(t *testing.T) {
	enc := NewEncoder("probe", Nanoseconds, []model.Header{{Key: "agent", Value: "lap top"}})
	line := string(enc.AppendResult(nil, model.Result{
		Target: "x", At: time.Unix(1, 0), Status: model.StatusUp,
	}))
	if !strings.HasPrefix(line, "probe,endpoint=x,") {
		t.Fatalf("bad prefix: %q", line)
	}
	if !strings.Contains(line, `agent=lap\ top`) {
		t.Fatalf("extra tag not escaped: %q", line)
	}
}

func TestSkipAndErrors(t *testing.T) {
	for _, s := range []string{"", "   ", "# comment"} {
		if _, err := ParseLine(s, Nanoseconds); err != ErrSkipLine {
			t.Errorf("ParseLine(%q) = %v, want ErrSkipLine", s, err)
		}
	}
	for _, s := range []string{"nofields", "m,endpoint=x up=1i", "m,nokey=1 up=1i 5"} {
		if _, err := ParseLine(s, Nanoseconds); err == nil || err == ErrSkipLine {
			t.Errorf("ParseLine(%q) should fail, got %v", s, err)
		}
	}
}
