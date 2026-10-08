// Package sim provides a fully local simulated park: incoming line, storage
// and adjustable loads generate telemetry that exercises the demand
// controller without any external service.
package sim

import (
	"fmt"
	"sync"
	"time"

	"github.com/lechandonga/peak-demand-storage-control-go/internal/demand"
)

// Park is the simulated electrical park.
type Park struct {
	mu sync.Mutex

	BatterySpec demand.BatterySpec
	SOC         float64
	TempC       float64
	BaseLoadKW  float64
	Loads       []demand.LoadSpec

	// Current setpoints as last commanded (discharge positive).
	batteryKW float64
	loadAllow map[string]float64

	energyWh float64
	seq      int
	runID    string
}

// NewPark builds a reference park: 1000 kWh storage, 500 kW contract.
func NewPark() *Park {
	spec := demand.BatterySpec{
		ID: "BESS-1", CapacityWh: 1_000_000,
		MaxDischargeKW: 250, MaxChargeKW: 200,
		ChargeEff: 0.95, DischargeEff: 0.95,
		BackupSOC: 20,
		TempHot:   45, TempHotCut: 55,
		TempCold: 0, TempColdCut: -10,
	}
	loads := []demand.LoadSpec{
		{ID: "L-AHU", RatedKW: 120, MinKW: 30, Priority: 1},
		{ID: "L-PUMP", RatedKW: 90, MinKW: 40, Priority: 2},
		{ID: "L-EV", RatedKW: 150, MinKW: 0, Priority: 3},
	}
	p := &Park{
		BatterySpec: spec, SOC: 70, TempC: 28,
		BaseLoadKW: 300, Loads: loads,
		loadAllow: map[string]float64{
			"L-AHU": 120, "L-PUMP": 90, "L-EV": 150,
		},
		runID: fmt.Sprintf("RUN-%d", time.Now().UnixNano()),
	}
	return p
}

// Config returns the controller configuration for the park.
func (p *Park) Config(now time.Time) demand.Config {
	return demand.Config{
		WindowWidth: 15 * time.Minute, TickInterval: time.Minute,
		Loc:               now.Location(),
		ContractKW:        500,
		RiskLeadKW:        10,
		RestoreLagKW:      20,
		ValleyThresholdKW: 300,
		ChargeHeadroomKW:  30,
		MinStableTicks:    3,
		StaleAfter:        2 * time.Minute,
		MaxTimeJump:       5 * time.Minute,
		Batteries:         []demand.BatterySpec{p.BatterySpec},
		Loads:             p.Loads,
	}
}

// ApplyActions pushes controller setpoints into the simulated equipment.
func (p *Park) ApplyActions(acts []demand.Action) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range acts {
		switch a.DeviceID {
		case p.BatterySpec.ID:
			p.batteryKW = a.KW
		default:
			if _, ok := p.loadAllow[a.DeviceID]; ok {
				p.loadAllow[a.DeviceID] = a.KW
			}
		}
	}
}

// Step advances the physical park by dt minutes and returns telemetry frames.
// extraLoadKW models an external demand surge (e.g. production peak).
func (p *Park) Step(t time.Time, dtMin, extraLoadKW float64) []demand.Sample {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Snapshot setpoints commanded for THIS interval, then advance physics
	// and measure resulting power as the interval's telemetry (no lag).
	batteryKW := p.batteryKW
	// Battery energy responds to the commanded setpoint with losses.
	p.SOC = demand.ApplyBatteryEnergy(p.BatterySpec, p.SOC, batteryKW, dtMin)
	// Cumulative export counter: monotonically non-decreasing throughput,
	// constant during idle so frames never look like a backwards jump.
	if batteryKW > 0 {
		p.energyWh += batteryKW * (dtMin / 60) * 1000
	}

	// Loads: actual consumption follows the allowed setpoint plus surge,
	// shared proportionally to rated power.
	totalAllow := 0.0
	for _, l := range p.Loads {
		totalAllow += p.loadAllow[l.ID]
	}
	var totalLoad float64
	loadPowers := map[string]float64{}
	for _, l := range p.Loads {
		share := 0.0
		if totalAllow > 0 {
			share = p.loadAllow[l.ID] / totalAllow
		}
		kw := p.loadAllow[l.ID] + extraLoadKW*share
		loadPowers[l.ID] = kw
		totalLoad += kw
	}

	// Grid import = loads - battery discharge (+ charge).
	gridKW := totalLoad - batteryKW
	if gridKW < 0 {
		gridKW = 0
	}

	p.seq++
	frames := []demand.Sample{}
	grid := demand.Sample{
		ID: fmt.Sprintf("%s-GRID-%d", p.runID, p.seq), Source: demand.SourceGrid,
		DeviceID: "GRID-MAIN", At: t, PowerKW: gridKW,
	}
	grid.Checksum = demand.ChecksumOf(grid)
	batt := demand.Sample{
		ID: fmt.Sprintf("%s-BATT-%d", p.runID, p.seq), Source: demand.SourceBattery,
		DeviceID: p.BatterySpec.ID, At: t,
		PowerKW: batteryKW, SOC: p.SOC,
		EnergyWh: p.energyWh, TempC: p.TempC,
	}
	batt.Checksum = demand.ChecksumOf(batt)
	frames = append(frames, grid, batt)

	for _, l := range p.Loads {
		s := demand.Sample{
			ID: fmt.Sprintf("%s-%s-%d", p.runID, l.ID, p.seq), Source: demand.SourceLoad,
			DeviceID: l.ID, At: t, PowerKW: loadPowers[l.ID],
		}
		s.Checksum = demand.ChecksumOf(s)
		frames = append(frames, s)
	}
	return frames
}

// SetTemp forces the simulated battery temperature.
func (p *Park) SetTemp(c float64) { p.mu.Lock(); p.TempC = c; p.mu.Unlock() }

// SOCValue reports the simulated SOC.
func (p *Park) SOCValue() float64 { p.mu.Lock(); defer p.mu.Unlock(); return p.SOC }
