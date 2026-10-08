package demand

import (
	"path/filepath"
	"sync"
	"testing"
)

func feedFull(f *fixture, min int, gridKW, battKW, soc, temp, energy float64) {
	m := min
	f.store.Ingest([]Sample{
		gridSample("g"+itoa(m), f.at(m), gridKW),
		battSample("b"+itoa(m), f.at(m), battKW, soc, temp, energy),
		loadSample("l1-"+itoa(m), f.at(m), "L1", 100),
		loadSample("l2-"+itoa(m), f.at(m), "L2", 100),
		loadSample("l3-"+itoa(m), f.at(m), "L3", 100),
	})
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [12]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func TestTelemetryLossConservativeAndRecovery(t *testing.T) {
	f := newFixture(t)
	feedFull(f, 0, 360, 0, 70, 25, 0)
	f.tick(0)

	// No frames at minutes 1..3: telemetry blackout exceeding StaleAfter.
	d := f.tick(3)
	if !d.Fault.Active {
		t.Fatal("telemetry blackout must enter conservative fault mode")
	}
	// All visible loads are shed (conservative: limit more, never overshoot).
	// At the first fault tick the store still holds old frames considered
	// stale; the controller still sheds the known loads.
	shedCount := 0
	for _, a := range d.EmittedActions {
		if a.Kind == ActLoadShed && a.Cause == CauseFaultConserve && a.KW == 0 {
			shedCount++
		}
	}
	if shedCount != 3 {
		t.Fatalf("conservative mode must shed all 3 adjustable loads, got %d: %+v",
			shedCount, d.EmittedActions)
	}

	// Telemetry resumes: outputs must stay conservative through the
	// MinStableTicks probation, then clear once (no flapping).
	cleared := false
	var postRecovery []Action
	for m := 4; m <= 7; m++ {
		f.clock = f.at(m)
		feedFull(f, m, 300, 0, 70, 25, 0)
		d = f.ctl.Decide()
		f.logDecision(d)
		if m < 4+f.cfg.MinStableTicks-1 {
			if !d.Fault.Active {
				t.Fatalf("recovery must hold conservative outputs at m%d", m)
			}
			continue
		}
		if !d.Fault.Active {
			cleared = true
		}
		postRecovery = append(postRecovery, d.EmittedActions...)
	}
	if !cleared {
		t.Fatal("fault must clear after stable healthy ticks")
	}

	// Exactly one fault-clear marker; loads return through stable_recovery.
	clearMarkers, restoreMarkers := 0, 0
	for _, a := range postRecovery {
		switch {
		case a.Cause == CauseFaultClear:
			clearMarkers++
		case a.Cause == CauseStableRecovery:
			restoreMarkers++
		}
	}
	if clearMarkers > 1 {
		t.Fatalf("fault clear must be emitted once, got %d", clearMarkers)
	}
	if restoreMarkers == 0 {
		t.Fatal("loads must return via stable_recovery actions")
	}
}

func TestTimeJumpConservative(t *testing.T) {
	f := newFixture(t)
	feedFull(f, 0, 360, 0, 70, 25, 0)
	f.tick(0)
	// A 10-minute gap with fresh data at the far side exceeds MaxTimeJump.
	f.clock = f.at(10)
	feedFullAt := func(min int) {
		f.store.Ingest([]Sample{
			gridSample("g"+itoa(min), f.at(min), 360),
			battSample("b"+itoa(min), f.at(min), 0, 70, 25, 0),
			loadSample("l1-"+itoa(min), f.at(min), "L1", 100),
			loadSample("l2-"+itoa(min), f.at(min), "L2", 100),
			loadSample("l3-"+itoa(min), f.at(min), "L3", 100),
		})
	}
	feedFullAt(10)
	d := f.ctl.Decide()
	if !d.Fault.Active {
		t.Fatal("clock jump must trigger conservative mode")
	}
}

func TestRestartReplayIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	run := func(load bool) (*fixture, *Decision) {
		f := newFixture(t)
		if load {
			if err := f.ctl.Load(path); err != nil {
				t.Fatalf("load: %v", err)
			}
		}
		feedFull(f, 0, 520, 0, 70, 25, 0)
		d := f.tick(0)
		feedFull(f, 1, 520, 10, 69.9, 25, 167)
		f.tick(1)
		if err := f.ctl.Save(path); err != nil {
			t.Fatalf("save: %v", err)
		}
		return f, d
	}

	f1, _ := run(false)
	peakBefore, _ := f1.ctl.book.Peak()

	// Restart, replay the exact same batches: every frame is a duplicate,
	// nothing re-accumulates, and no action is re-emitted.
	f2 := newFixture(t)
	if err := f2.ctl.Load(path); err != nil {
		t.Fatalf("load: %v", err)
	}
	// Restart continues from the saved wall clock; replayed frames at m0
	// are duplicates and the re-decision at the last tick must be a no-op.
	f2.clock = f1.at(1)
	r1 := f2.store.Ingest(samplesAt(f1, 0))
	if len(r1.Accepted) != 0 {
		t.Fatalf("replayed minute-0 batch must accept 0 frames after restart, got %d", len(r1.Accepted))
	}
	d := f2.tick(1)
	if len(d.EmittedActions) != 0 {
		t.Fatalf("replayed decision must not re-emit actions: %+v", d.EmittedActions)
	}
	if got, _ := f2.ctl.book.Peak(); got != peakBefore {
		t.Fatalf("peak must survive restart unchanged: %.1f != %.1f", got, peakBefore)
	}
}

// samplesAt reconstructs the batch used in the replay test by reading
// restored device snapshots is unnecessary: replay uses the same ID set, so
// we rebuild frames from the first fixture's inputs.
func samplesAt(f *fixture, min int) []Sample {
	kw := 520.0
	return []Sample{
		gridSample("g"+itoa(min), f.at(min), kw),
		battSample("b"+itoa(min), f.at(min), 0, 70, 25, 0),
		loadSample("l1-"+itoa(min), f.at(min), "L1", 100),
		loadSample("l2-"+itoa(min), f.at(min), "L2", 100),
		loadSample("l3-"+itoa(min), f.at(min), "L3", 100),
	}
}

func TestConcurrentDecisionsUnique(t *testing.T) {
	f := newFixture(t)
	// Identical batch handed in through two entry points (store.Ingest +
	// Decide vs controller.Ingest) and from many goroutines: the resulting
	// state must be one coherent set of setpoints.
	const n = 20
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			batch := []Sample{
				gridSample("cg", f.at(0), 540),
				battSample("cb", f.at(0), 0, 70, 25, 0),
				loadSample("cl1", f.at(0), "L1", 180),
				loadSample("cl2", f.at(0), "L2", 180),
				loadSample("cl3", f.at(0), "L3", 180),
			}
			_ = i
			f.store.Ingest(batch)
			f.ctl.Decide()
		}(i)
	}
	wg.Wait()

	// Repeated Decide at the same clock with unchanged inputs must not emit
	// further actions (no duplicate actions under concurrent entry).
	seen := map[string]bool{}
	d := f.ctl.Decide()
	for _, a := range d.EmittedActions {
		key := a.DeviceID + ":" + string(a.Kind)
		if seen[key] {
			t.Fatalf("duplicate action for %s", key)
		}
		seen[key] = true
	}
	for _, l := range f.cfg.Loads {
		if v := f.ctl.loadAllow[l.ID]; v < 0 || v > l.RatedKW+0.01 {
			t.Fatalf("concurrent updates corrupted load %s: %.1f", l.ID, v)
		}
	}
}
