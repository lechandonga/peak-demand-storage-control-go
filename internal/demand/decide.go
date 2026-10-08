package demand

import (
	"fmt"
	"sort"
	"time"
)

// Decide runs one control evaluation at the current clock.
func (c *Controller) Decide() *Decision {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now()
	c.tickCount++
	snap := c.store.snapshotWithAge(now)

	d := &Decision{At: now}

	// Fault detection: clock jumps / rewinds and telemetry loss.
	reasons := []string{}
	if c.started {
		gap := now.Sub(c.lastTick)
		switch {
		case gap < 0:
			reasons = append(reasons, "clock_moved_backwards")
			c.book.resetAt(now)
		case gap > c.cfg.MaxTimeJump:
			reasons = append(reasons, "time_jump:"+gap.Round(time.Second).String())
			c.book.resetAt(now)
		}
	}

	grid, gridOK := pickLatest(snap, SourceGrid)
	batts := pickAllBatt(snap, c.cfg.Batteries, c.cfg.StaleAfter)
	loads := pickAllLoad(snap, c.cfg.Loads, c.cfg.StaleAfter)

	if !gridOK {
		reasons = append(reasons, "grid_telemetry_missing")
	} else if grid.age >= c.cfg.StaleAfter {
		reasons = append(reasons, "grid_telemetry_stale")
	}
	if len(c.cfg.Batteries) > 0 {
		anyFresh := false
		for _, b := range batts {
			if b.present && b.age < c.cfg.StaleAfter {
				anyFresh = true
			}
		}
		if !anyFresh {
			reasons = append(reasons, "battery_telemetry_lost")
		}
	}

	faultNow := len(reasons) > 0
	switch {
	case faultNow && !c.fault.Active:
		c.fault = FaultState{Active: true, Reasons: reasons, Since: now, Detail: "entering conservative mode"}
	case faultNow:
		c.fault.Reasons = reasons
	}

	// Integrate the fixed window with sample-and-hold at each tick.
	if gridOK {
		c.book.Add(now, grid.sample.PowerKW)
	}
	d.WindowStart = c.book.WindowStart(now)
	d.WindowEnd = d.WindowStart.Add(c.cfg.WindowWidth)
	d.CurrentDemandKW = round1(c.book.CurrentDemand())
	d.PeakKW, d.PeakAt = c.book.Peak()
	elapsed := now.Sub(d.WindowStart).Minutes()
	if elapsed < 0 {
		elapsed = 0
	}
	remain := c.cfg.WindowWidth.Minutes() - elapsed
	if remain < 0 {
		remain = 0
	}
	d.RemainingMinutes = round2(remain)
	d.ProjectedDemandKW = d.CurrentDemandKW
	// React immediately to a fresh spike: the control projection uses the
	// larger of the instantaneous reading and the window mean. The mean
	// remains the authoritative demand/peak accounting figure.
	if gridOK && grid.sample.PowerKW > d.ProjectedDemandKW {
		d.ProjectedDemandKW = round1(grid.sample.PowerKW)
	}

	// Active defense target = min(contract, historical peak).
	target := c.cfg.ContractKW
	if d.PeakKW > 0 && d.PeakKW < target {
		target = d.PeakKW
	}

	var emitted []Action
	switch {
	case faultNow:
		c.healthy = 0
		emitted = c.buildConservative(now, batts, loads)
		d.Fault = c.fault
	case c.fault.Active:
		// Hold conservative outputs through a stability probation, then
		// resume normal control in one clean transition.
		c.healthy++
		if c.healthy < c.cfg.MinStableTicks {
			emitted = c.buildConservative(now, batts, loads)
			d.Fault = FaultState{
				Active: true, Since: c.fault.Since,
				Detail: fmt.Sprintf("recovery hold %d/%d", c.healthy, c.cfg.MinStableTicks),
			}
		} else {
			c.fault = FaultState{}
			emitted = c.buildNormal(now, d, target, batts, loads, true)
			emitted = append(emitted, Action{Kind: ActNothing, Cause: CauseFaultClear, At: now})
		}
	default:
		emitted = c.buildNormal(now, d, target, batts, loads, false)
	}

	emitted = c.mergeOverrides(now, emitted)
	emitted = append(emitted, c.pendingEmitted...)
	c.pendingEmitted = nil
	emitted = c.emitFilter(emitted)

	d.Actions = c.allCurrentActions(now)
	d.EmittedActions = emitted
	d.Fingerprint = c.fingerprint(now, snap, d)

	c.lastTick = now
	c.started = true
	return d
}

func (c *Controller) buildConservative(now time.Time, batts []battView, loads []loadView) []Action {
	var out []Action
	for _, b := range batts {
		out = append(out, c.setBattery(now, b.id, 0, CauseFaultConserve))
	}
	for _, lv := range loads {
		if !lv.present {
			continue // never command a device we have not seen
		}
		out = append(out, c.setLoad(now, lv.id, 0, ActLoadShed, CauseFaultConserve))
	}
	return out
}

func (c *Controller) buildNormal(now time.Time, d *Decision, target float64,
	batts []battView, loads []loadView, fromFault bool) []Action {

	var out []Action
	projection := d.ProjectedDemandKW
	engageAt := target - c.cfg.RiskLeadKW

	if projection >= engageAt {
		c.healthy = 0
		need := projection - engageAt
		cause := CausePeakAtRisk
		if projection >= c.cfg.ContractKW-c.cfg.RiskLeadKW {
			cause = CauseCapAtRisk
		}

		totalDischarge := 0.0
		for _, b := range sortedBatts(batts) {
			if !b.fresh {
				out = append(out, c.setBattery(now, b.id, 0, CauseStorageLimited))
				continue
			}
			p := clampDischarge(b.status, c.cfg.TickInterval.Minutes(), need-totalDischarge)
			totalDischarge += p
			out = append(out, c.setBattery(now, b.id, p, cause))
		}

		residual := need - totalDischarge
		for _, lv := range sortedLoads(loads) {
			if residual <= 0 {
				break
			}
			if !lv.fresh {
				continue
			}
			cur := c.loadAllow[lv.id]
			targetAllow := cur - residual
			if targetAllow < lv.spec.MinKW {
				targetAllow = 0
			}
			if targetAllow < cur {
				out = append(out, c.setLoad(now, lv.id, targetAllow, ActLoadShed, CauseStorageLimited))
				residual -= cur - targetAllow
			}
		}
		return out
	}

	if !fromFault {
		c.healthy++
	}
	stable := c.healthy >= c.cfg.MinStableTicks

	if projection <= c.cfg.ValleyThresholdKW {
		headroom := target - c.cfg.ChargeHeadroomKW - projection
		for _, b := range sortedBatts(batts) {
			if !b.fresh || headroom <= 0 {
				out = append(out, c.setBattery(now, b.id, 0, CauseUnchanged))
				continue
			}
			p := clampCharge(b.status, c.cfg.TickInterval.Minutes(), headroom)
			headroom -= p
			out = append(out, c.setBattery(now, b.id, -p, CauseValleyCharge))
		}
	} else {
		for _, b := range sortedBatts(batts) {
			out = append(out, c.setBattery(now, b.id, 0, CauseUnchanged))
		}
	}

	if stable {
		room := target - c.cfg.RestoreLagKW - projection
		for _, lv := range reverseSortedLoads(loads) {
			if !lv.fresh || room <= 0 {
				continue
			}
			cur := c.loadAllow[lv.id]
			if cur >= lv.spec.RatedKW {
				continue
			}
			step := minF(lv.spec.RatedKW-cur, room)
			out = append(out, c.setLoad(now, lv.id, cur+step, ActLoadRestore, CauseStableRecovery))
			room -= step
		}
	}
	return out
}

func (c *Controller) setBattery(now time.Time, id string, kw float64, cause ActionCause) Action {
	kw = round1(kw)
	c.batteryKW[id] = kw
	kind := ActBatteryIdle
	switch {
	case kw > 0.05:
		kind = ActBatteryDischarge
	case kw < -0.05:
		kind = ActBatteryCharge
	}
	return Action{Kind: kind, Cause: cause, DeviceID: id, KW: kw, At: now}
}

func (c *Controller) setLoad(now time.Time, id string, kw float64, kind ActionKind, cause ActionCause) Action {
	kw = round1(kw)
	c.loadAllow[id] = kw
	return Action{Kind: kind, Cause: cause, DeviceID: id, KW: kw, At: now}
}

func (c *Controller) emitFilter(in []Action) []Action {
	out := make([]Action, 0, len(in))
	for _, a := range in {
		k := a.key()
		if c.lastEmit[a.DeviceID] == k {
			continue
		}
		c.lastEmit[a.DeviceID] = k
		out = append(out, a)
	}
	return out
}

func (c *Controller) allCurrentActions(now time.Time) []Action {
	out := make([]Action, 0, len(c.batteryKW)+len(c.loadAllow))
	ids := make([]string, 0, len(c.batteryKW))
	for id := range c.batteryKW {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		kw := c.batteryKW[id]
		kind := ActBatteryIdle
		if kw > 0.05 {
			kind = ActBatteryDischarge
		} else if kw < -0.05 {
			kind = ActBatteryCharge
		}
		out = append(out, Action{Kind: kind, DeviceID: id, KW: kw, At: now})
	}
	ids = ids[:0]
	for id := range c.loadAllow {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		kind := ActionKind("load_state")
		if c.loadAllow[id] <= 0.05 {
			kind = ActLoadShed
		}
		out = append(out, Action{Kind: kind, DeviceID: id, KW: c.loadAllow[id], At: now})
	}
	return out
}
