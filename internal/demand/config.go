package demand

import "time"

// LoadSpec describes an adjustable load and its shedding priority.
type LoadSpec struct {
	ID string
	// RatedKW is normal operating power.
	RatedKW float64
	// MinKW is the lowest power at which the process can keep running;
	// when relief cannot be achieved above MinKW the load is fully shed.
	MinKW float64
	// Priority: smaller numbers are shed first (higher urgency cost to cut).
	Priority int
}

// Config holds all control parameters.
type Config struct {
	WindowWidth  time.Duration
	TickInterval time.Duration
	Loc          *time.Location

	// ContractKW is the contractual demand ceiling that must never break.
	ContractKW float64
	// RiskLeadKW: control engages when the projection enters this band
	// below the active target (acts before reaching the threshold).
	RiskLeadKW float64
	// RestoreLagKW: shed loads are only restored once the projection is
	// this far below the target (hysteresis to avoid chatter).
	RestoreLagKW float64
	// ValleyThresholdKW: projected demand below this level permits charging.
	ValleyThresholdKW float64
	// ChargeHeadroomKW keeps charging this margin under the active target.
	ChargeHeadroomKW float64
	// MinStableTicks: consecutive healthy ticks required before restoring.
	MinStableTicks int
	// StaleAfter: telemetry older than this is treated as lost.
	StaleAfter time.Duration
	// MaxTimeJump: a clock gap larger than this triggers conservative mode.
	MaxTimeJump time.Duration

	Batteries []BatterySpec
	Loads     []LoadSpec
}

func (c *Config) normalize() {
	if c.WindowWidth <= 0 {
		c.WindowWidth = 15 * time.Minute
	}
	if c.TickInterval <= 0 {
		c.TickInterval = time.Minute
	}
	if c.Loc == nil {
		c.Loc = time.Local
	}
	if c.RiskLeadKW <= 0 {
		c.RiskLeadKW = 5
	}
	if c.RestoreLagKW <= 0 {
		c.RestoreLagKW = 10
	}
	if c.ChargeHeadroomKW <= 0 {
		c.ChargeHeadroomKW = 10
	}
	if c.MinStableTicks <= 0 {
		c.MinStableTicks = 3
	}
	if c.StaleAfter <= 0 {
		c.StaleAfter = 3 * time.Minute
	}
	if c.MaxTimeJump <= 0 {
		c.MaxTimeJump = 5 * time.Minute
	}
}
