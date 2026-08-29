package stats

import (
	"math"
	"testing"
	"time"

	"github.com/didvc/uptime-mon/internal/model"
)

func approx(t *testing.T, name string, got, want, tol float64) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Errorf("%s = %g, want %g (+/-%g)", name, got, want, tol)
	}
}

func TestPercentileMatchesNumpyDefault(t *testing.T) {
	// numpy.percentile([1..10], q) with the default linear interpolation.
	v := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	for _, c := range []struct{ p, want float64 }{
		{0, 1}, {25, 3.25}, {50, 5.5}, {75, 7.75}, {90, 9.1}, {95, 9.55}, {100, 10},
	} {
		approx(t, "p", percentile(v, c.p), c.want, 1e-9)
	}
	if got := percentile(nil, 50); got != 0 {
		t.Errorf("empty = %g", got)
	}
	if got := percentile([]float64{7}, 99); got != 7 {
		t.Errorf("single = %g", got)
	}
}

func TestDescribe(t *testing.T) {
	d := describe([]float32{2, 4, 4, 4, 5, 5, 7, 9})
	approx(t, "mean", d.Mean, 5, 1e-9)
	// Sample stddev (n-1) of that classic set is sqrt(32/7).
	approx(t, "stddev", d.StdDev, math.Sqrt(32.0/7.0), 1e-9)
	approx(t, "median", d.Median, 4.5, 1e-9)
	// Deviations from the 4.5 median are {2.5, .5, .5, .5, .5, .5, 2.5, 4.5};
	// their median is 0.5.
	approx(t, "mad", d.MAD, 0.5, 1e-9)
	approx(t, "min", d.Min, 2, 1e-9)
	approx(t, "max", d.Max, 9, 1e-9)
	approx(t, "cv", d.CV, d.StdDev/5, 1e-9)
	if d.N != 8 {
		t.Errorf("N = %d", d.N)
	}
	if d.Skew <= 0 {
		t.Errorf("expected positive skew, got %g", d.Skew)
	}
}

// buildWindow feeds a scripted up/down pattern one sample per minute.
func buildWindow(t *testing.T, pattern string, rttMS float64) (Summary, time.Time) {
	t.Helper()
	from := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(time.Duration(len(pattern)) * time.Minute)
	a := NewAccumulator(from, to, 10, 500*time.Millisecond)
	for i, c := range pattern {
		r := model.Result{
			Target: "svc", Kind: model.KindHTTP, Host: "h",
			At: from.Add(time.Duration(i) * time.Minute), Attempt: 1,
		}
		if c == 'u' {
			r.Status = model.StatusUp
			r.Code = 200
			r.RTT = time.Duration(rttMS * float64(time.Millisecond))
		} else {
			r.Status = model.StatusDown
			r.Code = 500
			r.RTT = 10 * time.Second
			r.Err = "status 500 (want 200-299)"
		}
		a.Add(r)
	}
	s := a.Summaries()
	if len(s) != 1 {
		t.Fatalf("expected 1 summary, got %d", len(s))
	}
	return s[0], from
}

func TestAvailabilityAndOutages(t *testing.T) {
	// t=0..4 up, t=5..6 down, t=7..9 up.
	s2, _ := buildWindow(t, "uuuuudduuu", 50)
	if s2.Samples != 10 || s2.Up != 8 || s2.Down != 2 {
		t.Fatalf("counts: %+v", s2)
	}
	approx(t, "availability", s2.Availability, 0.8, 1e-9)
	if s2.OutageCount != 1 {
		t.Fatalf("expected 1 outage, got %d: %+v", s2.OutageCount, s2.Outages)
	}
	o := s2.Outages[0]
	if o.Samples != 2 || o.Ongoing {
		t.Errorf("outage = %+v", o)
	}
	// Down at t=5,6; recovery sample at t=7 bounds it, so 2 minutes.
	if o.Duration() != 2*time.Minute {
		t.Errorf("outage duration = %v, want 2m", o.Duration())
	}
	if s2.MTTR != 2*time.Minute {
		t.Errorf("MTTR = %v", s2.MTTR)
	}
	if s2.Interval != time.Minute {
		t.Errorf("interval = %v", s2.Interval)
	}
	if s2.LastStatus != model.StatusUp {
		t.Errorf("last status = %v", s2.LastStatus)
	}
	// Current up-run started at t=7 and last sample is t=9.
	if s2.StreakLength != 2*time.Minute || s2.StreakCount != 3 {
		t.Errorf("streak = %v / %d", s2.StreakLength, s2.StreakCount)
	}
}

func TestOngoingOutage(t *testing.T) {
	s, _ := buildWindow(t, "uuuuuuudd", 50)
	if s.OutageCount != 1 {
		t.Fatalf("expected 1 outage, got %d", s.OutageCount)
	}
	if !s.Outages[0].Ongoing {
		t.Error("expected ongoing outage")
	}
	if s.Outages[0].Duration() <= 0 {
		t.Error("ongoing outage should have non-zero duration")
	}
	if s.LastStatus != model.StatusDown || s.LastErr == "" {
		t.Errorf("last = %v %q", s.LastStatus, s.LastErr)
	}
}

func TestRTTSplitsUpAndDown(t *testing.T) {
	s, _ := buildWindow(t, "uuuuuuuudd", 50)
	// Successful samples are all 50 ms; the two failures took 10 s each and
	// must not contaminate the median.
	approx(t, "rtt median", s.RTT.Median, 50, 1e-6)
	if s.RTT.N != 8 {
		t.Errorf("RTT.N = %d", s.RTT.N)
	}
	approx(t, "fail rtt median", s.FailRTT.Median, 10000, 1e-6)
	if s.FailRTT.N != 2 {
		t.Errorf("FailRTT.N = %d", s.FailRTT.N)
	}
}

func TestApdex(t *testing.T) {
	// All up, all 100 ms, T=500ms -> every sample satisfied -> 1.0
	s, _ := buildWindow(t, "uuuuuuuuuu", 100)
	approx(t, "apdex satisfied", s.Apdex, 1.0, 1e-9)

	// 1000 ms is > T but <= 4T -> tolerating -> 0.5
	s, _ = buildWindow(t, "uuuuuuuuuu", 1000)
	approx(t, "apdex tolerating", s.Apdex, 0.5, 1e-9)

	// 3000 ms is > 4T -> frustrated -> 0
	s, _ = buildWindow(t, "uuuuuuuuuu", 3000)
	approx(t, "apdex frustrated", s.Apdex, 0.0, 1e-9)

	// Half down -> the up half is satisfied, the down half scores zero.
	s, _ = buildWindow(t, "uuuuudddd", 100)
	approx(t, "apdex mixed", s.Apdex, 5.0/9.0, 1e-9)
}

func TestBucketsAndGaps(t *testing.T) {
	s, _ := buildWindow(t, "uuuuudduuu", 50)
	if len(s.Buckets) != 10 {
		t.Fatalf("buckets = %d", len(s.Buckets))
	}
	filled := 0
	for _, b := range s.Buckets {
		if b.Samples > 0 {
			filled++
			if b.Up > 0 && b.MeanRTT <= 0 {
				t.Errorf("bucket with up samples has no mean RTT: %+v", b)
			}
		}
	}
	if filled != 10 {
		t.Errorf("expected every bucket filled, got %d", filled)
	}
	// An empty bucket reports NaN availability, not 0, so charts can draw a
	// hole rather than a fake outage.
	if av := (Bucket{}).Availability(); !math.IsNaN(av) {
		t.Errorf("empty bucket availability = %g, want NaN", av)
	}

	// Now a window with a real gap: the monitor was off for 20 minutes.
	from := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	a := NewAccumulator(from, from.Add(time.Hour), 0, 0)
	at := from
	for i := range 10 {
		a.Add(model.Result{Target: "x", At: at, Status: model.StatusUp, RTT: time.Millisecond})
		at = at.Add(time.Minute)
		if i == 4 {
			at = at.Add(20 * time.Minute)
		}
	}
	got := a.Summaries()[0]
	if got.Gaps != 1 {
		t.Errorf("gaps = %d, want 1", got.Gaps)
	}
	if got.GapDuration < 19*time.Minute {
		t.Errorf("gap duration = %v", got.GapDuration)
	}
}

func TestNines(t *testing.T) {
	approx(t, "99%", nines(0.99), 2, 1e-9)
	approx(t, "99.9%", nines(0.999), 3, 1e-9)
	if !math.IsInf(nines(1.0), 1) {
		t.Error("perfect availability should be +Inf nines")
	}
	if nines(0) != 0 {
		t.Error("zero availability should be 0 nines")
	}
}

func TestOverallAggregates(t *testing.T) {
	from := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	a := NewAccumulator(from, from.Add(time.Hour), 5, 500*time.Millisecond)
	for i := range 10 {
		at := from.Add(time.Duration(i) * time.Minute)
		a.Add(model.Result{Target: "a", At: at, Status: model.StatusUp, RTT: 10 * time.Millisecond})
		st := model.StatusUp
		if i%2 == 0 {
			st = model.StatusDown
		}
		a.Add(model.Result{Target: "b", At: at, Status: st, RTT: 20 * time.Millisecond, Err: "boom"})
	}
	if a.Len() != 2 {
		t.Fatalf("len = %d", a.Len())
	}
	o := a.Overall()
	if o.Samples != 20 || o.Up != 15 {
		t.Fatalf("overall = %+v", o)
	}
	approx(t, "overall availability", o.Availability, 0.75, 1e-9)
	if o.OutageCount != 0 {
		t.Error("overall should not claim outage structure")
	}
	if len(o.Errors) != 1 || o.Errors[0].Count != 5 {
		t.Errorf("errors = %+v", o.Errors)
	}
}

func TestSortSummaries(t *testing.T) {
	in := []Summary{
		{Target: "b", Availability: 0.5, RTT: Dist{Median: 10}},
		{Target: "a", Availability: 0.9, RTT: Dist{Median: 90}},
	}
	SortSummaries(in, "availability")
	if in[0].Target != "b" {
		t.Errorf("worst-first expected, got %s", in[0].Target)
	}
	SortSummaries(in, "rtt")
	if in[0].Target != "a" {
		t.Errorf("slowest-first expected, got %s", in[0].Target)
	}
	SortSummaries(in, "whatever")
	if in[0].Target != "a" {
		t.Errorf("name order expected, got %s", in[0].Target)
	}
}
