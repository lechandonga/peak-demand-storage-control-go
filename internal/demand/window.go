package demand

import "time"

// WindowBook keeps fixed, epoch-aligned demand windows and the historical
// peak. Window width is fixed (default 15m): every window mean uses the
// identical rule, so day/month boundaries need no special casing — windows
// are anchored to the zero instant and are labeled by local calendar day.
type WindowBook struct {
	Width time.Duration
	Loc   *time.Location

	// Current window accumulator.
	start         time.Time
	weightedSumKW float64 // power weighted by duration in minutes
	weightMinutes float64
	lastAt        time.Time
	lastPower     float64
	haveSample    bool

	// Historical peak across completed windows.
	peakKW float64
	peakAt time.Time

	// Completed-window values for transparent statistics.
	Completed []WindowResult
}

// WindowResult is one finalized demand window.
type WindowResult struct {
	Start    time.Time
	End      time.Time
	DemandKW float64
	Day      string // YYYY-MM-DD in the configured location
	Month    string // YYYY-MM in the configured location
}

// NewWindowBook creates an empty book aligned to width.
func NewWindowBook(width time.Duration, loc *time.Location) *WindowBook {
	if loc == nil {
		loc = time.Local
	}
	return &WindowBook{Width: width, Loc: loc}
}

// WindowStart returns the epoch-aligned start containing t.
func (b *WindowBook) WindowStart(t time.Time) time.Time {
	u := t.UnixNano()
	w := int64(b.Width)
	return time.Unix(0, u-u%w).In(b.Loc)
}

// Add integrates a grid-power observation at time t using time-weighted
// (sample-and-hold) integration; crossing a boundary finalizes the window.
func (b *WindowBook) Add(t time.Time, powerKW float64) {
	ws := b.WindowStart(t)
	if !b.haveSample {
		b.start = ws
		b.lastAt = t
		b.lastPower = powerKW
		b.haveSample = true
		return
	}
	if ws.Equal(b.start) {
		// Integrate the segment from the previous sample to this one.
		d := t.Sub(b.lastAt).Minutes()
		if d > 0 {
			b.weightedSumKW += b.lastPower * d
			b.weightMinutes += d
		}
		b.lastAt, b.lastPower = t, powerKW
		return
	}
	// Boundary crossed: finalize all intermediate windows (gap windows are
	// finalized with the held value so accounting never skips a window).
	b.rollForward(t, powerKW)
}

func (b *WindowBook) rollForward(t time.Time, powerKW float64) {
	// Segment up to the boundary completes the current window.
	boundary := b.start.Add(b.Width)
	d := boundary.Sub(b.lastAt).Minutes()
	if d > 0 {
		b.weightedSumKW += b.lastPower * d
		b.weightMinutes += d
	}
	b.finalize(boundary)

	nextStart := boundary
	for nextStart.Add(b.Width).Compare(t) <= 0 {
		// Entire window held at the last known value.
		b.weightedSumKW = b.lastPower * b.Width.Minutes()
		b.weightMinutes = b.Width.Minutes()
		b.start = nextStart
		nextStart = nextStart.Add(b.Width)
		b.finalize(nextStart)
	}
	// Current (partial) window begins at nextStart with the held power.
	b.start = nextStart
	b.weightedSumKW = 0
	b.weightMinutes = 0
	b.lastAt = t
	b.lastPower = powerKW
}

func (b *WindowBook) finalize(end time.Time) {
	dem := 0.0
	if b.weightMinutes > 0 {
		dem = b.weightedSumKW / b.weightMinutes
	}
	if dem > b.peakKW {
		b.peakKW = dem
		b.peakAt = b.start
	}
	b.Completed = append(b.Completed, WindowResult{
		Start:    b.start,
		End:      end,
		DemandKW: dem,
		Day:      b.start.Format("2006-01-02"),
		Month:    b.start.Format("2006-01"),
	})
	b.weightedSumKW, b.weightMinutes = 0, 0
}

// CurrentDemand returns the time-weighted mean of the window in progress.
func (b *WindowBook) CurrentDemand() float64 {
	if !b.haveSample || b.weightMinutes == 0 {
		if b.haveSample {
			return b.lastPower
		}
		return 0
	}
	return b.weightedSumKW / b.weightMinutes
}

// CurrentWindowStart reports the start of the in-progress window.
func (b *WindowBook) CurrentWindowStart() time.Time { return b.start }

// resetAt drops the in-progress accumulator after a clock anomaly while
// preserving completed windows and the historical peak.
func (b *WindowBook) resetAt(t time.Time) {
	b.start = b.WindowStart(t)
	b.weightedSumKW, b.weightMinutes = 0, 0
	b.lastAt = t
	b.haveSample = true
}

// Peak returns the running peak and when it occurred.
func (b *WindowBook) Peak() (float64, time.Time) { return b.peakKW, b.peakAt }

// Restore replaces internal accumulators from persisted state.
func (b *WindowBook) Restore(p windowPersist) {
	b.start = p.Start
	b.weightedSumKW = p.WeightedSum
	b.weightMinutes = p.WeightMin
	b.lastAt = p.LastAt
	b.lastPower = p.LastPower
	b.haveSample = p.Have
	b.peakKW = p.PeakKW
	b.peakAt = p.PeakAt
	b.Completed = append(b.Completed, p.Completed...)
}

type windowPersist struct {
	Start       time.Time
	WeightedSum float64
	WeightMin   float64
	LastAt      time.Time
	LastPower   float64
	Have        bool
	PeakKW      float64
	PeakAt      time.Time
	Completed   []WindowResult
}

func (b *WindowBook) persist() windowPersist {
	return windowPersist{
		Start: b.start, WeightedSum: b.weightedSumKW, WeightMin: b.weightMinutes,
		LastAt: b.lastAt, LastPower: b.lastPower, Have: b.haveSample,
		PeakKW: b.peakKW, PeakAt: b.peakAt,
		Completed: append([]WindowResult(nil), b.Completed...),
	}
}
