package demand

import "math"

// BatterySpec describes one storage unit's physical envelope.
type BatterySpec struct {
	ID string
	// CapacityWh is the usable energy capacity.
	CapacityWh float64
	// MaxDischargeKW / MaxChargeKW are PCS power limits.
	MaxDischargeKW float64
	MaxChargeKW    float64
	// ChargeEff and DischargeEff are one-way efficiencies in (0,1].
	// A discharge of P kW removes P/EffD kWh from the cell; a charge
	// setpoint of P kW stores P*EffC kWh in the cell.
	ChargeEff    float64
	DischargeEff float64
	// BackupSOC is the reserve floor below which no discharge is allowed
	// (except an explicit manual override).
	BackupSOC float64
	// TempDeratingHot/Cold define the linear derating envelope: above
	// TempHot (or below TempCold) usable power shrinks to zero at the
	// shutdown temperature.
	TempHot     float64
	TempHotCut  float64
	TempCold    float64
	TempColdCut float64
}

// BatteryStatus is the controller's live view of one storage unit.
type BatteryStatus struct {
	Spec BatterySpec
	SOC  float64
	Temp float64
	// Limited explains why full power is unavailable, if non-empty.
	Limited string
	// UsableDischargeKW / UsableChargeKW after SOC and temperature limits.
	UsableDischargeKW float64
	UsableChargeKW    float64
}

func (s BatterySpec) eff() (float64, float64) {
	ec, ed := s.ChargeEff, s.DischargeEff
	if ec <= 0 || ec > 1 {
		ec = 1
	}
	if ed <= 0 || ed > 1 {
		ed = 1
	}
	return ec, ed
}

// tempDerate returns a factor in [0,1] for temperature-driven power limits.
func (s BatterySpec) tempDerate(tempC float64) float64 {
	f := 1.0
	if tempC >= s.TempHotCut {
		f = 0
	} else if tempC > s.TempHot {
		f = math.Min(f, (s.TempHotCut-tempC)/(s.TempHotCut-s.TempHot))
	}
	if tempC <= s.TempColdCut {
		f = 0
	} else if tempC < s.TempCold {
		f = math.Min(f, (tempC-s.TempColdCut)/(s.TempCold-s.TempColdCut))
	}
	return math.Max(0, math.Min(1, f))
}

// deriveStatus computes usable power and the limiting reason.
func deriveStatus(spec BatterySpec, soc, temp float64) BatteryStatus {
	st := BatteryStatus{Spec: spec, SOC: soc, Temp: temp}
	derate := spec.tempDerate(temp)

	st.UsableDischargeKW = spec.MaxDischargeKW * derate
	st.UsableChargeKW = spec.MaxChargeKW * derate
	if derate <= 0 {
		st.Limited = "temperature_shutdown"
	} else if derate < 1 {
		st.Limited = "temperature_derated"
	}

	switch {
	case soc <= spec.BackupSOC:
		st.UsableDischargeKW = 0
		if st.Limited == "" {
			st.Limited = "at_backup_soc"
		} else {
			st.Limited += ";at_backup_soc"
		}
	case soc >= 100:
		st.UsableChargeKW = 0
		if st.Limited == "" {
			st.Limited = "soc_full"
		}
	}
	return st
}

// ApplyBatteryEnergy advances the simulated SOC for one control interval.
// dischargeKW > 0 discharges, dischargeKW < 0 charges. Efficiency losses are
// applied so SOC change reconciles with delivered energy:
//
//	discharge: SOC drops by P/(EffD*Cap); charge: SOC rises by P*EffC/Cap.
func ApplyBatteryEnergy(spec BatterySpec, soc float64, dischargeKW float64, dtMin float64) float64 {
	ec, ed := spec.eff()
	dtH := dtMin / 60
	var dSOC float64
	switch {
	case dischargeKW > 0:
		dSOC = -(dischargeKW / ed) * dtH / spec.CapacityWh * 1000 * 100
	case dischargeKW < 0:
		dSOC = (-dischargeKW * ec) * dtH / spec.CapacityWh * 1000 * 100
	}
	return math.Max(0, math.Min(100, soc+dSOC))
}

// ReconcileConfig validates that telemetered SOC motion matches the energy
// implied by power and elapsed time, allowing measurement noise.
type ReconcileConfig struct {
	CapacityWh              float64
	ChargeEff, DischargeEff float64
	SOCNoisePct             float64
}

// PredictSOC returns the SOC expected after dtMin given powerKW (discharge +).
func PredictSOC(rc ReconcileConfig, soc0, powerKW, dtMin float64) float64 {
	if rc.CapacityWh <= 0 || dtMin <= 0 {
		return soc0
	}
	ec, ed := rc.ChargeEff, rc.DischargeEff
	if ec <= 0 || ec > 1 {
		ec = 1
	}
	if ed <= 0 || ed > 1 {
		ed = 1
	}
	dtH := dtMin / 60
	var dSOC float64
	switch {
	case powerKW > 0:
		dSOC = -(powerKW / ed) * dtH / rc.CapacityWh * 1000 * 100
	case powerKW < 0:
		dSOC = (-powerKW * ec) * dtH / rc.CapacityWh * 1000 * 100
	}
	return soc0 + dSOC
}
