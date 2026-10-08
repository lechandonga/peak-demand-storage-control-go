// Command simulate runs the local park end-to-end: normal load, a demand
// surge answered first by storage then by load shedding, a telemetry gap
// exercising conservative mode, valley charging, and a restart replay.
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/lechandonga/peak-demand-storage-control-go/internal/demand"
	"github.com/lechandonga/peak-demand-storage-control-go/sim"
)

func main() {
	statePath := flag.String("state", "/tmp/peak-demand-state.json", "snapshot path")
	flag.Parse()

	base := time.Date(2026, 10, 8, 8, 0, 0, 0, time.FixedZone("CST", 8*3600))
	clock := base
	now := func() time.Time { return clock }

	park := sim.NewPark()
	store := demand.NewStore(demand.DefaultLimits(), now)
	store.SetBatteryReconcile(park.BatterySpec.ID, demand.ReconcileConfig{
		CapacityWh:   park.BatterySpec.CapacityWh,
		ChargeEff:    park.BatterySpec.ChargeEff,
		DischargeEff: park.BatterySpec.DischargeEff,
		SOCNoisePct:  2,
	})
	ctl := demand.NewController(park.Config(base), store, now)

	if _, err := os.Stat(*statePath); err == nil {
		if err := ctl.Load(*statePath); err != nil {
			fmt.Println("load state:", err)
		} else {
			fmt.Println("restarted from", *statePath)
		}
	}

	surgeAt := map[int]float64{}
	for m := 4; m <= 10; m++ {
		surgeAt[m] = 260 // demand surge within the first window
	}
	gapAt := map[int]bool{14: true, 15: true, 16: true} // telemetry lost

	for m := 0; m < 45; m++ {
		clock = base.Add(time.Duration(m) * time.Minute)
		surge := surgeAt[m]

		var frames []demand.Sample
		if !gapAt[m] {
			frames = park.Step(clock, 1, surge)
		} else {
			// No frames at all: telemetry blackout.
			fmt.Printf("[%s] telemetry blackout\n", clock.Format("15:04"))
		}

		res, d := ctl.Ingest(frames)
		for _, rj := range res.Rejected {
			fmt.Printf("[%s] REJECT %s reason=%s %s\n",
				clock.Format("15:04"), rj.Sample.ID, rj.Reason, rj.Detail)
		}
		park.ApplyActions(d.EmittedActions)

		for _, a := range d.EmittedActions {
			fmt.Printf("[%s] EMIT %-16s dev=%-8s kw=%7.1f cause=%s\n",
				clock.Format("15:04"), a.Kind, a.DeviceID, a.KW, a.Cause)
		}
		fmt.Printf("[%s] demand=%6.1f projected=%6.1f peak=%6.1f remain=%5.1fm soc=%5.1f fault=%v\n\n",
			clock.Format("15:04"), d.CurrentDemandKW, d.ProjectedDemandKW,
			d.PeakKW, d.RemainingMinutes, park.SOCValue(), d.Fault.Active)
	}

	if err := ctl.Save(*statePath); err != nil {
		fmt.Println("save state:", err)
	} else {
		fmt.Println("snapshot saved to", *statePath)
	}
	for _, a := range ctl.AuditTrail() {
		fmt.Printf("AUDIT %s %s %s %s kw=%.1f %s\n", a.Ref, a.At.Format("15:04"),
			a.Operator, a.DeviceID, a.KW, a.Reason)
	}
}
