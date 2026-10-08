package controller

import (
	"fmt"
	"math"
	"time"

	"github.com/lechandonga/peak-demand-storage-control-go/demand"
	"github.com/lechandonga/peak-demand-storage-control-go/storage"
)

func describeStorageLimit(caps storage.Capabilities) string {
	switch {
	case caps.Unreliable:
		return "storage unavailable: SOC inconsistent with energy ledger"
	case caps.AtMinSOC:
		return "storage at reserve min SOC, no discharge"
	case caps.TempDerate <= 0:
		return "storage power blocked by temperature cutoff or unknown temperature"
	case caps.DischargeSustainKW < 0.5:
		return "storage discharge energy insufficient to sustain window"
	default:
		return "storage discharge not needed"
	}
}

func (c *Controller) normal(d *Decision, caps storage.Capabilities,
	w demand.Window, now time.Time) {

	// 窗口内尚无进线数据：不预测风险，但也绝不主动充电/放电，等待观测。
	if !d.DemandReliable {
		c.emitStorage(d, 0, "awaiting_grid_telemetry", w, now)
		d.Reasons = append(d.Reasons, "no in-window grid data yet, hold idle")
		return
	}

	lastKW := d.CurrentDemand
	if lp, ok := demand.LastAtOrBefore(c.gridPoints(w.Start, now), now); ok {
		lastKW = lp.Value
	}

	actionLine := c.cfg.DemandLimitKW - c.cfg.StartMarginKW
	releaseLine := c.cfg.DemandLimitKW - c.cfg.ReleaseMarginKW
	projected := d.ProjectedDemand
	atRisk := projected >= actionLine
	safe := projected <= releaseLine

	for _, ls := range c.loads {
		if ls.Manual {
			continue
		}
		if safe {
			ls.ReleaseOK++
		} else {
			ls.ReleaseOK = 0
		}
	}

	if atRisk {
		d.Reasons = append(d.Reasons, fmt.Sprintf(
			"projected demand %.1f kW >= action line %.1f kW", projected, actionLine))

		need := requiredReduction(d.CurrentDemand, lastKW, c.cfg.DemandLimitKW, w, now)

		// 先储能：瞬时能力与"撑到窗口结束"可持续值取小。
		discharge := 0.0
		if !caps.Unreliable {
			discharge = math.Min(math.Min(caps.DischargeKW, caps.DischargeSustainKW), need)
		} else {
			d.Reasons = append(d.Reasons, "storage unreliable, skip discharge")
		}
		if discharge > 0.5 {
			c.emitStorage(d, -roundKV(discharge), "peak_risk_discharge", w, now)
		} else if !caps.Unreliable {
			d.Reasons = append(d.Reasons, describeStorageLimit(caps))
		}

		// 剩余缺口按优先级削减可调负载。
		if residual := need - discharge; residual > 0.5 {
			c.shedLoads(d, residual, w, now)
		}
		for _, ls := range c.loads {
			ls.ReleaseOK = 0
		}
		return
	}

	// 安全区：停放电，稳定条件满足后恢复负载。
	if safe {
		c.emitStorage(d, 0, "demand_safe_idle", w, now)
		for _, id := range c.loadOrder {
			ls := c.loads[id]
			if ls.Manual || !ls.Shed {
				continue
			}
			if ls.ReleaseOK >= c.cfg.HoldTicks {
				c.restoreLoad(d, ls, "stable_release", w, now)
			} else {
				d.Reasons = append(d.Reasons, fmt.Sprintf(
					"load %s kept shed, release hold %d/%d",
					id, ls.ReleaseOK, c.cfg.HoldTicks))
			}
		}

		// 低谷且余量充足才充电；充电后预测需量仍要留足裕量。
		headroom := c.cfg.DemandLimitKW - projected
		if headroom >= c.cfg.ChargeHeadroomKW && !caps.Unreliable &&
			caps.ChargeKW > 0.5 && caps.ChargeEnergyACKWh > 0.5 {
			charge := math.Min(caps.ChargeKW,
				math.Min(c.cfg.ChargeMaxKW, headroom-c.cfg.ChargeHeadroomKW))
			charge = math.Min(charge,
				caps.ChargeEnergyACKWh/math.Max(w.Remaining(now).Hours(), 1e-6))
			if charge > 0.5 {
				c.emitStorage(d, roundKV(charge), "valley_charge", w, now)
			}
		}
	}
}

// conservative 保守策略：不依赖储能，把可调负载压到保底功率，
// 并保持到数据恢复且风险解除。恢复仍需走稳定计数，避免状态跳变。
func (c *Controller) conservative(d *Decision, now time.Time) {
	// 储能置空闲（不充不放），避免在不可观测时动作。
	c.emitStorage(d, 0, "conservative_idle", d.Window, now)

	for _, id := range c.loadOrder {
		ls := c.loads[id]
		if ls.Manual {
			// 人工强制仍被尊重，但记录保守模式下的并存事实。
			d.Reasons = append(d.Reasons,
				fmt.Sprintf("manual override on %s respected while conservative", id))
			continue
		}
		if !ls.Shed {
			c.shedOne(d, ls, ls.Def.MinKW, "conservative_shed", d.Window, now)
		} else if ls.LimitKW > ls.Def.MinKW {
			c.shedOne(d, ls, ls.Def.MinKW, "conservative_shed", d.Window, now)
		} else {
			// 已处于保底：不重复下指令，但每个保守窗口留一次状态审计。
			c.journal.Append(AuditEntry{
				TS: now, Category: "fault",
				Reason: "conservative_shed_held",
				Target: id,
				Value:  ls.LimitKW,
				Detail: "load kept at minimum under conservative mode",
				DedupKey: fmt.Sprintf("conservative|hold|%s|%s",
					id, d.Window.Start.Format(time.RFC3339Nano)),
			})
		}
		ls.ReleaseOK = 0
	}

	// 若此刻数据已恢复且需量安全，允许进入恢复观察（仍保守一个 HoldTicks 周期）。
	if !d.GridStale && d.DemandReliable && !d.ClockJump && d.ProjectedDemand > 0 &&
		d.ProjectedDemand <= c.cfg.DemandLimitKW-c.cfg.ReleaseMarginKW {
		for _, ls := range c.loads {
			if !ls.Manual {
				ls.ReleaseOK++
			}
		}
		d.Reasons = append(d.Reasons, fmt.Sprintf(
			"conservative recovery observation, release hold in progress"))
	}
}

// manualControl 存在人工强制：被强制对象严格照人工执行；
// 其余对象仍执行自动需量保护，但不得覆盖人工状态。
func (c *Controller) manualControl(d *Decision, caps storage.Capabilities,
	w demand.Window, now time.Time) {

	// 储能人工指令。
	if ms, ok := c.manual[c.cfg.StorageID]; ok && ms.active {
		switch ms.kind {
		case "storage_force_charge", "storage_force_discharge":
			v := math.Abs(ms.valueKW)
			if ms.kind == "storage_force_discharge" {
				v = -v
			}
			c.emitStorage(d, roundKV(v), "manual:"+ms.kind, w, now)
		case "storage_unlock":
			delete(c.manual, c.cfg.StorageID)
		default:
			c.emitStorage(d, 0, "manual_idle", w, now)
		}
	}

	// 自动保护仍然运行，但跳过处于人工状态的负载，且削减只能更保守。
	lastKW := d.CurrentDemand
	if lp, ok := demand.LastAtOrBefore(c.gridPoints(w.Start, now), now); ok {
		lastKW = lp.Value
	}
	projected := d.ProjectedDemand
	if projected >= c.cfg.DemandLimitKW-c.cfg.StartMarginKW {
		need := requiredReduction(d.CurrentDemand, lastKW, c.cfg.DemandLimitKW, w, now)
		if ms, ok := c.manual[c.cfg.StorageID]; !ok || !ms.active {
			discharge := math.Min(math.Min(caps.DischargeKW, caps.DischargeSustainKW), need)
			if !caps.Unreliable && discharge > 0.5 {
				c.emitStorage(d, -roundKV(discharge), "manual_mode_auto_discharge", w, now)
				need -= discharge
			}
		}
		for _, id := range c.loadOrder {
			ls := c.loads[id]
			if need <= 0.5 {
				break
			}
			if ls.Manual {
				continue
			}
			cut := math.Min(ls.LimitKW-ls.Def.MinKW, need)
			if cut > 0.5 {
				c.shedOne(d, ls, ls.LimitKW-cut, "manual_mode_auto_shed", w, now)
				need -= cut
			}
		}
	}
}

func (c *Controller) shedLoads(d *Decision, residualKW float64,
	w demand.Window, now time.Time) {
	for _, id := range c.loadOrder {
		if residualKW <= 0.5 {
			return
		}
		ls := c.loads[id]
		if ls.Manual {
			d.Reasons = append(d.Reasons,
				fmt.Sprintf("load %s under manual override, not auto-shed", id))
			continue
		}
		cut := math.Min(ls.LimitKW-ls.Def.MinKW, residualKW)
		if cut <= 0.5 {
			continue
		}
		c.shedOne(d, ls, ls.LimitKW-cut, "priority_shed", w, now)
		residualKW -= cut
	}
	if residualKW > 0.5 {
		d.Reasons = append(d.Reasons, fmt.Sprintf(
			"%.1f kW reduction gap remains after shedding all adjustable loads", residualKW))
	}
}

func (c *Controller) shedOne(d *Decision, ls *LoadState, newLimit float64,
	reason string, w demand.Window, now time.Time) {
	newLimit = roundKV(math.Max(ls.Def.MinKW, newLimit))
	if ls.Shed && math.Abs(ls.LimitKW-newLimit) < 1e-9 {
		return
	}
	ls.Shed = true
	ls.LimitKW = newLimit
	ls.ReleaseOK = 0
	cmd := Command{
		ID:      c.cmdID("load_limit_kw", ls.Def.ID, newLimit, w),
		TS:      now,
		Target:  ls.Def.ID,
		Kind:    "load_limit_kw",
		ValueKW: newLimit,
		Reason:  reason,
		DedupKey: fmt.Sprintf("cmd|load_limit|%s|%s|%.1f",
			ls.Def.ID, w.Start.Format(time.RFC3339Nano), newLimit),
	}
	d.Commands = append(d.Commands, cmd)
	d.Reasons = append(d.Reasons, fmt.Sprintf(
		"shed load %s (prio %d) limit=%.1f kW: %s", ls.Def.ID, ls.Def.Priority, newLimit, reason))
	c.commandHistory = append(c.commandHistory, cmd)
	c.auditCommand(now, cmd)
}

func (c *Controller) restoreLoad(d *Decision, ls *LoadState,
	reason string, w demand.Window, now time.Time) {
	ls.Shed = false
	ls.LimitKW = ls.Def.RatedKW
	ls.ReleaseOK = 0
	cmd := Command{
		ID:      c.cmdID("load_restore", ls.Def.ID, ls.Def.RatedKW, w),
		TS:      now,
		Target:  ls.Def.ID,
		Kind:    "load_restore",
		ValueKW: ls.Def.RatedKW,
		Reason:  reason,
		DedupKey: fmt.Sprintf("cmd|load_restore|%s|%s",
			ls.Def.ID, w.Start.Format(time.RFC3339Nano)),
	}
	d.Commands = append(d.Commands, cmd)
	d.Reasons = append(d.Reasons, fmt.Sprintf("restore load %s: %s", ls.Def.ID, reason))
	c.commandHistory = append(c.commandHistory, cmd)
	c.auditCommand(now, cmd)
}

func (c *Controller) emitStorage(d *Decision, kw float64, reason string,
	w demand.Window, now time.Time) {
	sig := fmt.Sprintf("%.1f@%s", kw, w.Start.Format(time.RFC3339Nano))
	if kw == 0 {
		sig = "0@" + w.Start.Format(time.RFC3339Nano)
	}
	// 同一窗口内相同设定值不重复下发；不同原因导致同值也视为无新动作。
	if c.lastStorageSig == sig {
		return
	}
	c.lastStorageSig = sig
	switch {
	case kw < 0:
		d.StorageMode, d.StorageKW = "discharge", kw
	case kw > 0:
		d.StorageMode, d.StorageKW = "charge", kw
	default:
		d.StorageMode, d.StorageKW = "idle", 0
	}
	cmd := Command{
		ID:      c.cmdID("storage_set_kw", c.cfg.StorageID, kw, w),
		TS:      now,
		Target:  c.cfg.StorageID,
		Kind:    "storage_set_kw",
		ValueKW: kw,
		Reason:  reason,
		DedupKey: fmt.Sprintf("cmd|storage|%s|%.1f",
			w.Start.Format(time.RFC3339Nano), kw),
	}
	d.Commands = append(d.Commands, cmd)
	d.Reasons = append(d.Reasons, fmt.Sprintf(
		"storage %s %.1f kW: %s", d.StorageMode, math.Abs(kw), reason))
	c.commandHistory = append(c.commandHistory, cmd)
	c.auditCommand(now, cmd)
}

func (c *Controller) cmdID(kind, target string, kw float64, w demand.Window) string {
	return fmt.Sprintf("%s|%s|%s|%.1f", kind, target,
		w.Start.Format("20060102T150405"), kw)
}

func (c *Controller) auditCommand(now time.Time, cmd Command) {
	entry := AuditEntry{
		TS: now, Category: "command", Reason: cmd.Reason,
		Target: cmd.Target, Value: cmd.ValueKW,
		Detail:   fmt.Sprintf("%s %.1f kW", cmd.Kind, cmd.ValueKW),
		DedupKey: cmd.DedupKey,
	}
	if c.replay {
		// 重放历史：只对齐去重水位，不重复落盘动作。
		c.journal.PrimeFrom([]AuditEntry{entry})
		return
	}
	c.journal.Append(entry)
}
