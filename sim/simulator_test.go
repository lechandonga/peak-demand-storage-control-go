package sim

import (
	"testing"
	"time"

	"github.com/lechandonga/peak-demand-storage-control-go/internal/demand"
)

// TestLocalParkEndToEnd drives the local simulator through surge, storage
// support, a telemetry blackout and recovery, asserting distinguishable
// action causes end-to-end without any external service.
func TestLocalParkEndToEnd(t *testing.T) {
	base := time.Date(2026, 10, 8, 8, 0, 0, 0, time.UTC)
	clock := base
	now := func() time.Time { return clock }

	park := NewPark()
	store := demand.NewStore(demand.DefaultLimits(), now)
	store.SetBatteryReconcile(park.BatterySpec.ID, demand.ReconcileConfig{
		CapacityWh:   park.BatterySpec.CapacityWh,
		ChargeEff:    park.BatterySpec.ChargeEff,
		DischargeEff: park.BatterySpec.DischargeEff,
		SOCNoisePct:  2,
	})
	ctl := demand.NewController(park.Config(base), store, now)

	sawDischarge, sawShed, sawConserve, sawRecover := false, false, false, false
	for m := 0; m < 30; m++ {
		clock = base.Add(time.Duration(m) * time.Minute)

		var frames []demand.Sample
		switch {
		case m >= 4 && m <= 8:
			frames = park.Step(clock, 1, 260) // surge
		case m >= 14 && m <= 16:
			// telemetry blackout: no frames, but still decide
		default:
			frames = park.Step(clock, 1, 0)
		}
		res := store.Ingest(frames)
		if len(res.Rejected) != 0 {
			t.Fatalf("unexpected rejection at m%d: %+v", m, res.Rejected)
		}
		d := ctl.Decide()
		park.ApplyActions(d.EmittedActions)

		for _, a := range d.EmittedActions {
			t.Logf("m%d %s %s %.1f %s", m, a.Kind, a.DeviceID, a.KW, a.Cause)
			switch {
			case a.Kind == demand.ActBatteryDischarge &&
				(a.Cause == demand.CauseCapAtRisk || a.Cause == demand.CausePeakAtRisk):
				sawDischarge = true
			case a.Kind == demand.ActLoadShed && a.Cause == demand.CauseStorageLimited:
				sawShed = true
			case a.Cause == demand.CauseFaultConserve:
				sawConserve = true
			case a.Cause == demand.CauseStableRecovery:
				sawRecover = true
			}
		}
	}
	if !sawDischarge {
		t.Error("expected storage discharge at risk")
	}
	if !sawShed {
		t.Error("expected priority load shedding after storage is insufficient")
	}
	if !sawConserve {
		t.Error("expected conservative shedding during telemetry blackout")
	}
	if !sawRecover {
		t.Error("expected stable recovery after telemetry returns")
	}
}
