// Package series keeps a bounded, compact, in-memory view of recent samples.
//
// The TUI needs to recompute statistics and redraw charts several times a
// second, which rules out re-reading the day files on every frame, and storing
// model.Result directly costs about 130 bytes per sample once the string
// fields are counted. A Sample here is 24 bytes with error text interned per
// endpoint, so a 24-hour window over 90 endpoints at 60s costs roughly 3 MB
// instead of 17 MB — which is the whole point of the exercise, given that the
// tool this replaces used 100 MB before it had drawn anything.
package series

import (
	"sync"
	"time"

	"github.com/didvc/uptime-mon/internal/model"
	"github.com/didvc/uptime-mon/internal/stats"
)

// Sample is one observation, stripped to what the stats engine consumes.
type Sample struct {
	UnixNano int64
	RTTms    float32
	Bytes    int32
	Code     uint16
	errID    uint16
	Up       bool
	_        [3]byte // explicit padding; the struct is 24 bytes either way
}

// Series is the sample history for one endpoint, as a ring buffer.
type Series struct {
	Target string
	Group  string
	Host   string
	Kind   model.Kind

	buf   []Sample
	start int // index of the oldest sample
	n     int // number of live samples

	errs  []string // errID -> text; [0] is always ""
	errID map[string]uint16
}

func newSeries(r model.Result, max int) *Series {
	return &Series{
		Target: r.Target, Group: r.Group, Host: r.Host, Kind: r.Kind,
		buf:   make([]Sample, 0, min(max, 1024)),
		errs:  []string{""},
		errID: map[string]uint16{"": 0},
	}
}

// Len reports how many samples are retained.
func (s *Series) Len() int { return s.n }

// intern maps error text to a small id, capping the table so that an endpoint
// producing endlessly unique errors cannot grow it without bound.
func (s *Series) intern(text string) uint16 {
	if text == "" {
		return 0
	}
	if id, ok := s.errID[text]; ok {
		return id
	}
	if len(s.errs) >= 1<<12 {
		return 0
	}
	id := uint16(len(s.errs))
	s.errs = append(s.errs, text)
	s.errID[text] = id
	return id
}

func (s *Series) errText(id uint16) string {
	if int(id) < len(s.errs) {
		return s.errs[id]
	}
	return ""
}

func (s *Series) add(r model.Result, max int) {
	sm := Sample{
		UnixNano: r.At.UnixNano(),
		RTTms:    float32(float64(r.RTT) / float64(time.Millisecond)),
		Code:     uint16(r.Code),
		Up:       r.Status == model.StatusUp,
		errID:    s.intern(r.Err),
	}
	if r.Bytes > 0 {
		b := r.Bytes
		if b > 1<<31-1 {
			b = 1<<31 - 1
		}
		sm.Bytes = int32(b)
	}

	if len(s.buf) < max {
		s.buf = append(s.buf, sm)
		s.n++
		return
	}
	// Full: overwrite the oldest.
	s.buf[s.start] = sm
	s.start = (s.start + 1) % max
}

// at returns the i-th oldest sample. While the ring is still filling, start is
// zero and this is a plain index; once it wraps, start marks the oldest.
func (s *Series) at(i int) Sample {
	return s.buf[(s.start+i)%len(s.buf)]
}

// decode expands a stored Sample back into the shape the stats engine wants.
func (s *Series) decode(sm Sample) model.Result {
	st := model.StatusDown
	if sm.Up {
		st = model.StatusUp
	}
	return model.Result{
		Target: s.Target, Group: s.Group, Host: s.Host, Kind: s.Kind,
		At:     time.Unix(0, sm.UnixNano),
		Status: st,
		Code:   int(sm.Code),
		RTT:    time.Duration(float64(sm.RTTms) * float64(time.Millisecond)),
		Bytes:  int64(sm.Bytes),
		Err:    s.errText(sm.errID),
	}
}

// Each calls fn for every retained sample in [from, to), oldest first.
func (s *Series) Each(from, to time.Time, fn func(model.Result)) {
	lo, hi := from.UnixNano(), to.UnixNano()
	for i := 0; i < s.n; i++ {
		sm := s.at(i)
		if sm.UnixNano < lo || sm.UnixNano >= hi {
			continue
		}
		fn(s.decode(sm))
	}
}

// Last returns the most recent sample, if any.
func (s *Series) Last() (model.Result, bool) {
	if s.n == 0 {
		return model.Result{}, false
	}
	return s.decode(s.at(s.n - 1)), true
}

// ---------------------------------------------------------------------------

// Set is a concurrent collection of Series, one per endpoint. The collector
// writes to it from probe goroutines while the TUI reads from the render loop.
type Set struct {
	mu     sync.RWMutex
	order  []string
	byName map[string]*Series
	max    int
}

// NewSet returns a Set retaining at most maxPerSeries samples per endpoint.
func NewSet(maxPerSeries int) *Set {
	if maxPerSeries <= 0 {
		maxPerSeries = 2880 // 24h at 30s
	}
	return &Set{byName: make(map[string]*Series), max: maxPerSeries}
}

// Add records one result.
func (s *Set) Add(r model.Result) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ser := s.byName[r.Target]
	if ser == nil {
		ser = newSeries(r, s.max)
		s.byName[r.Target] = ser
		s.order = append(s.order, r.Target)
	}
	if r.Group != "" {
		ser.Group = r.Group
	}
	if r.Host != "" {
		ser.Host = r.Host
	}
	if r.Kind != "" {
		ser.Kind = r.Kind
	}
	ser.add(r, s.max)
}

// Adopt replaces this set's contents with those of other.
//
// The read-only TUI rebuilds a whole set from disk on each refresh and swaps
// it in here, so a reader never observes a half-loaded set: it sees either the
// previous window or the new one, never a set that is midway through being
// refilled.
func (s *Set) Adopt(other *Set) {
	other.mu.RLock()
	order, byName, max := other.order, other.byName, other.max
	other.mu.RUnlock()

	s.mu.Lock()
	s.order, s.byName, s.max = order, byName, max
	s.mu.Unlock()
}

// Reset drops all samples, e.g. when the TUI reloads a different window.
func (s *Set) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.order = nil
	s.byName = make(map[string]*Series)
}

// Names returns endpoint names in first-seen order.
func (s *Set) Names() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]string(nil), s.order...)
}

// Len reports the number of endpoints seen.
func (s *Set) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.order)
}

// Samples reports the total retained sample count, for the status line.
func (s *Set) Samples() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	total := 0
	for _, ser := range s.byName {
		total += ser.n
	}
	return total
}

// Summaries computes statistics over [from, to) for every endpoint.
//
// Endpoints are seeded into the accumulator in a stable order even when they
// have no samples in the window, so a silent endpoint keeps its row in the
// TUI instead of vanishing.
func (s *Set) Summaries(from, to time.Time, buckets int, apdexT time.Duration) []stats.Summary {
	s.mu.RLock()
	defer s.mu.RUnlock()

	acc := stats.NewAccumulator(from, to, buckets, apdexT)
	for _, name := range s.order {
		s.byName[name].Each(from, to, acc.Add)
	}
	return acc.Summaries()
}

// Overall computes the fleet-level summary over [from, to).
func (s *Set) Overall(from, to time.Time, apdexT time.Duration) stats.Summary {
	s.mu.RLock()
	defer s.mu.RUnlock()

	acc := stats.NewAccumulator(from, to, 0, apdexT)
	for _, name := range s.order {
		s.byName[name].Each(from, to, acc.Add)
	}
	return acc.Overall()
}

// LastByName returns the latest sample per endpoint.
func (s *Set) LastByName() map[string]model.Result {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]model.Result, len(s.order))
	for _, name := range s.order {
		if r, ok := s.byName[name].Last(); ok {
			out[name] = r
		}
	}
	return out
}
