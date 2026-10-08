package storage

import (
	"math"
	"testing"
	"time"
)

func TestLedgerChargeDischargeEfficiency(t *testing.T) {
	cfg := DefaultConfig()
	l := NewLedger(cfg)
	t0 := time.Date(2026, 10, 8, 10, 0, 0, 0, time.UTC)

	if mm := l.ApplySOC("soc0", t0, 0.5); mm != nil {
		t.Fatalf("init soc mismatch: %v", mm)
	}

	// 充电 1 小时：50kW 交流 -> 电池 +50*0.95 kWh。
	t1 := t0.Add(time.Hour)
	if err := l.ApplyPower(PowerSegment{"p1", t1, 50}); err != nil {
		t.Fatal(err)
	}
	snap := l.Snapshot()
	want := 0.5 + 50*cfg.ChargeEff/cfg.CapacityKWh
	if math.Abs(snap.SOC-want) > 1e-9 {
		t.Fatalf("after charge soc = %.6f, want %.6f", snap.SOC, want)
	}

	// 回放同一功率段：不重复累计。
	if err := l.ApplyPower(PowerSegment{"p1", t1, 50}); err != nil {
		t.Fatal(err)
	}
	if l.Snapshot().SOC != want {
		t.Fatalf("replay double-counted energy")
	}

	// 放电 1 小时：交流 50kW -> 电池耗能 50/0.95 kWh。
	t2 := t1.Add(time.Hour)
	if err := l.ApplyPower(PowerSegment{"p2", t2, -50}); err != nil {
		t.Fatal(err)
	}
	snap = l.Snapshot()
	want = want - 50/cfg.DischargeEff/cfg.CapacityKWh
	if math.Abs(snap.SOC-want) > 1e-9 {
		t.Fatalf("after discharge soc = %.6f, want %.6f", snap.SOC, want)
	}

	// 上报 SOC 与模型一致 -> 无告警。
	if mm := l.ApplySOC("soc1", t2, want); mm != nil {
		t.Fatalf("consistent soc flagged: %v", mm)
	}
	// 明显对不上 -> 告警。
	if mm := l.ApplySOC("soc2", t2.Add(time.Minute), 0.95); mm == nil {
		t.Fatalf("inconsistent soc must be rejected")
	} else {
		t.Logf("mismatch detected: %v", mm)
		if math.Abs(mm.Delta-(0.95-want)) > 1e-9 {
			t.Fatalf("mismatch delta = %v", mm.Delta)
		}
	}

	// 乱序功率段必须被拒绝。
	if err := l.ApplyPower(PowerSegment{"p3", t1, -10}); err == nil {
		t.Fatalf("out-of-order power must be rejected")
	}
}

func TestLedgerCapabilitiesAndReserve(t *testing.T) {
	cfg := DefaultConfig()
	l := NewLedger(cfg)
	t0 := time.Date(2026, 10, 8, 10, 0, 0, 0, time.UTC)
	l.ApplySOC("s0", t0, cfg.MinSOC) // 已到备用下限
	l.ApplyTemp(25)

	windowEnd := t0.Add(15 * time.Minute)
	caps := l.CapabilitiesAt(25, true, false, windowEnd, t0)
	if caps.DischargeKW > 0 || caps.DischargeSustainKW > 0 || !caps.AtMinSOC {
		t.Fatalf("at min SOC must not discharge: %+v", caps)
	}
	if caps.ChargeKW != cfg.MaxChargeKW || caps.ChargeEnergyACKWh <= 0 {
		t.Fatalf("charge capability wrong: %+v", caps)
	}

	// 高温降额：50℃ 位于 [45,55] 线性区间，系数 0.5。
	l2 := NewLedger(cfg)
	l2.ApplySOC("s0", t0, 0.8)
	l2.ApplyTemp(50)
	caps = l2.CapabilitiesAt(50, true, false, windowEnd, t0)
	if math.Abs(caps.TempDerate-0.5) > 1e-9 ||
		math.Abs(caps.DischargeKW-cfg.MaxDischargeKW*0.5) > 1e-9 {
		t.Fatalf("temp derate wrong: %+v", caps)
	}

	// 55℃ 截止；温度未知也截止。
	if c := l2.CapabilitiesAt(55, true, false, windowEnd, t0); c.DischargeKW != 0 || c.ChargeKW != 0 {
		t.Fatalf("temp cutoff failed: %+v", c)
	}
	if c := l2.CapabilitiesAt(25, false, false, windowEnd, t0); c.DischargeKW != 0 {
		t.Fatalf("unknown temp must block power: %+v", c)
	}

	// SOC 对不上时能力全部封锁。
	if c := l2.CapabilitiesAt(25, true, true, windowEnd, t0); c.Unreliable != true ||
		c.DischargeKW != 0 || c.ChargeKW != 0 {
		t.Fatalf("unreliable storage must be blocked: %+v", c)
	}

	// 可持续放电：0.8 SOC、下限 0.2、容量 100、效率 0.95 ->
	// 交流电量 60*0.95=57kWh；15min 可持续功率 57/0.25=228kW，被 50kW 功率上限截断。
	caps = l2.CapabilitiesAt(25, true, false, windowEnd, t0)
	if math.Abs(caps.DischargeSustainKW-50) > 1e-9 {
		t.Fatalf("sustain kw = %v", caps.DischargeSustainKW)
	}
}
