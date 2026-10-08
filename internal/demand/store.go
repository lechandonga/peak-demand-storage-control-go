package demand

import (
	"math"
	"sort"
	"sync"
	"time"
)

// deviceState is the latest accepted observation of one device.
type deviceState struct {
	sample     Sample
	receivedAt time.Time
}

// Store accepts telemetry from multiple sources, enforces per-sample
// validation and idempotency, and keeps the latest state per device.
//
// It is safe for concurrent use: all decisions read a consistent snapshot.
type Store struct {
	mu      sync.Mutex
	limits  Limits
	now     func() time.Time
	seen    map[string]int64 // sample ID -> At unix nano (dedup)
	devices map[string]deviceState
	// lastEnergyAt[device] is the energy counter value of the latest sample;
	// counters must never move backwards.
	lastEnergy map[string]float64
	lastSOC    map[string]float64
	// reconcile holds per-battery energy models used to verify that the
	// reported SOC motion matches reported power + efficiency losses.
	reconcile map[string]ReconcileConfig
	lastBatt  map[string]Sample
	// acceptedOrder is the At-ordered stream for fingerprint stability.
	acceptedOrder []Sample
}

// NewStore builds a telemetry store. now may be nil for wall-clock time.
func NewStore(limits Limits, now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{
		limits:     limits,
		now:        now,
		seen:       map[string]int64{},
		devices:    map[string]deviceState{},
		lastEnergy: map[string]float64{},
		lastSOC:    map[string]float64{},
		reconcile:  map[string]ReconcileConfig{},
		lastBatt:   map[string]Sample{},
	}
}

// SetBatteryReconcile registers the physical model of a battery so that SOC
// changes are checked against charge/discharge energy including losses.
func (st *Store) SetBatteryReconcile(deviceID string, rc ReconcileConfig) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.reconcile[deviceID] = rc
}

// Ingest validates and applies a batch. Out-of-order delivery is supported:
// samples are sorted by (At, ID) and an older-but-new-information sample for a
// device is accepted, while a replayed duplicate frame is not.
func (st *Store) Ingest(batch []Sample) IngestResult {
	st.mu.Lock()
	defer st.mu.Unlock()

	ordered := make([]Sample, len(batch))
	copy(ordered, batch)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].At.Equal(ordered[j].At) {
			return ordered[i].ID < ordered[j].ID
		}
		return ordered[i].At.Before(ordered[j].At)
	})

	res := IngestResult{}
	now := st.now()
	// Batch-local dedup: identical IDs inside one batch are duplicates too.
	batchSeen := map[string]bool{}
	for _, s := range ordered {
		if reason, detail := st.validate(s, now); reason != "" {
			res.Rejected = append(res.Rejected, RejectedSample{Sample: s, Reason: reason, Detail: detail})
			continue
		}
		if batchSeen[s.ID] {
			res.Rejected = append(res.Rejected, RejectedSample{Sample: s, Reason: RejectDuplicate, Detail: "id repeated within batch"})
			continue
		}
		batchSeen[s.ID] = true
		st.apply(s, now)
		res.Accepted = append(res.Accepted, s)
		st.acceptedOrder = append(st.acceptedOrder, s)
	}
	return res
}

func (st *Store) validate(s Sample, now time.Time) (RejectReason, string) {
	switch s.Source {
	case SourceGrid, SourceBattery, SourceLoad:
	default:
		return RejectUnknownSource, string(s.Source)
	}
	if s.ID == "" {
		return RejectMissingID, "sample id is required for idempotent replay"
	}
	if s.At.IsZero() {
		return RejectZeroTime, "timestamp is zero"
	}
	if s.At.After(now.Add(st.limits.MaxFutureSkew)) {
		return RejectFuture, "timestamp later than clock + skew allowance"
	}
	if now.Sub(s.At) > st.limits.MaxSampleAge {
		return RejectTooOld, "sample older than max retention"
	}
	if prevNano, ok := st.seen[s.ID]; ok {
		if prevNano == s.At.UnixNano() {
			return RejectDuplicate, "sample id already ingested"
		}
		return RejectStaleReplay, "sample id reused with a different timestamp"
	}
	if !finite(s.PowerKW) || !finite(s.SOC) || !finite(s.EnergyWh) || !finite(s.TempC) {
		return RejectNaN, "numeric field is NaN/Inf"
	}
	if s.Checksum != "" && s.Checksum != ChecksumOf(s) {
		return RejectChecksum, "frame checksum does not match measurement fields"
	}
	switch s.Source {
	case SourceGrid:
		if math.Abs(s.PowerKW) > st.limits.MaxAbsGridKW {
			return RejectPowerRange, "grid power beyond configured bound"
		}
	case SourceBattery:
		if math.Abs(s.PowerKW) > st.limits.MaxAbsBattKW {
			return RejectPowerRange, "battery power beyond configured bound"
		}
		if s.SOC < 0 || s.SOC > 100 {
			return RejectSOCRange, "battery SOC must be within [0,100]%"
		}
		if s.TempC < st.limits.MinTempC || s.TempC > st.limits.MaxTempC {
			return RejectTempRange, "battery temperature outside safe envelope"
		}
		if prev, ok := st.lastEnergy[s.DeviceID]; ok && s.EnergyWh < prev-1 {
			return RejectSOCJump, "battery energy counter moved backwards"
		}
		if rc, ok := st.reconcile[s.DeviceID]; ok {
			if prev, had := st.lastBatt[s.DeviceID]; had && s.At.After(prev.At) {
				dtMin := s.At.Sub(prev.At).Minutes()
				expected := PredictSOC(rc, prev.SOC, prev.PowerKW, dtMin)
				tol := rc.SOCNoisePct
				if tol <= 0 {
					tol = 1.5
				}
				if math.Abs(expected-s.SOC) > tol {
					return RejectSOCJump, "SOC change does not reconcile with charge/discharge energy"
				}
			}
		}
	case SourceLoad:
		if s.PowerKW < 0 || s.PowerKW > st.limits.MaxLoadKW {
			return RejectPowerRange, "load power must be within [0,max] kW"
		}
	}
	return "", ""
}

func (st *Store) apply(s Sample, receivedAt time.Time) {
	st.seen[s.ID] = s.At.UnixNano()
	key := string(s.Source) + ":" + s.DeviceID
	prev, exists := st.devices[key]
	// Out-of-order: only overwrite when the new sample is newer.
	if !exists || !s.At.Before(prev.sample.At) {
		st.devices[key] = deviceState{sample: s, receivedAt: receivedAt}
	}
	if s.Source == SourceBattery {
		if v, ok := st.lastEnergy[s.DeviceID]; !ok || s.EnergyWh > v {
			st.lastEnergy[s.DeviceID] = s.EnergyWh
		}
		if v, ok := st.lastSOC[s.DeviceID]; !ok {
			st.lastSOC[s.DeviceID] = s.SOC
		} else if math.Abs(s.SOC-v) > 0 {
			st.lastSOC[s.DeviceID] = s.SOC
		}
		if prev, ok := st.lastBatt[s.DeviceID]; !ok || !s.At.Before(prev.At) {
			st.lastBatt[s.DeviceID] = s
		}
	}
}

// Snapshot returns an immutable copy of current device states.
func (st *Store) Snapshot() map[string]Sample {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make(map[string]Sample, len(st.devices))
	for k, v := range st.devices {
		out[k] = v.sample
	}
	return out
}

// snapshotWithAge returns the latest sample plus its age for staleness checks.
func (st *Store) snapshotWithAge(now time.Time) map[string]agedSample {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make(map[string]agedSample, len(st.devices))
	for k, v := range st.devices {
		out[k] = agedSample{sample: v.sample, age: now.Sub(v.sample.At)}
	}
	return out
}

type agedSample struct {
	sample Sample
	age    time.Duration
}

func finite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}
