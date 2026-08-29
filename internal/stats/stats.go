// Package stats turns a stream of Results into the numbers a human actually
// reads: availability, a latency distribution, outage structure, and a
// bucketed series for charting.
//
// It is a streaming accumulator rather than a query engine — you feed it the
// output of store.Reader.Scan and it holds one compact per-endpoint struct.
// The only unbounded allocation is the latency vector needed for exact
// percentiles, kept as float32 milliseconds (4 bytes/sample, so a 30-day
// window over 90 endpoints at 60s costs roughly 15 MB and a 24h window about
// 0.5 MB).
package stats

import (
	"math"
	"sort"
	"time"

	"github.com/didvc/uptime-mon/internal/model"
)

// Dist is a latency distribution in milliseconds.
//
// Percentiles use linear interpolation between order statistics (the "R-7"
// definition, which is what numpy.percentile and most spreadsheets use), so
// small sample counts degrade gracefully instead of snapping to a single
// observation.
type Dist struct {
	N      int
	Min    float64
	Max    float64
	Mean   float64
	Median float64
	P75    float64
	P90    float64
	P95    float64
	P99    float64
	StdDev float64 // sample standard deviation (n-1)
	// MAD is the median absolute deviation from the median. Unlike StdDev it
	// is not dragged around by a single 10-second timeout, so it is the honest
	// answer to "how jittery is this endpoint normally".
	MAD float64
	IQR float64 // P75 - P25
	P25 float64
	// CV is StdDev/Mean, a unitless measure of relative spread that lets a
	// 5 ms endpoint and a 500 ms endpoint be compared for stability.
	CV float64
	// Skew is the sample skewness (Fisher-Pearson). Positive means a long
	// right tail, which is the normal shape for latency.
	Skew float64
}

// Outage is a maximal run of consecutive failing samples.
type Outage struct {
	Start   time.Time // timestamp of the first failing sample
	End     time.Time // timestamp of the recovering sample, or window end
	Samples int
	Ongoing bool // still failing at the end of the window
	Reason  string
}

// Duration is the wall-clock span of the outage.
func (o Outage) Duration() time.Duration { return o.End.Sub(o.Start) }

// ErrCount is one failure class and how often it happened.
type ErrCount struct {
	Err   string
	Count int
}

// Bucket is one time slice of the window, used to draw charts.
type Bucket struct {
	Start   time.Time
	Samples int
	Up      int
	// MeanRTT and MaxRTT are over successful samples in the bucket.
	MeanRTT float64
	MaxRTT  float64
	MinRTT  float64
	P95RTT  float64
}

// Availability of the bucket, or NaN when it holds no samples (a gap, which
// charts should render as a hole rather than as an outage).
func (b Bucket) Availability() float64 {
	if b.Samples == 0 {
		return math.NaN()
	}
	return float64(b.Up) / float64(b.Samples)
}

// Summary is everything known about one endpoint over the window.
type Summary struct {
	Target string
	Group  string
	Host   string
	Kind   model.Kind

	From time.Time
	To   time.Time

	Samples int
	Up      int
	Down    int

	// Availability is the fraction of samples that succeeded. It is a
	// sample-based estimate, not a time-weighted one: with even sampling the
	// two agree, and probe data has nothing better to offer.
	Availability float64

	// Nines is Availability expressed as the count of leading nines (99.9% ->
	// 3.0), which is how availability targets are usually stated.
	Nines float64

	// RTT covers successful samples only. Mixing in the 10-second timeouts of
	// failed samples would make the median meaningless.
	RTT Dist
	// FailRTT covers failing samples: how long it takes to *fail*. A fast
	// connection-refused and a slow timeout are different operational stories.
	FailRTT Dist

	// Current state at the end of the window.
	LastSeen     time.Time
	LastStatus   model.Status
	LastCode     int
	LastErr      string
	LastRTT      time.Duration
	StreakStart  time.Time     // when the current up/down run began
	StreakLength time.Duration //
	StreakCount  int           // samples in the current run

	Outages       []Outage
	OutageCount   int
	Downtime      time.Duration // summed outage duration
	LongestOutage time.Duration
	MTTR          time.Duration // mean time to recovery over completed outages
	MTBF          time.Duration // mean uptime between outage starts

	// Interval is the median gap between consecutive samples: the effective
	// resolution of everything above.
	Interval time.Duration
	// Gaps counts places where the gap was more than 3x the median, i.e. the
	// monitor itself was not running. Availability cannot see these, so they
	// are reported separately rather than silently counted as "up".
	Gaps        int
	GapDuration time.Duration

	// Apdex scores latency against a target T: satisfied (<=T) counts 1,
	// tolerating (<=4T) counts 0.5, everything slower or failed counts 0.
	Apdex  float64
	ApdexT time.Duration

	Errors []ErrCount // most frequent first
	Codes  map[int]int

	BytesTotal int64
	Buckets    []Bucket
}

// ---------------------------------------------------------------------------

// Accumulator consumes Results and produces Summaries.
type Accumulator struct {
	from, to time.Time
	nBuckets int
	apdexT   time.Duration

	order []string // stable output order: first-seen
	byKey map[string]*acc
}

// NewAccumulator prepares an accumulator for the window [from, to).
// buckets is the number of chart slices; pass 0 to skip bucketing.
func NewAccumulator(from, to time.Time, buckets int, apdexT time.Duration) *Accumulator {
	if apdexT <= 0 {
		apdexT = 500 * time.Millisecond
	}
	if buckets < 0 {
		buckets = 0
	}
	return &Accumulator{
		from: from, to: to, nBuckets: buckets, apdexT: apdexT,
		byKey: make(map[string]*acc),
	}
}

type acc struct {
	target, group, host string
	kind                model.Kind

	samples, up int
	okRTT       []float32
	failRTT     []float32
	bytesTotal  int64

	errs  map[string]int
	codes map[int]int

	// sample-to-sample bookkeeping
	havePrev  bool
	prevAt    time.Time
	prevUp    bool
	deltas    []float64 // seconds between consecutive samples
	streakAt  time.Time // start of the current run
	streakN   int
	lastRes   model.Result
	firstAt   time.Time
	outages   []Outage
	inOutage  bool
	curOutage Outage

	apdexScore float64

	buckets []Bucket
	bkSum   []float64   // per-bucket sum of successful RTTs
	bkVals  [][]float32 // per-bucket successful RTTs, for the p95 line
}

// Add folds one result into the accumulator. Results are expected roughly in
// chronological order (which is what the store produces).
func (a *Accumulator) Add(r model.Result) {
	e := a.byKey[r.Target]
	if e == nil {
		e = &acc{
			target: r.Target, group: r.Group, host: r.Host, kind: r.Kind,
			errs: map[string]int{}, codes: map[int]int{},
			firstAt: r.At, streakAt: r.At,
		}
		if a.nBuckets > 0 {
			e.buckets = make([]Bucket, a.nBuckets)
			e.bkSum = make([]float64, a.nBuckets)
			e.bkVals = make([][]float32, a.nBuckets)
			span := a.to.Sub(a.from)
			for i := range e.buckets {
				e.buckets[i].Start = a.from.Add(time.Duration(int64(span) * int64(i) / int64(a.nBuckets)))
				e.buckets[i].MinRTT = math.Inf(1)
			}
		}
		a.byKey[r.Target] = e
		a.order = append(a.order, r.Target)
	}
	// Tags can be backfilled: an endpoint may have gained a group partway
	// through the window, and the newer value is the more useful one.
	if r.Group != "" {
		e.group = r.Group
	}
	if r.Host != "" {
		e.host = r.Host
	}
	if r.Kind != "" {
		e.kind = r.Kind
	}

	up := r.Status == model.StatusUp
	rttMS := float32(float64(r.RTT) / float64(time.Millisecond))

	e.samples++
	e.bytesTotal += r.Bytes
	if r.Code != 0 {
		e.codes[r.Code]++
	}
	if up {
		e.up++
		e.okRTT = append(e.okRTT, rttMS)
	} else {
		e.failRTT = append(e.failRTT, rttMS)
		reason := r.Err
		if reason == "" {
			reason = "unknown"
		}
		e.errs[reason]++
	}

	// Apdex: a failure is frustrating by definition, whatever its latency.
	switch {
	case !up:
		// contributes 0
	case r.RTT <= a.apdexT:
		e.apdexScore++
	case r.RTT <= 4*a.apdexT:
		e.apdexScore += 0.5
	}

	// Runs and outages.
	if e.havePrev {
		e.deltas = append(e.deltas, r.At.Sub(e.prevAt).Seconds())
		if up != e.prevUp {
			e.streakAt, e.streakN = r.At, 0
		}
	}
	e.streakN++

	if !up {
		if !e.inOutage {
			e.inOutage = true
			e.curOutage = Outage{Start: r.At, Reason: r.Err}
		}
		e.curOutage.Samples++
		e.curOutage.End = r.At
	} else if e.inOutage {
		// The recovering sample bounds the outage: the service was down
		// somewhere between the last failure and this success, and taking the
		// recovery timestamp is the conservative (longer) reading.
		e.curOutage.End = r.At
		e.outages = append(e.outages, e.curOutage)
		e.inOutage = false
		e.curOutage = Outage{}
	}

	// Buckets.
	if a.nBuckets > 0 {
		if i := a.bucketIndex(r.At); i >= 0 {
			b := &e.buckets[i]
			b.Samples++
			if up {
				b.Up++
				v := float64(rttMS)
				e.bkSum[i] += v
				e.bkVals[i] = append(e.bkVals[i], rttMS)
				if v > b.MaxRTT {
					b.MaxRTT = v
				}
				if v < b.MinRTT {
					b.MinRTT = v
				}
			}
		}
	}

	e.havePrev, e.prevAt, e.prevUp = true, r.At, up
	e.lastRes = r
}

func (a *Accumulator) bucketIndex(t time.Time) int {
	span := a.to.Sub(a.from)
	if span <= 0 {
		return -1
	}
	off := t.Sub(a.from)
	if off < 0 || off >= span {
		return -1
	}
	i := int(int64(a.nBuckets) * int64(off) / int64(span))
	if i >= a.nBuckets {
		i = a.nBuckets - 1
	}
	return i
}

// Summaries returns one Summary per endpoint, in first-seen order.
func (a *Accumulator) Summaries() []Summary {
	out := make([]Summary, 0, len(a.order))
	for _, k := range a.order {
		out = append(out, a.finish(a.byKey[k]))
	}
	return out
}

// Len reports how many endpoints have been seen.
func (a *Accumulator) Len() int { return len(a.byKey) }

func (a *Accumulator) finish(e *acc) Summary {
	s := Summary{
		Target: e.target, Group: e.group, Host: e.host, Kind: e.kind,
		From: a.from, To: a.to,
		Samples: e.samples, Up: e.up, Down: e.samples - e.up,
		ApdexT:     a.apdexT,
		Codes:      e.codes,
		BytesTotal: e.bytesTotal,
	}
	if e.samples == 0 {
		return s
	}

	s.Availability = float64(e.up) / float64(e.samples)
	s.Nines = nines(s.Availability)
	s.Apdex = e.apdexScore / float64(e.samples)

	s.RTT = describe(e.okRTT)
	s.FailRTT = describe(e.failRTT)

	s.LastSeen = e.lastRes.At
	s.LastStatus = e.lastRes.Status
	s.LastCode = e.lastRes.Code
	s.LastErr = e.lastRes.Err
	s.LastRTT = e.lastRes.RTT
	s.StreakStart = e.streakAt
	s.StreakCount = e.streakN
	s.StreakLength = e.lastRes.At.Sub(e.streakAt)

	// Median sampling interval, and gaps where the monitor was not running.
	if len(e.deltas) > 0 {
		d := append([]float64(nil), e.deltas...)
		sort.Float64s(d)
		med := percentile(d, 50)
		s.Interval = time.Duration(med * float64(time.Second))
		for _, x := range e.deltas {
			if med > 0 && x > 3*med {
				s.Gaps++
				s.GapDuration += time.Duration((x - med) * float64(time.Second))
			}
		}
	}

	// Close an outage that is still open at the end of the window.
	outages := e.outages
	if e.inOutage {
		o := e.curOutage
		o.Ongoing = true
		// Extend to the window end (or the last sample, whichever is earlier)
		// so an ongoing outage is not reported as zero-length.
		end := a.to
		if e.lastRes.At.After(end) {
			end = e.lastRes.At
		}
		o.End = end
		outages = append(outages, o)
	}
	s.Outages = outages
	s.OutageCount = len(outages)
	for _, o := range outages {
		d := o.Duration()
		s.Downtime += d
		if d > s.LongestOutage {
			s.LongestOutage = d
		}
	}

	var completed []time.Duration
	for _, o := range outages {
		if !o.Ongoing {
			completed = append(completed, o.Duration())
		}
	}
	if len(completed) > 0 {
		var total time.Duration
		for _, d := range completed {
			total += d
		}
		s.MTTR = total / time.Duration(len(completed))
	}
	if s.OutageCount > 0 {
		observed := e.lastRes.At.Sub(e.firstAt)
		if uptime := observed - s.Downtime; uptime > 0 {
			s.MTBF = uptime / time.Duration(s.OutageCount)
		}
	}

	// Error breakdown, most frequent first.
	s.Errors = make([]ErrCount, 0, len(e.errs))
	for k, v := range e.errs {
		s.Errors = append(s.Errors, ErrCount{Err: k, Count: v})
	}
	sort.Slice(s.Errors, func(i, j int) bool {
		if s.Errors[i].Count != s.Errors[j].Count {
			return s.Errors[i].Count > s.Errors[j].Count
		}
		return s.Errors[i].Err < s.Errors[j].Err
	})

	// Finish buckets.
	if a.nBuckets > 0 {
		s.Buckets = e.buckets
		for i := range s.Buckets {
			b := &s.Buckets[i]
			if b.Up > 0 {
				b.MeanRTT = e.bkSum[i] / float64(b.Up)
				b.P95RTT = percentile(toFloat64(e.bkVals[i]), 95)
			} else {
				b.MinRTT = 0
			}
			if math.IsInf(b.MinRTT, 1) {
				b.MinRTT = 0
			}
		}
	}
	return s
}

// Overall aggregates every endpoint into one fleet-level summary. Latency
// percentiles are pooled across endpoints, which is the right question for
// "how is everything doing" and the wrong one for capacity planning.
func (a *Accumulator) Overall() Summary {
	agg := &acc{
		target: "ALL", errs: map[string]int{}, codes: map[int]int{},
	}
	var (
		first, last time.Time
		deltas      []float64
	)
	for _, k := range a.order {
		e := a.byKey[k]
		agg.samples += e.samples
		agg.up += e.up
		agg.bytesTotal += e.bytesTotal
		agg.okRTT = append(agg.okRTT, e.okRTT...)
		agg.failRTT = append(agg.failRTT, e.failRTT...)
		agg.apdexScore += e.apdexScore
		for kk, v := range e.errs {
			agg.errs[kk] += v
		}
		for kk, v := range e.codes {
			agg.codes[kk] += v
		}
		deltas = append(deltas, e.deltas...)
		if first.IsZero() || e.firstAt.Before(first) {
			first = e.firstAt
		}
		if e.lastRes.At.After(last) {
			last = e.lastRes.At
		}
	}
	agg.firstAt = first
	agg.lastRes = model.Result{At: last}
	agg.streakAt = last
	agg.deltas = deltas

	s := a.finish(agg)
	// Outage/streak structure is per-endpoint and does not aggregate
	// meaningfully; clear it rather than report a misleading zero.
	s.Outages, s.OutageCount = nil, 0
	s.MTTR, s.MTBF, s.LongestOutage, s.Downtime = 0, 0, 0, 0
	s.StreakLength, s.StreakCount = 0, 0
	s.Target = "ALL"
	return s
}

// ---------------------------------------------------------------------------
// numerics
// ---------------------------------------------------------------------------

func toFloat64(in []float32) []float64 {
	out := make([]float64, len(in))
	for i, v := range in {
		out[i] = float64(v)
	}
	return out
}

// describe computes the full distribution. It sorts a copy, so the caller's
// insertion order survives.
func describe(in []float32) Dist {
	d := Dist{N: len(in)}
	if len(in) == 0 {
		return d
	}
	v := toFloat64(in)
	sort.Float64s(v)

	d.Min, d.Max = v[0], v[len(v)-1]
	var sum float64
	for _, x := range v {
		sum += x
	}
	d.Mean = sum / float64(len(v))
	d.Median = percentile(v, 50)
	d.P25 = percentile(v, 25)
	d.P75 = percentile(v, 75)
	d.P90 = percentile(v, 90)
	d.P95 = percentile(v, 95)
	d.P99 = percentile(v, 99)
	d.IQR = d.P75 - d.P25

	if len(v) > 1 {
		var m2, m3 float64
		for _, x := range v {
			dx := x - d.Mean
			m2 += dx * dx
			m3 += dx * dx * dx
		}
		d.StdDev = math.Sqrt(m2 / float64(len(v)-1))
		// Population second moment for the skewness denominator.
		s2 := m2 / float64(len(v))
		if s2 > 0 {
			d.Skew = (m3 / float64(len(v))) / math.Pow(s2, 1.5)
		}
	}
	if d.Mean != 0 {
		d.CV = d.StdDev / d.Mean
	}

	// MAD: median of |x - median|.
	dev := make([]float64, len(v))
	for i, x := range v {
		dev[i] = math.Abs(x - d.Median)
	}
	sort.Float64s(dev)
	d.MAD = percentile(dev, 50)

	return d
}

// percentile returns the p-th percentile (0..100) of an already-sorted slice,
// using linear interpolation between the two neighbouring order statistics.
func percentile(sorted []float64, p float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n == 1 {
		return sorted[0]
	}
	if p <= 0 {
		return sorted[0]
	}
	if p >= 100 {
		return sorted[n-1]
	}
	pos := (p / 100) * float64(n-1)
	lo := int(math.Floor(pos))
	hi := lo + 1
	if hi >= n {
		return sorted[n-1]
	}
	frac := pos - float64(lo)
	return sorted[lo] + frac*(sorted[hi]-sorted[lo])
}

// nines converts an availability fraction into "how many nines", so 0.999
// reads as 3.0 and 0.9999 as 4.0. Perfect availability over a finite sample
// is reported as the best the sample size can support rather than +Inf.
func nines(avail float64) float64 {
	if avail >= 1 {
		return math.Inf(1)
	}
	if avail <= 0 {
		return 0
	}
	return -math.Log10(1 - avail)
}

// SortSummaries orders summaries by one of a few useful keys. Unknown keys
// fall back to name order.
func SortSummaries(in []Summary, key string) {
	less := func(i, j int) bool { return in[i].Target < in[j].Target }
	switch key {
	case "availability", "uptime":
		less = func(i, j int) bool { return in[i].Availability < in[j].Availability }
	case "rtt", "latency":
		less = func(i, j int) bool { return in[i].RTT.Median > in[j].RTT.Median }
	case "p95":
		less = func(i, j int) bool { return in[i].RTT.P95 > in[j].RTT.P95 }
	case "outages":
		less = func(i, j int) bool { return in[i].OutageCount > in[j].OutageCount }
	case "samples":
		less = func(i, j int) bool { return in[i].Samples > in[j].Samples }
	case "apdex":
		less = func(i, j int) bool { return in[i].Apdex < in[j].Apdex }
	case "group":
		less = func(i, j int) bool {
			if in[i].Group != in[j].Group {
				return in[i].Group < in[j].Group
			}
			return in[i].Target < in[j].Target
		}
	}
	sort.SliceStable(in, less)
}
