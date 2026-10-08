package demand

import (
	"fmt"
	"sort"
	"time"
)

// ApplyOverride forces a device instruction. It is respected on every tick
// until released, and recorded on the audit trail exactly once.
func (c *Controller) ApplyOverride(operator, deviceID string, kind ActionKind, kw float64, reason string) AuditRecord {
	c.mu.Lock()
	defer c.mu.Unlock()

	rec := AuditRecord{
		At: c.now(), Operator: operator, DeviceID: deviceID,
		Kind: kind, KW: round1(kw), Reason: reason,
		Ref: fmt.Sprintf("OVR-%d-%s", len(c.audits)+1, deviceID),
	}
	c.audits = append(c.audits, rec)
	c.overrides[deviceID] = rec

	switch kind {
	case ActBatteryDischarge, ActBatteryCharge, ActBatteryIdle:
		c.batteryKW[deviceID] = round1(kw)
	case ActLoadShed:
		c.loadAllow[deviceID] = 0
	case ActLoadRestore:
		if l, ok := c.loadSpec(deviceID); ok {
			c.loadAllow[deviceID] = l.RatedKW
		}
	}
	// Emit promptly on the next Decide; also reset suppression so the
	// forced transition is never mistaken for "unchanged".
	delete(c.lastEmit, deviceID)
	c.pendingEmitted = append(c.pendingEmitted, Action{
		Kind: kind, Cause: CauseManualOverride, DeviceID: deviceID,
		KW: rec.KW, At: rec.At, AuditRef: rec.Ref,
	})
	return rec
}

// ReleaseOverride ends a forced instruction; normal control resumes.
func (c *Controller) ReleaseOverride(deviceID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.overrides[deviceID]; !ok {
		return false
	}
	delete(c.overrides, deviceID)
	delete(c.lastEmit, deviceID)
	return true
}

// AuditTrail returns a copy of all audit records.
func (c *Controller) AuditTrail() []AuditRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]AuditRecord(nil), c.audits...)
}

func (c *Controller) loadSpec(id string) (LoadSpec, bool) {
	for _, l := range c.cfg.Loads {
		if l.ID == id {
			return l, true
		}
	}
	return LoadSpec{}, false
}

// mergeOverrides drops computed instructions for forced devices and appends
// the forced instruction. Suppression is applied once by the caller.
func (c *Controller) mergeOverrides(now time.Time, computed []Action) []Action {
	out := make([]Action, 0, len(computed)+len(c.overrides))
	for _, a := range computed {
		if _, forced := c.overrides[a.DeviceID]; forced {
			continue
		}
		out = append(out, a)
	}
	ids := make([]string, 0, len(c.overrides))
	for id := range c.overrides {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		ov := c.overrides[id]
		kw := ov.KW
		if ov.Kind == ActLoadShed {
			kw = 0
			if l, ok := c.loadSpec(id); ok {
				c.loadAllow[id] = 0
				_ = l
			}
		}
		out = append(out, Action{
			Kind: ov.Kind, Cause: CauseManualOverride, DeviceID: id,
			KW: kw, At: now, AuditRef: ov.Ref,
		})
	}
	return out
}
