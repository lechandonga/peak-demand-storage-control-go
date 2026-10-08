package sim

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/lechandonga/peak-demand-storage-control-go/controller"
	"github.com/lechandonga/peak-demand-storage-control-go/telemetry"
)

// 场景 1：高峰越限 -> 先储能放电，不足再按优先级削减负载；
// 回落后必须满足滞回稳定条件才恢复。
func TestPeakDischargeThenShedAndHysteresisRestore(t *testing.T) {
	sc := peakScenario()
	r, _, ctrl := newHarness(t, sc, "")
	res := runAll(t, r, sc.Steps, 7)

	sawDischarge, sawShed := false, false
	shedAt := map[string]int{}
	for _, x := range res {
		for _, cmd := range x.Decision.Commands {
			if cmd.Kind == "storage_set_kw" && cmd.ValueKW < 0 {
				sawDischarge = true
			}
			if cmd.Kind == "load_limit_kw" {
				if _, ok := shedAt[cmd.Target]; !ok {
					shedAt[cmd.Target] = x.Step
				}
				sawShed = true
			}
		}
	}
	if !sawDischarge {
		t.Fatal("expected storage discharge during peak")
	}
	if !sawShed {
		t.Fatal("expected load shedding after storage insufficient")
	}
	if !(shedAt["hvac-1"] < shedAt["evcp-1"] && shedAt["evcp-1"] < shedAt["pump-1"]) {
		t.Fatalf("shed order not priority-based: %+v", shedAt)
	}

	restoreAt := map[string]int{}
	for _, x := range res {
		for _, cmd := range x.Decision.Commands {
			if cmd.Kind == "load_restore" {
				if cmd.Reason != "stable_release" {
					t.Fatalf("restore reason = %s", cmd.Reason)
				}
				restoreAt[cmd.Target] = x.Step
			}
		}
	}
	if len(restoreAt) == 0 {
		t.Fatal("loads never restored after peak subsided")
	}
	for id, at := range restoreAt {
		if at-shedAt[id] < 2 {
			t.Fatalf("load %s restored without stable hold (shed=%d restore=%d)",
				id, shedAt[id], at)
		}
	}

	hist := ctrl.PeakBook().History()
	if len(hist) != 1 || hist[0].Record.DemandKW < 800 {
		t.Fatalf("peak history wrong: %+v", hist)
	}
}

// 场景 2：乱序批量 vs 并发单条入口，最终指令序列必须唯一。
func TestDeterministicAcrossEntryPoints(t *testing.T) {
	run := func(concurrent bool) []string {
		sc := peakScenario()
		r, _, _ := newHarness(t, sc, "")
		var sig []string
		for i := 0; i < sc.Steps; i++ {
			ts := r.PlantTime(i)
			samples := r.PlantSamples(i)
			if !concurrent {
				rng := rand7()
				rng.Shuffle(len(samples), func(a, b int) {
					samples[a], samples[b] = samples[b], samples[a]
				})
				if rs := r.IngestBatch(samples, ts.Add(time.Second)); len(rs) != 0 {
					t.Fatalf("unexpected rejects in batch mode: %d", len(rs))
				}
			} else {
				var wg sync.WaitGroup
				var mu sync.Mutex
				var bad []telemetry.Reject
				for _, sm := range samples {
					wg.Add(1)
					go func(sm telemetry.Sample) {
						defer wg.Done()
						if rj := r.Ingest(sm, ts.Add(time.Second)); rj != nil {
							mu.Lock()
							bad = append(bad, *rj)
							mu.Unlock()
						}
					}(sm)
				}
				wg.Wait()
				if len(bad) != 0 {
					t.Fatalf("unexpected rejects in concurrent mode: %+v", bad)
				}
			}
			d := r.TickOnly(i, ts).Decision
			for _, c := range d.Commands {
				sig = append(sig, fmt.Sprintf("%d|%s|%s|%.1f", i, c.Target, c.Kind, c.ValueKW))
			}
		}
		return sig
	}

	seq := run(false)
	con := run(true)
	if len(seq) == 0 {
		t.Fatal("no commands emitted")
	}
	if len(seq) != len(con) {
		t.Fatalf("command count diverges: %d vs %d", len(seq), len(con))
	}
	for i := range seq {
		if seq[i] != con[i] {
			t.Fatalf("divergence at %d:\n batch=%s\n concurrent=%s", i, seq[i], con[i])
		}
	}
	t.Logf("deterministic command sequence length = %d", len(seq))
}

// 场景 3：进线失联 -> 保守模式削减全部可调负载；恢复后平稳回正常。
func TestBlackoutConservativeAndRecovery(t *testing.T) {
	sc := peakScenario()
	sc.BlackoutFrom = 20
	sc.BlackoutFor = 6
	r, _, ctrl := newHarness(t, sc, "")
	res := runAll(t, r, sc.Steps, 0)

	conservative := 0
	for _, x := range res {
		if x.Decision.Mode == controller.ModeConservative {
			conservative++
			if len(x.Decision.ShedLoads) != 3 {
				t.Fatalf("conservative step %d must shed all adjustable loads, got %v",
					x.Step, x.Decision.ShedLoads)
			}
		}
	}
	if conservative < 3 {
		t.Fatalf("expected >=3 conservative steps during blackout, got %d", conservative)
	}

	returned := false
	for _, x := range res[27:] {
		if x.Decision.Mode == controller.ModeNormal {
			returned = true
		}
	}
	if !returned {
		t.Fatal("controller did not return to normal after blackout")
	}

	found := false
	for _, e := range ctrl.Journal().Entries() {
		if (e.Category == "command" && e.Reason == "conservative_shed") ||
			(e.Category == "fault" && e.Reason == "conservative_shed_held") {
			found = true
		}
	}
	if !found {
		t.Fatal("no conservative_shed audit record")
	}
}

// 场景 4：时间跳变 -> 保守。
func TestClockJumpConservative(t *testing.T) {
	sc := peakScenario()
	sc.ClockJumpAt = 10
	sc.ClockJumpBy = -30 * time.Minute
	r, _, _ := newHarness(t, sc, "")
	res := runAll(t, r, sc.Steps, 0)
	if !res[10].Decision.ClockJump || res[10].Decision.Mode != controller.ModeConservative {
		t.Fatalf("clock jump must trigger conservative: mode=%s jump=%v",
			res[10].Decision.Mode, res[10].Decision.ClockJump)
	}
}

// 场景 5：上报 SOC 与能量积分对不上 -> 不信储能、保守、审计，且不发非空闲指令。
func TestSOCMismatchConservative(t *testing.T) {
	sc := peakScenario()
	sc.BadSOCAt = 8
	sc.BadSOCValue = 0.2
	r, _, ctrl := newHarness(t, sc, "")
	res := runAll(t, r, sc.Steps, 0)

	if ctrl.Ledger().Mismatch() == nil {
		t.Fatal("SOC mismatch not detected")
	}
	if res[8].Decision.Mode != controller.ModeConservative {
		t.Fatalf("SOC mismatch must enter conservative, got %s", res[8].Decision.Mode)
	}
	socAudit := false
	for _, e := range ctrl.Journal().Entries() {
		if e.Category == "soc" && e.Reason == "soc_energy_mismatch" {
			socAudit = true
		}
	}
	if !socAudit {
		t.Fatal("soc mismatch not audited")
	}
	for _, x := range res[8:] {
		if x.Decision.Mode != controller.ModeConservative {
			continue
		}
		for _, cmd := range x.Decision.Commands {
			if cmd.Target == "bess-1" && cmd.ValueKW != 0 {
				t.Fatalf("unreliable storage issued non-idle command %.1f at step %d",
					cmd.ValueKW, x.Step)
			}
		}
	}
}

// 场景 6：高温降额限制储能，缺口由负载削减兜底。
func TestTemperatureDerating(t *testing.T) {
	sc := peakScenario()
	sc.TempAt = func(step int) float64 {
		if step >= 4 {
			return 50
		}
		return 25
	}
	r, _, _ := newHarness(t, sc, "")
	res := runAll(t, r, sc.Steps, 0)

	derated := false
	for _, x := range res {
		if x.Decision.StorageCaps.TempDerate == 0.5 {
			derated = true
			if x.Decision.StorageCaps.DischargeKW > 25+0.01 {
				t.Fatalf("discharge cap not derated: %v",
					x.Decision.StorageCaps.DischargeKW)
			}
		}
	}
	if !derated {
		t.Fatal("never observed derated capabilities")
	}
	if len(res[10].Decision.ShedLoads) == 0 {
		t.Fatal("expected load shedding to cover derated storage")
	}
}
