package demand

import "time"

// Source identifies a telemetry origin.
type Source string

const (
	SourceGrid    Source = "grid"    // incoming-line power
	SourceBattery Source = "battery" // storage PCS
	SourceLoad    Source = "load"    // adjustable load
)

// Sample is a raw telemetry observation from any source.
//
// Power is measured in kW and follows the source sign convention:
//   - grid:   positive = importing power from the utility
//   - battery: positive = discharging, negative = charging
//   - load:   always positive consumption
//
// SOC is the storage state of charge in percent [0,100] (battery only).
// EnergyWh is the cumulative throughput energy in Wh:
// for a battery it is the signed exported energy (discharge increases it).
type Sample struct {
	ID       string
	Source   Source
	DeviceID string
	At       time.Time
	PowerKW  float64
	SOC      float64
	EnergyWh float64
	TempC    float64
	// Checksum, when set, must equal ChecksumOf(s) for the sample to be valid.
	Checksum string
}

// RejectReason explains why a sample was denied entry into decision state.
type RejectReason string

const (
	RejectUnknownSource RejectReason = "unknown_source"
	RejectMissingID     RejectReason = "missing_id"
	RejectZeroTime      RejectReason = "zero_timestamp"
	RejectFuture        RejectReason = "future_timestamp"
	RejectTooOld        RejectReason = "too_old"
	RejectNaN           RejectReason = "field_not_finite"
	RejectPowerRange    RejectReason = "power_out_of_range"
	RejectSOCRange      RejectReason = "soc_out_of_range"
	RejectTempRange     RejectReason = "temperature_out_of_range"
	RejectChecksum      RejectReason = "checksum_mismatch"
	RejectDuplicate     RejectReason = "duplicate_id"
	RejectStaleReplay   RejectReason = "stale_replay"
	RejectSOCJump       RejectReason = "soc_energy_mismatch"
)

// RejectedSample records a refused sample and the first failing reason.
type RejectedSample struct {
	Sample Sample
	Reason RejectReason
	Detail string
}

// IngestResult is the outcome of feeding a batch to the telemetry store.
type IngestResult struct {
	Accepted []Sample
	Rejected []RejectedSample
}

// Accepted reports whether the sample with the given id was newly accepted.
func (r IngestResult) AcceptedID(id string) bool {
	for _, s := range r.Accepted {
		if s.ID == id {
			return true
		}
	}
	return false
}

// RejectionFor returns the rejection record for id, if any.
func (r IngestResult) RejectionFor(id string) (RejectedSample, bool) {
	for _, x := range r.Rejected {
		if x.Sample.ID == id {
			return x, true
		}
	}
	return RejectedSample{}, false
}

// ActionKind enumerates controller outputs.
type ActionKind string

const (
	ActBatteryDischarge ActionKind = "battery_discharge"
	ActBatteryCharge    ActionKind = "battery_charge"
	ActBatteryIdle      ActionKind = "battery_idle"
	ActLoadShed         ActionKind = "load_shed"
	ActLoadRestore      ActionKind = "load_restore"
	ActNothing          ActionKind = "none"
)

// ActionCause gives a distinguishable reason for an action.
type ActionCause string

const (
	CausePeakAtRisk     ActionCause = "peak_projection_at_risk"
	CauseCapAtRisk      ActionCause = "cap_projection_at_risk"
	CauseStorageLimited ActionCause = "storage_at_backup_or_limited"
	CauseValleyCharge   ActionCause = "valley_surplus_charge"
	CauseStableRecovery ActionCause = "stable_recovery"
	CauseManualOverride ActionCause = "manual_override"
	CauseFaultConserve  ActionCause = "fault_conservative"
	CauseFaultClear     ActionCause = "fault_cleared"
	CauseUnchanged      ActionCause = "setpoint_unchanged"
)

// Action is one emitted control instruction.
type Action struct {
	Kind     ActionKind
	Cause    ActionCause
	DeviceID string
	// KW is the battery power setpoint (discharge positive) or the
	// remaining allowed power for a load (0 means fully shed).
	KW       float64
	At       time.Time
	AuditRef string
}

func (a Action) key() string {
	return string(a.Kind) + "|" + a.DeviceID + "|" + quantize(a.KW)
}

// Decision is the result of one controller tick.
type Decision struct {
	At time.Time
	// WindowStart..WindowEnd is the fixed demand window currently in progress.
	WindowStart time.Time
	WindowEnd   time.Time
	// CurrentDemandKW is the time-weighted mean grid power so far this window.
	CurrentDemandKW float64
	// ProjectedDemandKW extrapolates the current mean over the full window.
	ProjectedDemandKW float64
	PeakKW            float64 // historical rolling peak (completed windows)
	PeakAt            time.Time
	// RemainingMinutes until the window closes.
	RemainingMinutes float64
	Fault            FaultState
	Actions          []Action
	// EmittedActions contains only newly raised/changed instructions; the
	// controller suppresses unchanged repeats and never drops a real change.
	EmittedActions []Action
	// Deterministic fingerprint over inputs + state + time.
	Fingerprint string
}

// FaultState describes conservative-mode reasons.
type FaultState struct {
	Active  bool
	Reasons []string
	Since   time.Time
	Detail  string
}

// AuditRecord is an immutable entry for forced manual operations.
type AuditRecord struct {
	At       time.Time
	Operator string
	DeviceID string
	Kind     ActionKind
	KW       float64
	Reason   string
	Ref      string
}

// Limits bound plausible measurements; impossible values are rejected.
type Limits struct {
	MaxAbsGridKW  float64
	MaxAbsBattKW  float64
	MaxLoadKW     float64
	MaxTempC      float64
	MinTempC      float64
	MaxSampleAge  time.Duration
	MaxFutureSkew time.Duration
}

// DefaultLimits are permissive园区-scale bounds.
func DefaultLimits() Limits {
	return Limits{
		MaxAbsGridKW:  100_000,
		MaxAbsBattKW:  100_000,
		MaxLoadKW:     100_000,
		MaxTempC:      85,
		MinTempC:      -40,
		MaxSampleAge:  6 * time.Hour,
		MaxFutureSkew: 5 * time.Second,
	}
}
