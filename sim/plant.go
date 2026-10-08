// Package sim 用本地内存数据模拟园区进线、储能与可调负载，
// 不依赖任何外部服务。
package sim

import (
	"fmt"
	"time"

	"github.com/lechandonga/peak-demand-storage-control-go/telemetry"
)

// Scenario 描述一段园区负荷曲线。
type Scenario struct {
	Start   time.Time
	Step    time.Duration // 遥测上报周期（如 30s）
	Steps   int
	BaseKW  float64            // 基础负荷
	PeakKW  float64            // 高峰时段负荷（线性抬升后保持）
	PeakAt  int                // 进入高峰的步号
	PeakFor int                // 高峰持续步数
	LoadKW  map[string]float64 // 各可调负载额定功率

	InitialSOC float64

	// 注入故障。
	BlackoutFrom int // 从第几步起进线遥测中断（-1 关闭）
	BlackoutFor  int
	ClockJumpAt  int // 时间跳变注入步（-1 关闭）
	ClockJumpBy  time.Duration

	// 储能温度（℃）随步变化；nil 时恒 25℃。
	TempAt func(step int) float64

	// 注入一条与能量账本对不上的 SOC（步号，-1 关闭）。
	BadSOCAt    int
	BadSOCValue float64
}

// Plant 是对物理园区的简化仿真：
// 进线功率 = 基础负荷 + 各负载实际功率 + 储能交流功率（正充负放）。
type Plant struct {
	sc Scenario

	gridID, bessID string
	loadIDs        []string

	storageKW float64 // 当前储能执行功率（由外部指令设置）
	loadLimit map[string]float64
	histKW    []float64 // 每步执行的储能功率（与账本同一效率模型）
}

// NewPlant 创建仿真对象。
func NewPlant(sc Scenario, gridID, bessID string, loadIDs []string) *Plant {
	if sc.Step == 0 {
		sc.Step = 30 * time.Second
	}
	if sc.TempAt == nil {
		sc.TempAt = func(int) float64 { return 25 }
	}
	// 零值场景（未注入坏 SOC）默认关闭，避免误命中第 0 步。
	if sc.BadSOCAt == 0 && sc.BadSOCValue == 0 {
		sc.BadSOCAt = -1
	}
	lim := make(map[string]float64)
	for _, id := range loadIDs {
		lim[id] = sc.LoadKW[id]
	}
	return &Plant{sc: sc, gridID: gridID, bessID: bessID,
		loadIDs: loadIDs, loadLimit: lim}
}

// TimeAt 返回第 step 步（0-based）的时间戳。
func (p *Plant) TimeAt(step int) time.Time {
	t := p.sc.Start.Add(time.Duration(step) * p.sc.Step)
	if p.sc.ClockJumpAt >= 0 && step >= p.sc.ClockJumpAt {
		t = t.Add(p.sc.ClockJumpBy)
	}
	return t
}

// SetStoragePower 设置储能执行功率（正充负放）。
func (p *Plant) SetStoragePower(kw float64) { p.storageKW = kw }

// RecordStep 在第 step 步决策执行完后调用，固化该步的储能功率。
func (p *Plant) RecordStep(step int, kw float64) {
	p.storageKW = kw
	for len(p.histKW) <= step {
		p.histKW = append(p.histKW, 0)
	}
	p.histKW[step] = kw
}

// SetLoadLimit 设置某负载的功率上限（物理执行）。
func (p *Plant) SetLoadLimit(id string, kw float64) { p.loadLimit[id] = kw }

// RestoreLoad 恢复负载满投。
func (p *Plant) RestoreLoad(id string, rated float64) { p.loadLimit[id] = rated }

// SamplesAt 生成第 step 步的全部遥测样本（带确定性 ID 与校验和）。
func (p *Plant) SamplesAt(step int) []telemetry.Sample {
	ts := p.TimeAt(step)
	blackout := p.sc.BlackoutFrom >= 0 &&
		step >= p.sc.BlackoutFrom && step < p.sc.BlackoutFrom+p.sc.BlackoutFor

	demand := p.baseLoadAt(step)
	loadPower := 0.0
	var out []telemetry.Sample

	mk := func(source telemetry.Source, asset string, kind telemetry.Kind,
		value float64, suffix string) telemetry.Sample {
		s := telemetry.Sample{
			ID:     fmt.Sprintf("s%d|%s|%s|%s", step, source, asset, suffix),
			Source: source, AssetID: asset, Kind: kind,
			TS: ts, Value: value,
		}
		s.Checksum = telemetry.Checksum(s)
		return s
	}

	for _, id := range p.loadIDs {
		kw := p.loadLimit[id]
		loadPower += kw
		out = append(out, mk(telemetry.SourceLoad, id, telemetry.KindLoadPower, kw, "p"))
		out = append(out, mk(telemetry.SourceLoad, id, telemetry.KindLoadSettableKW, kw, "lim"))
	}

	grid := demand + loadPower + p.storageKW
	if !blackout {
		out = append(out, mk(telemetry.SourceGrid, p.gridID,
			telemetry.KindGridPower, grid, "p"))
	}

	soc := p.SOCAt(step)
	if p.sc.BadSOCAt == step {
		soc = p.sc.BadSOCValue
	}
	out = append(out,
		mk(telemetry.SourceStorage, p.bessID, telemetry.KindStoragePower, p.storageKW, "p"),
		mk(telemetry.SourceStorage, p.bessID, telemetry.KindStorageSOC, soc, "soc"),
		mk(telemetry.SourceStorage, p.bessID, telemetry.KindStorageTemp,
			p.sc.TempAt(step), "t"),
	)
	return out
}

// baseLoadAt 返回不可调负荷曲线：高峰段线性爬到 PeakKW 并保持。
func (p *Plant) baseLoadAt(step int) float64 {
	if step < p.sc.PeakAt || step >= p.sc.PeakAt+p.sc.PeakFor {
		return p.sc.BaseKW
	}
	ramp := 4
	off := step - p.sc.PeakAt
	if off < ramp {
		return p.sc.BaseKW + (p.sc.PeakKW-p.sc.BaseKW)*float64(off+1)/float64(ramp)
	}
	return p.sc.PeakKW
}

// SOCAt 用与 storage.Ledger 相同的效率模型正向仿真真实 SOC，
// 因此正常场景下上报 SOC 与控制器账本必然对得上。
func (p *Plant) SOCAt(step int) float64 {
	const (
		chargeEff    = 0.95
		dischargeEff = 0.95
		capacityKWh  = 100
	)
	soc := p.sc.InitialSOC
	hours := p.sc.Step.Hours()
	for i := 0; i < step && i < 100000; i++ {
		kw := p.storageKWAt(i)
		if kw >= 0 {
			soc += kw * hours * chargeEff / capacityKWh
		} else {
			soc += kw * hours / dischargeEff / capacityKWh
		}
		if soc < 0 {
			soc = 0
		}
		if soc > 1 {
			soc = 1
		}
	}
	return soc
}

// storageKWAt 返回历史第 i 步期间执行的储能功率。
// 简化模型：外部每步通过 SetStoragePower 设定，记录历史由 controller 驱动；
// 仿真器本身只保持当前值，历史 SOC 场景测试中由 recorder 提供。
func (p *Plant) storageKWAt(i int) float64 {
	if i >= 0 && i < len(p.histKW) {
		return p.histKW[i]
	}
	return 0
}
