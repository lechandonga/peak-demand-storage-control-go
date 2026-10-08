package controller

import (
	"fmt"
	"sort"
	"time"

	"github.com/lechandonga/peak-demand-storage-control-go/demand"
	"github.com/lechandonga/peak-demand-storage-control-go/storage"
	"github.com/lechandonga/peak-demand-storage-control-go/telemetry"
)

// Mode 是控制器的决策模式。
type Mode string

const (
	ModeNormal       Mode = "normal"       // 正常控制
	ModeManual       Mode = "manual"       // 存在人工强制操作
	ModeConservative Mode = "conservative" // 失联/校验失败/时间跳变/SOC 对不上：宁可多限电
)

// LoadDef 定义一个可调负载。Priority 数字越小优先级越先削减。
type LoadDef struct {
	ID       string
	Priority int
	MinKW    float64 // 削减后允许的最低运行功率（保底）
	RatedKW  float64 // 额定（满投）功率
}

// Config 是需量控制器配置。
type Config struct {
	GridAssetID string
	StorageID   string

	DemandLimitKW float64       // 需量上限（合同容量）
	WindowLength  time.Duration // 需量窗口长度
	Location      *time.Location

	// 滞回与稳定条件。
	StartMarginKW   float64 // 预测值 >= 上限 - 该裕量 时启动动作
	ReleaseMarginKW float64 // 预测值 <= 上限 - 该裕量 才允许恢复投入（>StartMargin）
	HoldTicks       int     // 恢复条件需连续满足的 tick 数

	// 充电：负荷低谷、预测需量 <= 上限 - ChargeHeadroomKW 时允许充电。
	ChargeHeadroomKW float64
	ChargeMaxKW      float64

	// 失联判定。
	StaleGrid time.Duration
	StaleLoad time.Duration
	StaleBESS time.Duration

	Loads   []LoadDef
	Storage storage.Config

	// 时间跳变容忍：同一资产样本回退超过该值即判时钟异常。
	ClockJumpTolerance time.Duration
}

// DefaultConfig 返回典型 15 分钟窗口、800kW 需量上限的园区配置。
func DefaultConfig() Config {
	loc, _ := time.LoadLocation("Asia/Shanghai")
	return Config{
		GridAssetID:      "grid-1",
		StorageID:        "bess-1",
		DemandLimitKW:    800,
		WindowLength:     15 * time.Minute,
		Location:         loc,
		StartMarginKW:    10,
		ReleaseMarginKW:  30,
		HoldTicks:        2,
		ChargeHeadroomKW: 120,
		ChargeMaxKW:      50,
		StaleGrid:        2 * time.Minute,
		StaleLoad:        2 * time.Minute,
		StaleBESS:        2 * time.Minute,
		Loads: []LoadDef{
			{ID: "hvac-1", Priority: 1, MinKW: 20, RatedKW: 120},
			{ID: "evcp-1", Priority: 2, MinKW: 0, RatedKW: 100},
			{ID: "pump-1", Priority: 3, MinKW: 30, RatedKW: 80},
		},
		Storage:            storage.DefaultConfig(),
		ClockJumpTolerance: 10 * time.Minute,
	}
}

// Command 是下发给设备的控制指令。
type Command struct {
	ID       string
	TS       time.Time
	Target   string  // 资产 ID
	Kind     string  // storage_set_kw | load_limit_kw | load_restore
	ValueKW  float64 // storage: 正充负放；load_limit: 功率上限
	Reason   string
	DedupKey string
}

// LoadState 是单个负载的当前控制状态。
type LoadState struct {
	Def       LoadDef
	Shed      bool
	LimitKW   float64 // 当前下发的上限；满投时为 RatedKW
	Manual    bool
	ReleaseOK int // 恢复条件连续满足计数
}

// Decision 是一次 Tick 的完整结论（输入、判定依据、最终状态），便于日志与测试。
type Decision struct {
	TS              time.Time
	Mode            Mode
	Window          demand.Window
	CurrentDemand   float64
	ProjectedDemand float64
	DemandReliable  bool
	GridStale       bool
	LoadsStale      []string
	BESSStale       bool
	ClockJump       bool

	StorageMode string // discharge|charge|idle
	StorageKW   float64
	StorageCaps storage.Capabilities
	SOC         float64

	ShedLoads  []string
	LoadLimits map[string]float64

	Commands []Command
	Reasons  []string

	PeakNew      bool // 本窗口关闭且刷新了当月峰值
	ClosedWindow *demand.PeakRecord
}

// ManualOverride 是人工强制操作。
type ManualOverride struct {
	TS      time.Time
	Actor   string
	Target  string // 储能 ID 或负载 ID
	Kind    string // storage_force_charge|storage_force_discharge|storage_unlock|load_force_shed|load_force_restore|load_unlock
	ValueKW float64
	Reason  string
}

type manualState struct {
	active  bool
	kind    string
	valueKW float64
	actor   string
	ts      time.Time
	reason  string
}

// Controller 是需量控制+储能协同的核心组件。
type Controller struct {
	cfg     Config
	store   *telemetry.Store
	ledger  *storage.Ledger
	book    *demand.PeakBook
	journal *Journal

	loads     map[string]*LoadState
	loadOrder []string
	manual    map[string]manualState

	lastTick   time.Time
	lastGridTS time.Time
	primed     bool

	lastStorageSig string
	windowClosed   map[string]bool // 已关闭窗口起点 -> 已结算

	// 给储能功率样本去重记账（账本自身按 ID 去重，这里仅保证喂入有序）。
	powerFed map[string]bool

	// replay 模式：重放历史期间不重复产生动作副作用（指令仍计算用于比对）。
	replay bool

	// commandHistory 保留按时间顺序的全部已生效指令（replay 时用于同步物理模型）。
	commandHistory []Command
}

// BeginReplay 在加载历史审计后进入重放模式：
// 重放期间遇到与历史一致的动作只重建状态、不再写审计/不重复下发。
func (c *Controller) BeginReplay(entries []AuditEntry) {
	c.replay = true
	c.journal.SetReplay(true)
	c.journal.PrimeFrom(entries)
}

// EndReplay 结束重放，恢复正常控制。
func (c *Controller) EndReplay() {
	c.replay = false
	c.journal.SetReplay(false)
}

// Replaying 报告是否处于重放模式。
func (c *Controller) Replaying() bool { return c.replay }

// New 创建控制器。journalPath 为空时审计只保留在内存。
func New(cfg Config, store *telemetry.Store, journalPath string) (*Controller, error) {
	if cfg.Location == nil {
		cfg.Location = time.Local
	}
	if cfg.ReleaseMarginKW <= cfg.StartMarginKW {
		return nil, fmt.Errorf("controller: release margin must exceed start margin")
	}
	j, err := NewJournal(journalPath)
	if err != nil {
		return nil, err
	}
	c := &Controller{
		cfg:          cfg,
		store:        store,
		ledger:       storage.NewLedger(cfg.Storage),
		book:         demand.NewPeakBook(cfg.Location),
		journal:      j,
		loads:        make(map[string]*LoadState),
		manual:       make(map[string]manualState),
		windowClosed: make(map[string]bool),
		powerFed:     make(map[string]bool),
	}
	for _, ld := range cfg.Loads {
		c.loads[ld.ID] = &LoadState{Def: ld, LimitKW: ld.RatedKW}
		c.loadOrder = append(c.loadOrder, ld.ID)
	}
	sort.Slice(c.loadOrder, func(i, j int) bool {
		a, b := c.loads[c.loadOrder[i]], c.loads[c.loadOrder[j]]
		if a.Def.Priority != b.Def.Priority {
			return a.Def.Priority < b.Def.Priority
		}
		return a.Def.ID < b.Def.ID
	})
	return c, nil
}

// Journal 暴露审计日志。
func (c *Controller) Journal() *Journal { return c.journal }

// PeakBook 暴露峰值台账。
func (c *Controller) PeakBook() *demand.PeakBook { return c.book }

// Ledger 暴露储能账本。
func (c *Controller) Ledger() *storage.Ledger { return c.ledger }

// ExportState 导出可持久化运行状态（账本能量基线 + 已处理样本 ID）。
func (c *Controller) ExportState() storage.State { return c.ledger.ExportState() }

// RestoreState 恢复运行状态（重启续跑）。
func (c *Controller) RestoreState(st storage.State) { c.ledger.RestoreState(st) }

// LoadSnapshot 是单个负载的持久化状态。
type LoadSnapshot struct {
	ID        string
	Shed      bool
	LimitKW   float64
	Manual    bool
	ReleaseOK int
}

// ExportLoads 导出负载控制状态。
func (c *Controller) ExportLoads() []LoadSnapshot {
	out := make([]LoadSnapshot, 0, len(c.loads))
	for _, id := range c.loadOrder {
		ls := c.loads[id]
		out = append(out, LoadSnapshot{
			ID: ls.Def.ID, Shed: ls.Shed, LimitKW: ls.LimitKW,
			Manual: ls.Manual, ReleaseOK: ls.ReleaseOK,
		})
	}
	return out
}

// RestoreLoads 恢复负载控制状态。
func (c *Controller) RestoreLoads(snaps []LoadSnapshot) {
	for _, sn := range snaps {
		if ls, ok := c.loads[sn.ID]; ok {
			ls.Shed, ls.LimitKW = sn.Shed, sn.LimitKW
			ls.Manual, ls.ReleaseOK = sn.Manual, sn.ReleaseOK
		}
	}
}

// CommandsAtOrBefore 返回 t 时刻（含）前已生效的指令副本。
func (c *Controller) CommandsAtOrBefore(t time.Time) []Command {
	var out []Command
	for _, cmd := range c.commandHistory {
		if !cmd.TS.After(t) {
			out = append(out, cmd)
		}
	}
	return out
}

// LoadStates 返回负载状态副本。
func (c *Controller) LoadStates() map[string]LoadState {
	out := make(map[string]LoadState, len(c.loads))
	for id, s := range c.loads {
		out[id] = *s
	}
	return out
}

// ApplyManual 接受人工强制操作并写审计；后续 Tick 必须尊重该操作直到解锁。
func (c *Controller) ApplyManual(m ManualOverride) {
	c.manual[m.Target] = manualState{
		active: true, kind: m.Kind, valueKW: m.ValueKW,
		actor: m.Actor, ts: m.TS, reason: m.Reason,
	}
	if ls, ok := c.loads[m.Target]; ok {
		switch m.Kind {
		case "load_force_shed":
			ls.Shed, ls.Manual = true, true
			ls.LimitKW = ls.Def.MinKW
		case "load_force_restore":
			ls.Shed, ls.Manual = false, true
			ls.LimitKW = ls.Def.RatedKW
		case "load_unlock":
			ls.Manual = false
		}
	}
	c.journal.Append(AuditEntry{
		TS: m.TS, Category: "manual", Reason: m.Kind, Target: m.Target,
		Actor: m.Actor, Value: m.ValueKW, Detail: m.Reason,
		DedupKey: fmt.Sprintf("manual|%s|%s|%d", m.Target, m.Kind, m.TS.UnixNano()),
	})
}

// hasManual 报告是否存在未解锁的人工操作。
func (c *Controller) hasManual() (bool, bool) {
	storageManual, loadManual := false, false
	for t, ms := range c.manual {
		if !ms.active {
			continue
		}
		if t == c.cfg.StorageID {
			storageManual = true
		} else if _, ok := c.loads[t]; ok {
			loadManual = true
		}
	}
	return storageManual, loadManual
}
