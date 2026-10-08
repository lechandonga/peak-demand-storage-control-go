package demand

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// fingerprint is deterministic over clock, accepted device states, setpoints,
// overrides and window accounting: identical inputs at any entry point or
// under concurrency produce the identical decision fingerprint.
func (c *Controller) fingerprint(now time.Time, snap map[string]agedSample, d *Decision) string {
	keys := make([]string, 0, len(snap))
	for k := range snap {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(strconv.FormatInt(now.UnixNano(), 10))
	b.WriteByte(';')
	for _, k := range keys {
		s := snap[k].sample
		fmt.Fprintf(&b, "%s=%d:%.4f:%.4f:%.2f,", k, s.At.UnixNano(), s.PowerKW, s.EnergyWh, s.SOC)
	}
	b.WriteString(";set:")
	setIDs := make([]string, 0)
	for id := range c.batteryKW {
		setIDs = append(setIDs, id)
	}
	for id := range c.loadAllow {
		setIDs = append(setIDs, id)
	}
	sort.Strings(setIDs)
	for _, id := range setIDs {
		v, okB := c.batteryKW[id]
		if !okB {
			v = c.loadAllow[id]
		}
		fmt.Fprintf(&b, "%s:%.1f,", id, v)
	}
	fmt.Fprintf(&b, ";win=%.4f:%.4f:%d", d.CurrentDemandKW, d.ProjectedDemandKW, d.WindowStart.UnixNano())
	fmt.Fprintf(&b, ";peak=%.4f;fault=%v;ovr=%d;tick=%d", d.PeakKW, d.Fault.Active, len(c.overrides), c.tickCount)
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:12])
}
