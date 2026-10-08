package demand

import (
	"sort"
	"sync"
	"time"
)

// Controller runs demand-limit control with storage coordination.
// All public methods are safe for concurrent use; Decide holds a single
// mutex so the same batch at any entry point yields one unique outcome.
type Controller struct {
	mu    sync.Mutex
	cfg   Config
	store *Store
	book  *WindowBook
	now   func() time.Time

	lastTick time.Time
	started  bool
	fault    FaultState
	healthy  int

	batteryKW map[string]float64 // discharge-positive setpoint
	loadAllow map[string]float64 // allowed kW per adjustable load
	lastEmit  map[string]string  // device -> last emitted action key

	overrides map[string]AuditRecord
	audits    []AuditRecord

	pendingEmitted []Action
	tickCount      int64
}

// NewController builds a controller over the given store.
func NewController(cfg Config, store *Store, now func() time.Time) *Controller {
	cfg.normalize()
	if now == nil {
		now = time.Now
	}
	c := &Controller{
		cfg:       cfg,
		store:     store,
		book:      NewWindowBook(cfg.WindowWidth, cfg.Loc),
		now:       now,
		batteryKW: map[string]float64{},
		loadAllow: map[string]float64{},
		lastEmit:  map[string]string{},
		overrides: map[string]AuditRecord{},
	}
	for _, l := range cfg.Loads {
		c.loadAllow[l.ID] = l.RatedKW
	}
	for _, b := range cfg.Batteries {
		c.batteryKW[b.ID] = 0
	}
	return c
}

// Ingest is the common entry point for telemetry. It validates the batch in
// the store and then runs exactly one decision.
func (c *Controller) Ingest(batch []Sample) (IngestResult, *Decision) {
	res := c.store.Ingest(batch)
	d := c.Decide()
	return res, d
}

// Status of devices collected from the snapshot.
type battView struct {
	id      string
	present bool
	fresh   bool
	age     time.Duration
	status  BatteryStatus
}

type loadView struct {
	id      string
	spec    LoadSpec
	present bool
	fresh   bool
	age     time.Duration
}

func pickLatest(snap map[string]agedSample, src Source) (agedSample, bool) {
	var best agedSample
	found := false
	for k, v := range snap {
		if keySource(k) != src {
			continue
		}
		if !found || v.sample.At.After(best.sample.At) {
			best, found = v, true
		}
	}
	return best, found
}

func keySource(k string) Source {
	for _, s := range []Source{SourceGrid, SourceBattery, SourceLoad} {
		if len(k) >= len(s)+1 && k[:len(s)+1] == string(s)+":" {
			return s
		}
	}
	return ""
}

func pickAllBatt(snap map[string]agedSample, specs []BatterySpec, stale time.Duration) []battView {
	out := make([]battView, 0, len(specs))
	for _, sp := range specs {
		v := battView{id: sp.ID, status: deriveStatus(sp, sp.BackupSOC, 25)}
		if a, ok := snap[string(SourceBattery)+":"+sp.ID]; ok {
			v.present = true
			v.age = a.age
			v.fresh = a.age < stale
			v.status = deriveStatus(sp, a.sample.SOC, a.sample.TempC)
		}
		out = append(out, v)
	}
	return out
}

func pickAllLoad(snap map[string]agedSample, specs []LoadSpec, stale time.Duration) []loadView {
	out := make([]loadView, 0, len(specs))
	for _, sp := range specs {
		v := loadView{id: sp.ID, spec: sp}
		if a, ok := snap[string(SourceLoad)+":"+sp.ID]; ok {
			v.present = true
			v.age = a.age
			v.fresh = a.age < stale
		}
		out = append(out, v)
	}
	return out
}

func sortedBatts(vs []battView) []battView {
	sort.Slice(vs, func(i, j int) bool { return vs[i].id < vs[j].id })
	return vs
}

func sortedLoads(vs []loadView) []loadView {
	sort.Slice(vs, func(i, j int) bool {
		if vs[i].spec.Priority != vs[j].spec.Priority {
			return vs[i].spec.Priority < vs[j].spec.Priority
		}
		return vs[i].id < vs[j].id
	})
	return vs
}

func reverseSortedLoads(vs []loadView) []loadView {
	s := sortedLoads(vs)
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
	return s
}
