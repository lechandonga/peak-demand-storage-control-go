package controller

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/lechandonga/peak-demand-storage-control-go/demand"
)

const kwRounding = 0.1 // 指令功率量化粒度，抑制浮点抖动

func roundKV(v float64) float64 {
	return math.Round(v/kwRounding) * kwRounding
}

// Tick 基于 now 时刻已接受的全部遥测做一次确定性决策。
// 同一输入集合、同一 now，无论调用入口或并发方式，结论唯一
// （store/ledger/journal 均串行化；相同指令不重复下发）。
func (c *Controller) Tick(now time.Time) Decision {
	d := Decision{TS: now, LoadLimits: make(map[string]float64)}

	anchor := demand.DayAnchor(now, c.cfg.Location)
	w := demand.CurrentWindow(now, anchor, c.cfg.WindowLength)
	d.Window = w

	// 1) 时钟大幅回退 -> 保守。
	if !c.lastTick.IsZero() && c.lastTick.Sub(now) > c.cfg.ClockJumpTolerance {
		d.ClockJump = true
		d.Reasons = append(d.Reasons, fmt.Sprintf(
			"clock jumped backwards: last tick %s, now %s",
			c.lastTick.Format(time.RFC3339), now.Format(time.RFC3339)))
	}

	// 2) 结算已跨过的窗口（一次 tick 可能跨多个边界，逐窗结算）。
	c.closeExpiredWindows(&d, w)

	// 3) 新到储能遥测按时间顺序（SOC→温度→功率）喂入能量账本。
	c.feedLedger(now)

	// 4) 数据新鲜度。
	c.checkFreshness(now, &d)

	// 5) 需量计算与窗口末预测。
	gridPts := c.gridPoints(w.Start, now)
	if cur, ok := demand.CurrentDemand(gridPts, w, now); ok {
		d.CurrentDemand, d.DemandReliable = cur, true
	}
	if proj, ok := demand.ProjectEnd(gridPts, w, now); ok {
		d.ProjectedDemand, d.DemandReliable = proj, true
	}

	// 6) 储能能力。
	snap := c.ledger.Snapshot()
	d.SOC = snap.SOC
	unreliable := snap.Mismatch != nil
	caps := c.ledger.CapabilitiesAt(snap.TempC, snap.TempValid, unreliable, w.End, now)
	d.StorageCaps = caps

	storageManual, loadManual := c.hasManual()

	// 7) 模式判定：任何不可观测/不一致都优先保守。
	// 冷启动（此前从未 tick 且尚无任何历史）不算故障，按无风险空窗处理。
	fault := d.ClockJump || d.GridStale || unreliable ||
		(!d.DemandReliable && c.primed)
	switch {
	case fault:
		d.Mode = ModeConservative
	case storageManual || loadManual:
		d.Mode = ModeManual
	default:
		d.Mode = ModeNormal
	}

	// 8) 分模式决策。
	switch d.Mode {
	case ModeConservative:
		c.conservative(&d, now)
	case ModeManual:
		c.manualControl(&d, caps, w, now)
	default:
		c.normal(&d, caps, w, now)
	}
	// 重放历史期间：动作只用于重建内存状态，不作为新指令输出。
	if c.replay {
		d.Commands = nil
	}

	// 9) 汇总负载状态。
	for _, id := range c.loadOrder {
		ls := c.loads[id]
		d.LoadLimits[id] = ls.LimitKW
		if ls.Shed {
			d.ShedLoads = append(d.ShedLoads, id)
		}
	}
	sort.Strings(d.ShedLoads)

	c.lastTick = now
	c.primed = true
	return d
}
