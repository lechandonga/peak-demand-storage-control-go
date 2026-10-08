package storage

import (
	"fmt"
	"math"
	"sync"
	"time"
)

// Config 是储能能量账本与调度约束配置。
type Config struct {
	CapacityKWh    float64 // 额定容量 kWh
	ChargeEff      float64 // 充电效率（交流->电池），0~1
	DischargeEff   float64 // 放电效率（电池->交流），0~1
	MinSOC         float64 // 备用下限，放电不得使 SOC 低于该值
	MaxSOC         float64 // 充电上限
	MaxChargeKW    float64 // 最大充电交流功率
	MaxDischargeKW float64 // 最大放电交流功率

	// 温度降额：[DerateLow,DerateHigh] 内满功率；
	// 从 DerateLow 线性下降到 CutoffLow 处为 0，高温同理。
	TempCutoffLow  float64
	TempDerateLow  float64
	TempDerateHigh float64
	TempCutoffHigh float64

	// SOCMismatchTol 上报 SOC 与账本 SOC 的最大允许偏差。
	SOCMismatchTol float64
}

// DefaultConfig 返回典型 100kWh/50kW 储能参数。
func DefaultConfig() Config {
	return Config{
		CapacityKWh:    100,
		ChargeEff:      0.95,
		DischargeEff:   0.95,
		MinSOC:         0.2,
		MaxSOC:         0.95,
		MaxChargeKW:    50,
		MaxDischargeKW: 50,
		TempCutoffLow:  -10,
		TempDerateLow:  5,
		TempDerateHigh: 45,
		TempCutoffHigh: 55,
		SOCMismatchTol: 0.05,
	}
}

// PowerSegment 是一段交流侧功率观测（正充负放）。
type PowerSegment struct {
	SampleID string
	TS       time.Time
	PowerKW  float64
}

// SOCMismatch 描述上报 SOC 与能量账本推算值不一致。
type SOCMismatch struct {
	TS       time.Time
	Reported float64
	Modeled  float64
	Delta    float64
	SampleID string
}

func (m SOCMismatch) Error() string {
	return fmt.Sprintf("storage soc mismatch at %s: reported %.4f modeled %.4f delta %.4f",
		m.TS.Format(time.RFC3339), m.Reported, m.Modeled, m.Delta)
}

// Ledger 是储能能量账本。
// 电池侧能量 = SOC*容量；充放电积分按效率换算，保证"电量变化与能量对得上"。
// Apply* 方法按样本 ID 去重，重复回放不重复累计。
type Ledger struct {
	cfg Config
	mu  sync.Mutex

	soc          float64
	initTS       time.Time
	initialized  bool
	lastPowerTS  time.Time
	appliedPower map[string]bool
	appliedSOC   map[string]bool

	mismatch   *SOCMismatch
	lastTempC  float64
	tempValid  bool
	lastRepSOC float64
}

// NewLedger 创建账本。
func NewLedger(cfg Config) *Ledger {
	return &Ledger{
		cfg:          cfg,
		appliedPower: make(map[string]bool),
		appliedSOC:   make(map[string]bool),
	}
}

// Init 用首个可信 SOC 样本初始化账本（重启续跑时由持久化状态调用）。
func (l *Ledger) Init(soc float64, ts time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.soc = clamp(soc, 0, 1)
	l.initTS = ts
	l.lastPowerTS = ts
	l.initialized = true
}

// Initialized 报告账本是否已被首个 SOC 样本初始化。
func (l *Ledger) Initialized() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.initialized
}

// ApplyPower 按交流侧功率积分更新电池能量；重复样本 ID 不产生二次累计。
// 要求时间戳单调（乱序由遥测层排序后调用）；零时长段忽略。
func (l *Ledger) ApplyPower(seg PowerSegment) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.appliedPower[seg.SampleID] {
		return nil
	}
	if !l.initialized {
		return fmt.Errorf("storage: power segment %s before soc init", seg.SampleID)
	}
	if seg.TS.Before(l.lastPowerTS) {
		return fmt.Errorf("storage: out-of-order power segment %s %s < %s",
			seg.SampleID, seg.TS.Format(time.RFC3339), l.lastPowerTS.Format(time.RFC3339))
	}
	hours := seg.TS.Sub(l.lastPowerTS).Hours()
	if hours > 0 {
		dSOC := acEnergyToSOC(seg.PowerKW, hours, l.cfg)
		l.soc = clamp(l.soc+dSOC, 0, 1)
		l.lastPowerTS = seg.TS
	}
	l.appliedPower[seg.SampleID] = true
	return nil
}

// acEnergyToSOC 把一段交流功率换算成电池 SOC 变化：
// 充电时进电池能量 = P*dt*etaCharge；放电时电池耗能 = |P|*dt/etaDischarge。
func acEnergyToSOC(powerKW, hours float64, cfg Config) float64 {
	if powerKW >= 0 {
		return powerKW * hours * cfg.ChargeEff / cfg.CapacityKWh
	}
	return powerKW * hours / cfg.DischargeEff / cfg.CapacityKWh
}

// ApplySOC 用上报表对账：首个样本初始化账本；之后偏差超容忍度返回 mismatch，
// 并冻结账本模型（不以上报值覆盖），由上层转入保守策略。
func (l *Ledger) ApplySOC(sampleID string, ts time.Time, reported float64) *SOCMismatch {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.appliedSOC[sampleID] {
		return nil
	}
	l.appliedSOC[sampleID] = true
	l.lastRepSOC = reported

	if !l.initialized {
		l.soc = clamp(reported, 0, 1)
		l.initTS = ts
		l.lastPowerTS = ts
		l.initialized = true
		return nil
	}
	delta := reported - l.soc
	if math.Abs(delta) > l.cfg.SOCMismatchTol {
		m := &SOCMismatch{TS: ts, Reported: reported, Modeled: l.soc,
			Delta: delta, SampleID: sampleID}
		l.mismatch = m
		return m
	}
	// 小偏差：以观测值做卡尔曼式轻校正（取中点），模型仍以能量积分为主。
	l.soc = clamp((l.soc+reported)/2, 0, 1)
	return nil
}

// ApplyTemp 更新最新电芯温度。
func (l *Ledger) ApplyTemp(tempC float64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lastTempC = tempC
	l.tempValid = true
}

// Snapshot 是账本状态快照（用于持久化与测试断言）。
type Snapshot struct {
	SOC                   float64
	LastPowerTS           time.Time
	InitTS                time.Time
	Mismatch              *SOCMismatch
	TempC                 float64
	TempValid             bool
	AvailableDischargeKWh float64 // 备用下限之上可放出的电池能量
	AvailableChargeKWh    float64 // 充到上限还能吸收的电池能量
}

// State 是可持久化的账本状态（重启续跑用）。
type State struct {
	SOC         float64
	LastPowerTS time.Time
	InitTS      time.Time
	TempC       float64
	TempValid   bool
	PowerIDs    []string
	SOCIDs      []string
}

// Snapshot 返回当前账本状态。
func (l *Ledger) Snapshot() Snapshot {
	l.mu.Lock()
	defer l.mu.Unlock()
	snap := Snapshot{
		SOC:         l.soc,
		LastPowerTS: l.lastPowerTS,
		InitTS:      l.initTS,
		Mismatch:    l.mismatch,
		TempC:       l.lastTempC,
		TempValid:   l.tempValid,
	}
	if l.soc > l.cfg.MinSOC {
		snap.AvailableDischargeKWh = (l.soc - l.cfg.MinSOC) * l.cfg.CapacityKWh
	}
	if l.soc < l.cfg.MaxSOC {
		snap.AvailableChargeKWh = (l.cfg.MaxSOC - l.soc) * l.cfg.CapacityKWh
	}
	return snap
}

// ExportState 导出可持久化状态。
func (l *Ledger) ExportState() State {
	l.mu.Lock()
	defer l.mu.Unlock()
	st := State{SOC: l.soc, LastPowerTS: l.lastPowerTS, InitTS: l.initTS,
		TempC: l.lastTempC, TempValid: l.tempValid,
		PowerIDs: make([]string, 0, len(l.appliedPower)),
		SOCIDs:   make([]string, 0, len(l.appliedSOC))}
	for id := range l.appliedPower {
		st.PowerIDs = append(st.PowerIDs, id)
	}
	for id := range l.appliedSOC {
		st.SOCIDs = append(st.SOCIDs, id)
	}
	return st
}

// RestoreState 恢复持久化状态。恢复后账本以快照能量为基线，
// 历史样本 ID 仍登记为已处理，重复回放不会二次累计。
func (l *Ledger) RestoreState(st State) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.soc = st.SOC
	l.lastPowerTS = st.LastPowerTS
	l.initTS = st.InitTS
	l.initialized = true
	l.lastTempC, l.tempValid = st.TempC, st.TempValid
	for _, id := range st.PowerIDs {
		l.appliedPower[id] = true
	}
	for _, id := range st.SOCIDs {
		l.appliedSOC[id] = true
	}
}

// Mismatch 返回最近一次 SOC 对账失败记录（未恢复前一直保留）。
func (l *Ledger) Mismatch() *SOCMismatch {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.mismatch
}

// ClearMismatch 在人工核查/重新标定后清除对账告警。
func (l *Ledger) ClearMismatch() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.mismatch = nil
}

// TempDerate 返回温度导致的功率降额系数 0~1。
func TempDerate(tempC float64, cfg Config) float64 {
	switch {
	case tempC <= cfg.TempCutoffLow || tempC >= cfg.TempCutoffHigh:
		return 0
	case tempC < cfg.TempDerateLow:
		return (tempC - cfg.TempCutoffLow) / (cfg.TempDerateLow - cfg.TempCutoffLow)
	case tempC > cfg.TempDerateHigh:
		return (cfg.TempCutoffHigh - tempC) / (cfg.TempCutoffHigh - cfg.TempDerateHigh)
	}
	return 1
}

// Capabilities 描述当前时刻储能可用的充/放功率与可持续能量。
type Capabilities struct {
	ChargeKW             float64 // 当前可立即执行的最大充电交流功率
	DischargeKW          float64 // 当前可立即执行的最大放电交流功率
	DischargeSustainKW   float64 // 能持续到窗口结束的放电功率（受能量约束）
	DischargeEnergyACKWh float64 // 可持续到窗口结束的放电交流电量
	ChargeEnergyACKWh    float64 // 到充电上限还能吸收的交流电量
	TempDerate           float64
	AtMinSOC             bool
	AtMaxSOC             bool
	Unreliable           bool // SOC 对账失败，不能依赖储能
}

// CapabilitiesAt 评估在给定温度、到 windowEnd 前的可调度能力。
func (l *Ledger) CapabilitiesAt(tempC float64, tempValid, unreliable bool, windowEnd time.Time, now time.Time) Capabilities {
	l.mu.Lock()
	cfg := l.cfg
	soc := l.soc
	l.mu.Unlock()

	c := Capabilities{AtMinSOC: soc <= cfg.MinSOC+1e-9, AtMaxSOC: soc >= cfg.MaxSOC-1e-9}
	derate := 1.0
	if tempValid {
		derate = TempDerate(tempC, cfg)
	} else {
		derate = 0 // 温度未知时不允许功率动作，保守处理
	}
	c.TempDerate = derate
	c.Unreliable = unreliable
	if unreliable || derate <= 0 {
		return c
	}
	c.ChargeKW = cfg.MaxChargeKW * derate
	c.DischargeKW = cfg.MaxDischargeKW * derate

	battDischarge := math.Max(0, (soc-cfg.MinSOC)*cfg.CapacityKWh)
	battCharge := math.Max(0, (cfg.MaxSOC-soc)*cfg.CapacityKWh)
	if battDischarge <= 0 {
		c.DischargeKW = 0
	}
	if battCharge <= 0 {
		c.ChargeKW = 0
	}
	// 交流侧可持续电量：放电经放电效率折减；充电需除以充电效率。
	c.DischargeEnergyACKWh = battDischarge * cfg.DischargeEff
	c.ChargeEnergyACKWh = battCharge / cfg.ChargeEff

	remainHours := windowEnd.Sub(now).Hours()
	if remainHours > 0 {
		c.DischargeSustainKW = math.Min(c.DischargeKW,
			c.DischargeEnergyACKWh/remainHours)
	}
	return c
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
