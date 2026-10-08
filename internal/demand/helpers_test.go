package demand

import (
	"fmt"
	"testing"
	"time"
)

type fixture struct {
	t     *testing.T
	base  time.Time
	clock time.Time
	store *Store
	cfg   Config
	ctl   *Controller
}

func testConfig() Config {
	return Config{
		WindowWidth:       15 * time.Minute,
		TickInterval:      time.Minute,
		ContractKW:        500,
		RiskLeadKW:        10,
		RestoreLagKW:      20,
		ValleyThresholdKW: 300,
		ChargeHeadroomKW:  30,
		MinStableTicks:    3,
		StaleAfter:        2 * time.Minute,
		MaxTimeJump:       5 * time.Minute,
		Batteries: []BatterySpec{{
			ID: "B", CapacityWh: 1_000_000,
			MaxDischargeKW: 250, MaxChargeKW: 200,
			ChargeEff: 0.95, DischargeEff: 0.95, BackupSOC: 20,
			TempHot: 45, TempHotCut: 55, TempCold: 0, TempColdCut: -10,
		}},
		Loads: []LoadSpec{
			{ID: "L1", RatedKW: 100, MinKW: 20, Priority: 1},
			{ID: "L2", RatedKW: 100, MinKW: 20, Priority: 2},
			{ID: "L3", RatedKW: 100, MinKW: 0, Priority: 3},
		},
	}
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	base := time.Date(2026, 10, 8, 8, 0, 0, 0, time.UTC)
	f := &fixture{t: t, base: base, clock: base}
	now := func() time.Time { return f.clock }
	f.store = NewStore(DefaultLimits(), now)
	f.cfg = testConfig()
	f.store.SetBatteryReconcile("B", ReconcileConfig{
		// Behavioral tests prescribe SOC directly (including reserve/thermal
		// scenarios); reconciliation is covered with a tight model in the
		// dedicated telemetry test.
		CapacityWh: 1_000_000, ChargeEff: .95, DischargeEff: .95, SOCNoisePct: 60,
	})
	f.ctl = NewController(f.cfg, f.store, now)
	return f
}

func (f *fixture) at(min int) time.Time {
	return f.base.Add(time.Duration(min) * time.Minute)
}

// ingestAt moves the fixture clock to the sample's minute then ingests,
// matching how production frames always arrive at/near their timestamp.
func (f *fixture) ingestAt(samples ...Sample) IngestResult {
	var latest time.Time
	for _, s := range samples {
		if s.At.After(latest) {
			latest = s.At
		}
	}
	f.clock = latest
	res := f.store.Ingest(samples)
	for _, s := range samples {
		f.t.Logf("INPUT  %-7s id=%s power=%.1f soc=%.1f temp=%.1f",
			s.Source, s.ID, s.PowerKW, s.SOC, s.TempC)
	}
	for _, rj := range res.Rejected {
		f.t.Logf("REJECT id=%s reason=%s detail=%s", rj.Sample.ID, rj.Reason, rj.Detail)
	}
	return res
}

func (f *fixture) tick(min int) *Decision {
	f.clock = f.at(min)
	d := f.ctl.Decide()
	f.logDecision(d)
	return d
}

func (f *fixture) feed(min int, samples ...Sample) IngestResult {
	f.clock = f.at(min)
	res := f.store.Ingest(samples)
	for _, s := range samples {
		f.t.Logf("INPUT  %-7s dev=%s at=m%d power=%.1f soc=%.1f temp=%.1f",
			s.Source, s.DeviceID, min, s.PowerKW, s.SOC, s.TempC)
	}
	for _, rj := range res.Rejected {
		f.t.Logf("REJECT id=%s reason=%s detail=%s", rj.Sample.ID, rj.Reason, rj.Detail)
	}
	return res
}

// step feeds a complete one-minute frame (grid, battery, three loads) and
// runs one decision.
func (f *fixture) step(min, gridKW, battKW, soc, temp, energy float64, loads [3]float64) *Decision {
	m := int(min)
	s := []Sample{
		gridSample(fmt.Sprintf("g%d", m), f.at(m), gridKW),
		battSample(fmt.Sprintf("b%d", m), f.at(m), battKW, soc, temp, energy),
	}
	for i, kw := range loads {
		s = append(s, loadSample(fmt.Sprintf("l%d-%d", i+1, m), f.at(m),
			fmt.Sprintf("L%d", i+1), kw))
	}
	f.feed(m, s...)
	return f.tick(m)
}

func (f *fixture) logDecision(d *Decision) {
	f.t.Logf("DECIDE at=%s demand=%.1f projected=%.1f peak=%.1f remain=%.0f fault=%v %s",
		d.At.Format("15:04"), d.CurrentDemandKW, d.ProjectedDemandKW,
		d.PeakKW, d.RemainingMinutes, d.Fault.Active, d.Fault.Detail)
	for _, a := range d.EmittedActions {
		f.t.Logf("  EMIT %-17s dev=%s kw=%.1f cause=%s", a.Kind, a.DeviceID, a.KW, a.Cause)
	}
}

func gridSample(id string, at time.Time, kw float64) Sample {
	return Sample{ID: id, Source: SourceGrid, DeviceID: "G", At: at, PowerKW: kw}
}

func battSample(id string, at time.Time, kw, soc, temp, energy float64) Sample {
	return Sample{ID: id, Source: SourceBattery, DeviceID: "B", At: at,
		PowerKW: kw, SOC: soc, TempC: temp, EnergyWh: energy}
}

func loadSample(id string, at time.Time, dev string, kw float64) Sample {
	return Sample{ID: id, Source: SourceLoad, DeviceID: dev, At: at, PowerKW: kw}
}

func emittedFor(d *Decision, dev string) (Action, bool) {
	for _, a := range d.EmittedActions {
		if a.DeviceID == dev {
			return a, true
		}
	}
	return Action{}, false
}

func nanF() float64 {
	var z float64
	return z / z
}
