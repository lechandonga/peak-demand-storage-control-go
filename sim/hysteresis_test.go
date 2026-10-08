package sim

import (
	"testing"
	"time"

	"github.com/lechandonga/peak-demand-storage-control-go/controller"
	"github.com/lechandonga/peak-demand-storage-control-go/telemetry"
)

// feedAt 直接喂一条进线功率+同刻储能状态样本，并在 ts 时刻 tick。
// 测试中储能功率恒为 0、SOC 与积分一致，保证账本不报错。
func feedAt(t *testing.T, r *Runner, seq int, ts time.Time,
	gridKW float64, soc float64, tempC ...float64) controller.Decision {
	t.Helper()
	mk := func(id string, src telemetry.Source, asset string,
		kind telemetry.Kind, v float64) telemetry.Sample {
		s := telemetry.Sample{ID: id, Source: src, AssetID: asset,
			Kind: kind, TS: ts, Value: v}
		s.Checksum = telemetry.Checksum(s)
		return s
	}
	id := ts.Format("150405")
	temp := 25.0
	if len(tempC) > 0 {
		temp = tempC[0]
	}
	sms := []telemetry.Sample{
		mk("g-"+id, telemetry.SourceGrid, "grid-1", telemetry.KindGridPower, gridKW),
		mk("p-"+id, telemetry.SourceStorage, "bess-1", telemetry.KindStoragePower, 0),
		mk("t-"+id, telemetry.SourceStorage, "bess-1", telemetry.KindStorageTemp, temp),
		mk("s-"+id, telemetry.SourceStorage, "bess-1", telemetry.KindStorageSOC, soc),
	}
	if rj := r.IngestBatch(sms, ts.Add(time.Second)); len(rj) != 0 {
		t.Fatalf("ts %s rejects: %+v", ts.Format("15:04:05"), rj)
	}
	return r.TickOnly(seq, ts).Decision
}

func newManualHarness(t *testing.T) (*Runner, *controller.Controller) {
	cfg := controller.DefaultConfig()
	store := telemetry.NewStore(telemetry.DefaultConfig())
	ctrl, err := controller.New(cfg, store, "")
	if err != nil {
		t.Fatal(err)
	}
	plant := NewPlant(peakScenario(), cfg.GridAssetID, cfg.StorageID, testLoads)
	return NewRunner(plant, store, ctrl), ctrl
}

// 滞回带内（释放线 770 < 780 < 动作线 790）：不放电、不削减、不充电。
func TestHysteresisBandNoAction(t *testing.T) {
	r, _ := newManualHarness(t)
	t0 := testStart()
	for step := 0; step < 6; step++ {
		ts := t0.Add(time.Duration(step) * 30 * time.Second)
		d := feedAt(t, r, step, ts, 780, 0.8)
		t.Logf("band step %d mode=%s cur=%.1f proj=%.1f cmds=%d shed=%v",
			step, d.Mode, d.CurrentDemand, d.ProjectedDemand,
			len(d.Commands), d.ShedLoads)
		for _, c := range d.Commands {
			if c.Target == "bess-1" && c.ValueKW != 0 {
				t.Fatalf("no storage action in band: %.1f (%s)", c.ValueKW, c.Reason)
			}
			if c.Kind == "load_limit_kw" {
				t.Fatalf("no shedding inside band: %+v", c)
			}
		}
		if len(d.ShedLoads) != 0 {
			t.Fatalf("no shedding inside band: %v", d.ShedLoads)
		}
	}
}

// 越过动作线 -> 放电+削减；回到释放线以下需连续 2 tick 才恢复，且顺序不抖动。

// 越过动作线 -> 放电+削减；跨过窗口边界、负荷回落后必须连续 2 tick 才恢复。
func TestActionLineAndStableRelease(t *testing.T) {
	r, _ := newManualHarness(t)
	w0 := testStart().Add(15 * time.Minute)
	step := func(seq, kw int) controller.Decision {
		return feedAt(t, r, 100+seq,
			w0.Add(time.Duration(seq)*30*time.Second), float64(kw), 0.8, 50)
	}

	var d controller.Decision
	for i := 0; i < 8; i++ {
		d = step(i, 760)
	}
	if len(d.ShedLoads) != 0 {
		t.Fatalf("safe baseline must not shed: %v", d.ShedLoads)
	}

	sawDischarge, sawShed := false, false
	shedAt := map[string]int{}
	for i := 8; i < 20; i++ {
		d = step(i, 900)
		t.Logf("risk step %d cur=%.1f proj=%.1f cmds=%d shed=%v",
			i, d.CurrentDemand, d.ProjectedDemand, len(d.Commands), d.ShedLoads)
		for _, c := range d.Commands {
			if c.Kind == "storage_set_kw" && c.ValueKW < 0 {
				sawDischarge = true
			}
			if c.Kind == "load_limit_kw" {
				if _, ok := shedAt[c.Target]; !ok {
					shedAt[c.Target] = i
				}
				sawShed = true
			}
		}
	}
	if !sawDischarge {
		t.Fatal("expected discharge above action line")
	}
	if !sawShed {
		t.Fatal("expected load shedding when storage cannot cover gap")
	}
	if !(shedAt["hvac-1"] < shedAt["evcp-1"] && shedAt["evcp-1"] < shedAt["pump-1"]) {
		t.Fatalf("shed order wrong: %+v", shedAt)
	}

	// 当前窗口高值占比大，预测无法快速回落；跨过窗口边界（30 个 tick=15min）
	// 后新窗口历史清零。持续喂 700kW 直到恢复。
	var restored map[string]bool
	restoreTick := -1
	for i := 20; i < 42; i++ {
		d := step(i, 700)
		t.Logf("recovery step %d proj=%.1f cmds=%d shed=%v",
			i, d.ProjectedDemand, len(d.Commands), d.ShedLoads)
		// 回落第一个 tick 不允许立即恢复（防抖）。
		if i == 20 {
			for _, c := range d.Commands {
				if c.Kind == "load_restore" {
					t.Fatalf("first safe tick must not restore: %+v", c)
				}
			}
		}
		restored = map[string]bool{}
		for _, c := range d.Commands {
			if c.Kind == "load_restore" {
				if c.Reason != "stable_release" {
					t.Fatalf("restore reason = %s", c.Reason)
				}
				restored[c.Target] = true
				restoreTick = i
			}
		}
		if restoreTick > 0 {
			break
		}
	}
	for id := range shedAt {
		if !restored[id] {
			t.Fatalf("load %s not restored after stable conditions", id)
		}
	}
	// 恢复必须跨过至少一个 tick 的稳定计数（不是风险一结束就恢复）。
	if restoreTick < 22 {
		t.Fatalf("restore happened too fast at tick %d, stable hold missing", restoreTick)
	}
}
func TestChargeThreshold(t *testing.T) {
	r, _ := newManualHarness(t)
	t0 := testStart()

	// 700kW：余量 100 < ChargeHeadroom(120)，禁止充电。
	d := feedAt(t, r, 1, t0.Add(30*time.Second), 700, 0.3)
	for _, c := range d.Commands {
		if c.Kind == "storage_set_kw" && c.ValueKW > 0 {
			t.Fatalf("charging above charge threshold forbidden: %.1f (%s)",
				c.ValueKW, c.Reason)
		}
	}

	// 650kW：余量 150 >= 120，允许充电。
	d2 := feedAt(t, r, 2, t0.Add(60*time.Second), 650, 0.3)
	sawCharge := false
	for _, c := range d2.Commands {
		if c.Kind == "storage_set_kw" && c.ValueKW > 0 {
			sawCharge = true
		}
	}
	if !sawCharge {
		t.Fatalf("650kW with 150kW headroom must permit charging: %+v", d2.Commands)
	}
}
