package series

import (
	"testing"
	"time"
	"unsafe"

	"github.com/didvc/uptime-mon/internal/model"
)

func res(name string, at time.Time, up bool, rtt time.Duration, errText string) model.Result {
	st := model.StatusDown
	code := 500
	if up {
		st, code, errText = model.StatusUp, 200, ""
	}
	return model.Result{
		Target: name, Group: "g", Host: "h", Kind: model.KindHTTP,
		At: at, Status: st, Code: code, RTT: rtt, Bytes: 100, Err: errText,
	}
}

func TestRingBufferWrapsAndKeepsNewest(t *testing.T) {
	s := NewSet(10)
	base := time.Unix(1700000000, 0)
	for i := range 25 {
		s.Add(res("a", base.Add(time.Duration(i)*time.Second), true, time.Millisecond, ""))
	}

	var got []model.Result
	s.mu.RLock()
	s.byName["a"].Each(base, base.Add(time.Hour), func(r model.Result) { got = append(got, r) })
	s.mu.RUnlock()

	if len(got) != 10 {
		t.Fatalf("retained %d samples, want 10", len(got))
	}
	// The oldest retained sample should be #15, the newest #24.
	if !got[0].At.Equal(base.Add(15 * time.Second)) {
		t.Errorf("oldest = %v", got[0].At)
	}
	if !got[9].At.Equal(base.Add(24 * time.Second)) {
		t.Errorf("newest = %v", got[9].At)
	}
	// Samples must stay in chronological order across the wrap point.
	for i := 1; i < len(got); i++ {
		if !got[i].At.After(got[i-1].At) {
			t.Fatalf("out of order at %d: %v then %v", i, got[i-1].At, got[i].At)
		}
	}
}

func TestRoundTripFields(t *testing.T) {
	s := NewSet(100)
	at := time.Unix(1700000000, 0)
	s.Add(res("a", at, false, 1500*time.Millisecond, "connection refused"))

	last := s.LastByName()["a"]
	if last.Status != model.StatusDown || last.Code != 500 {
		t.Errorf("status/code = %v/%d", last.Status, last.Code)
	}
	if last.Err != "connection refused" {
		t.Errorf("err = %q", last.Err)
	}
	if d := last.RTT - 1500*time.Millisecond; d > time.Microsecond || d < -time.Microsecond {
		t.Errorf("rtt = %v", last.RTT)
	}
	if last.Bytes != 100 || last.Group != "g" || last.Host != "h" {
		t.Errorf("fields = %+v", last)
	}
}

func TestErrorInterning(t *testing.T) {
	s := NewSet(1000)
	base := time.Unix(1700000000, 0)
	for i := range 500 {
		// Two repeating error texts across many samples.
		e := "timeout"
		if i%2 == 0 {
			e = "connection refused"
		}
		s.Add(res("a", base.Add(time.Duration(i)*time.Second), false, time.Second, e))
	}
	s.mu.RLock()
	n := len(s.byName["a"].errs)
	s.mu.RUnlock()
	if n != 3 { // "", timeout, connection refused
		t.Errorf("error table has %d entries, want 3", n)
	}
}

func TestSummariesAndOverall(t *testing.T) {
	s := NewSet(1000)
	base := time.Unix(1700000000, 0)
	for i := range 20 {
		at := base.Add(time.Duration(i) * time.Minute)
		s.Add(res("a", at, true, 10*time.Millisecond, ""))
		s.Add(res("b", at, i%4 != 0, 20*time.Millisecond, "boom"))
	}
	from, to := base, base.Add(time.Hour)

	sums := s.Summaries(from, to, 8, 500*time.Millisecond)
	if len(sums) != 2 {
		t.Fatalf("got %d summaries", len(sums))
	}
	byName := map[string]float64{}
	for _, x := range sums {
		byName[x.Target] = x.Availability
		if len(x.Buckets) != 8 {
			t.Errorf("%s: %d buckets", x.Target, len(x.Buckets))
		}
	}
	if byName["a"] != 1.0 {
		t.Errorf("a availability = %g", byName["a"])
	}
	if byName["b"] != 0.75 {
		t.Errorf("b availability = %g", byName["b"])
	}

	o := s.Overall(from, to, 500*time.Millisecond)
	if o.Samples != 40 || o.Up != 35 {
		t.Errorf("overall = %d/%d", o.Up, o.Samples)
	}

	if s.Len() != 2 || s.Samples() != 40 {
		t.Errorf("len=%d samples=%d", s.Len(), s.Samples())
	}
	if names := s.Names(); len(names) != 2 || names[0] != "a" {
		t.Errorf("names = %v", names)
	}

	s.Reset()
	if s.Len() != 0 || s.Samples() != 0 {
		t.Error("reset did not clear")
	}
}

func TestWindowFiltering(t *testing.T) {
	s := NewSet(1000)
	base := time.Unix(1700000000, 0)
	for i := range 60 {
		s.Add(res("a", base.Add(time.Duration(i)*time.Minute), true, time.Millisecond, ""))
	}
	sums := s.Summaries(base.Add(10*time.Minute), base.Add(20*time.Minute), 0, 0)
	if len(sums) != 1 || sums[0].Samples != 10 {
		t.Fatalf("window filter got %+v", sums)
	}
}

func TestSampleStaysCompact(t *testing.T) {
	// The whole reason this package exists is the per-sample footprint; if it
	// grows, the memory claims in the docs stop being true.
	if got := int(unsafeSizeof()); got > 24 {
		t.Errorf("Sample is %d bytes, want <= 24", got)
	}
}

func unsafeSizeof() uintptr { return unsafe.Sizeof(Sample{}) }
