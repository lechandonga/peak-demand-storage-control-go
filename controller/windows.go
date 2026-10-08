package controller

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/lechandonga/peak-demand-storage-control-go/demand"
	"github.com/lechandonga/peak-demand-storage-control-go/storage"
	"github.com/lechandonga/peak-demand-storage-control-go/telemetry"
)

func (c *Controller) closeExpiredWindows(d *Decision, cur demand.Window) {
	if c.lastTick.IsZero() {
		return
	}
	anchorPrev := demand.DayAnchor(c.lastTick, c.cfg.Location)
	prevW := demand.CurrentWindow(c.lastTick, anchorPrev, c.cfg.WindowLength)
	for prevW.Start.Before(cur.Start) {
		key := prevW.Start.Format(time.RFC3339Nano)
		if !c.windowClosed[key] {
			pts := c.gridPoints(prevW.Start, prevW.End)
			avg, ok := demand.AverageDemand(pts, prevW.Start, prevW.End)
			if !ok {
				c.journal.Append(AuditEntry{
					TS: d.TS, Category: "fault", Reason: "window_unclosed_missing_data",
					Target:   c.cfg.GridAssetID,
					Detail:   fmt.Sprintf("window %s cannot be settled reliably, peak not updated", key),
					DedupKey: "window|missing|" + key,
				})
			} else {
				rec := demand.PeakRecord{WindowStart: prevW.Start,
					WindowEnd: prevW.End, DemandKW: roundKV(avg)}
				before, hadBefore := c.book.CurrentMonthPeak(prevW.Start)
				c.book.CloseWindow(rec)
				after, _ := c.book.CurrentMonthPeak(prevW.Start)
				isNew := !hadBefore ||
					after.WindowStart.Equal(rec.WindowStart) && rec.DemandKW > before.DemandKW+1e-9
				if isNew {
					d.PeakNew = true
				}
				d.ClosedWindow = &rec
				reason := "window_closed"
				if isNew {
					reason = "new_monthly_peak"
				}
				c.journal.Append(AuditEntry{
					TS: d.TS, Category: "peak", Reason: reason,
					Target: c.cfg.GridAssetID, Value: rec.DemandKW,
					Detail: fmt.Sprintf("window %s avg %.2f kW, month peak %.2f kW",
						key, rec.DemandKW, after.DemandKW),
					DedupKey: "window|close|" + key,
				})
			}
			c.windowClosed[key] = true
		}
		prevW = demand.Window{Start: prevW.End, End: prevW.End.Add(c.cfg.WindowLength)}
	}
}

// feedLedger 把新到储能遥测按时间顺序（SOC→温度→功率）喂入账本。
// ledger 内部按样本 ID 去重，重复回放不重复累计。
func (c *Controller) feedLedger(now time.Time) {
	type item struct {
		ts   time.Time
		kind telemetry.Kind
		s    telemetry.Sample
	}
	var all []item
	collect := func(kind telemetry.Kind) {
		for _, s := range c.store.All(telemetry.SourceStorage, c.cfg.StorageID, kind) {
			if s.TS.After(now.Add(time.Second)) {
				continue
			}
			all = append(all, item{s.TS, kind, s})
		}
	}
	collect(telemetry.KindStorageSOC)
	collect(telemetry.KindStorageTemp)
	collect(telemetry.KindStoragePower)
	sort.SliceStable(all, func(i, j int) bool {
		if !all[i].ts.Equal(all[j].ts) {
			return all[i].ts.Before(all[j].ts)
		}
		return kindRank(all[i].kind) < kindRank(all[j].kind)
	})
	for _, it := range all {
		s := it.s
		if c.powerFed[s.ID] {
			continue
		}
		switch it.kind {
		case telemetry.KindStorageSOC:
			if mm := c.ledger.ApplySOC(s.ID, s.TS, s.Value); mm != nil {
				c.journal.Append(AuditEntry{
					TS: s.TS, Category: "soc", Reason: "soc_energy_mismatch",
					Target: c.cfg.StorageID, Value: mm.Delta, Detail: mm.Error(),
					DedupKey: "soc|mismatch|" + s.ID,
				})
			}
		case telemetry.KindStorageTemp:
			c.ledger.ApplyTemp(s.Value)
		case telemetry.KindStoragePower:
			if err := c.ledger.ApplyPower(storage.PowerSegment{
				SampleID: s.ID, TS: s.TS, PowerKW: s.Value}); err != nil {
				c.journal.Append(AuditEntry{
					TS: s.TS, Category: "fault", Reason: "ledger_power_rejected",
					Target: c.cfg.StorageID, Detail: err.Error(),
					DedupKey: "ledger|pwrerr|" + s.ID,
				})
			}
		}
		c.powerFed[s.ID] = true
	}
}

// kindRank 保证同一时间戳按 SOC→温度→功率喂入（先标定再积分）。
func kindRank(k telemetry.Kind) int {
	switch k {
	case telemetry.KindStorageSOC:
		return 0
	case telemetry.KindStorageTemp:
		return 1
	default:
		return 2
	}
}

func (c *Controller) checkFreshness(now time.Time, d *Decision) {
	if sm, ok := c.store.Latest(telemetry.SourceGrid, c.cfg.GridAssetID,
		telemetry.KindGridPower); ok {
		c.lastGridTS = sm.TS
		if now.Sub(sm.TS) > c.cfg.StaleGrid {
			d.GridStale = true
			d.Reasons = append(d.Reasons, fmt.Sprintf(
				"grid telemetry stale: latest %s age %s",
				sm.TS.Format(time.RFC3339), now.Sub(sm.TS).Round(time.Second)))
		}
	} else if c.primed {
		d.GridStale = true
		d.Reasons = append(d.Reasons, "grid telemetry never received")
	}
	for _, id := range c.loadOrder {
		if sm, ok := c.store.Latest(telemetry.SourceLoad, id, telemetry.KindLoadPower); ok {
			if now.Sub(sm.TS) > c.cfg.StaleLoad {
				d.LoadsStale = append(d.LoadsStale, id)
			}
		}
	}
	sort.Strings(d.LoadsStale)
	if sm, ok := c.store.Latest(telemetry.SourceStorage, c.cfg.StorageID,
		telemetry.KindStoragePower); ok && now.Sub(sm.TS) > c.cfg.StaleBESS {
		d.BESSStale = true
		d.Reasons = append(d.Reasons, "storage telemetry stale")
	}
	if snap := c.ledger.Snapshot(); snap.Mismatch != nil {
		d.Reasons = append(d.Reasons, "storage SOC inconsistent with energy ledger")
	}
}

func (c *Controller) gridPoints(from, to time.Time) []demand.Point {
	sms := c.store.Range(telemetry.SourceGrid, c.cfg.GridAssetID,
		telemetry.KindGridPower, from.Add(-2*c.cfg.WindowLength), to.Add(time.Second))
	pts := make([]demand.Point, 0, len(sms))
	for _, sm := range sms {
		pts = append(pts, demand.Point{TS: sm.TS, Value: sm.Value})
	}
	return pts
}

// requiredReduction 反推：要让窗口末预测需量降到 target，
// 从现在起电网功率需要持续降低多少 kW。
func requiredReduction(cur, lastKW, target float64, w demand.Window, now time.Time) float64 {
	elapsed := w.Elapsed(now).Seconds()
	remain := w.Remaining(now).Seconds()
	total := elapsed + remain
	if remain <= 0 {
		return math.Max(0, cur-target)
	}
	need := (cur*elapsed+lastKW*remain)/total - target
	if need <= 0 {
		return 0
	}
	return need * total / remain
}
