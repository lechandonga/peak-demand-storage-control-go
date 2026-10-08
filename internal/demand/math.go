package demand

import (
	"math"
	"strconv"
)

func round1(v float64) float64 { return math.Round(v*10) / 10 }
func round2(v float64) float64 { return math.Round(v*100) / 100 }

func minF(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func quantize(v float64) string {
	// 0.1 kW quantization keeps noise from producing churn.
	return strconv.FormatFloat(math.Round(v*10)/10, 'f', 1, 64)
}

// clampDischarge returns usable discharge kW, bounded by PCS/temperature
// power, the energy above the backup reserve over the tick, and the need.
func clampDischarge(st BatteryStatus, dtMin, needKW float64) float64 {
	if needKW <= 0 || dtMin <= 0 {
		return 0
	}
	p := minF(st.UsableDischargeKW, needKW)
	// Energy bound: do not commit power that would cross BackupSOC this tick.
	reserveWh := (st.SOC - st.Spec.BackupSOC) / 100 * st.Spec.CapacityWh
	_, ed := st.Spec.eff()
	maxEnergyKW := reserveWh * ed / 1000 * 60 / dtMin
	if maxEnergyKW < p {
		p = maxEnergyKW
	}
	if p < 0 {
		p = 0
	}
	return round1(p)
}

// clampCharge returns usable charging kW bounded by headroom, power limits
// and the free cell capacity over the tick.
func clampCharge(st BatteryStatus, dtMin, headroomKW float64) float64 {
	if headroomKW <= 0 || dtMin <= 0 {
		return 0
	}
	p := minF(st.UsableChargeKW, headroomKW)
	headWh := (100 - st.SOC) / 100 * st.Spec.CapacityWh
	ec, _ := st.Spec.eff()
	maxEnergyKW := headWh / ec / 1000 * 60 / dtMin
	if maxEnergyKW < p {
		p = maxEnergyKW
	}
	if p < 0 {
		p = 0
	}
	return round1(p)
}
