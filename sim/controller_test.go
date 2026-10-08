package sim

import (
	"fmt"
	"math/rand"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lechandonga/peak-demand-storage-control-go/controller"
	"github.com/lechandonga/peak-demand-storage-control-go/telemetry"
)

var testLoads = []string{"hvac-1", "evcp-1", "pump-1"}

func testStart() time.Time {
	loc, _ := time.LoadLocation("Asia/Shanghai")
	return time.Date(2026, 9, 30, 13, 45, 0, 0, loc)
}

func peakScenario() Scenario {
	return Scenario{
		Start: testStart(), Step: 30 * time.Second, Steps: 40,
		BaseKW: 560, PeakKW: 900, PeakAt: 2, PeakFor: 30,
		LoadKW: map[string]float64{
			"hvac-1": 120, "evcp-1": 100, "pump-1": 80,
		},
		InitialSOC: 0.8,
	}
}

func newHarness(t *testing.T, sc Scenario, journalPath string) (*Runner, *telemetry.Store, *controller.Controller) {
	t.Helper()
	cfg := controller.DefaultConfig()
	store := telemetry.NewStore(telemetry.DefaultConfig())
	ctrl, err := controller.New(cfg, store, journalPath)
	if err != nil {
		t.Fatal(err)
	}
	plant := NewPlant(sc, cfg.GridAssetID, cfg.StorageID, testLoads)
	return NewRunner(plant, store, ctrl), store, ctrl
}

func runAll(t *testing.T, r *Runner, steps int, shuffleSeed int64) []StepResult {
	t.Helper()
	var rng *rand.Rand
	if shuffleSeed > 0 {
		rng = rand.New(rand.NewSource(shuffleSeed))
	}
	out := make([]StepResult, 0, steps)
	for i := 0; i < steps; i++ {
		var sh func([]telemetry.Sample)
		if rng != nil {
			sh = func(s []telemetry.Sample) {
				rng.Shuffle(len(s), func(a, b int) { s[a], s[b] = s[b], s[a] })
			}
		}
		res := r.RunStep(i, sh)
		out = append(out, res)
		logStep(t, res)
	}
	return out
}

func logStep(t *testing.T, res StepResult) {
	t.Helper()
	d := res.Decision
	t.Logf("[step %02d %s] mode=%-12s cur=%6.1f proj=%6.1f soc=%.3f bess=%s%.0f shed=%v cmds=%d rejects=%d",
		res.Step, d.TS.Format("15:04:05"), d.Mode, d.CurrentDemand, d.ProjectedDemand,
		d.SOC, d.StorageMode, d.StorageKW, d.ShedLoads, len(d.Commands), len(res.Rejects))
	for _, rj := range res.Rejects {
		t.Logf("    reject: id=%s reason=%s (%s)", rj.Sample.ID, rj.Reason, rj.Detail)
	}
	for _, cmd := range d.Commands {
		t.Logf("    command: %s target=%s %.1f kW reason=%s",
			cmd.Kind, cmd.Target, cmd.ValueKW, cmd.Reason)
	}
	for _, why := range d.Reasons {
		if strings.Contains(why, "gap") || strings.Contains(why, "manual") ||
			strings.Contains(why, "stale") || strings.Contains(why, "inconsistent") ||
			strings.Contains(why, "kept shed") {
			t.Logf("    note: %s", why)
		}
	}
}

func commandSig(res []StepResult) []string {
	var sig []string
	for _, x := range res {
		for _, c := range x.Decision.Commands {
			sig = append(sig, fmt.Sprintf("%d|%s|%s|%.1f", x.Step, c.Target, c.Kind, c.ValueKW))
		}
	}
	return sig
}

func tmpJournal(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	return filepath.Join(dir, "audit.jsonl")
}
