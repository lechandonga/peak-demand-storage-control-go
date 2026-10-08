package sim

import (
	"time"

	"github.com/lechandonga/peak-demand-storage-control-go/controller"
	"github.com/lechandonga/peak-demand-storage-control-go/telemetry"
)

// StepResult 是单步仿真结果。
type StepResult struct {
	Step     int
	TS       time.Time
	Samples  []telemetry.Sample
	Rejects  []telemetry.Reject
	Decision controller.Decision
}

// Runner 串联遥测接入、控制器决策与物理执行。
type Runner struct {
	plant *Plant
	store *telemetry.Store
	ctrl  *controller.Controller
}

// PlantTime 返回第 step 步时间戳。
func (r *Runner) PlantTime(step int) time.Time { return r.plant.TimeAt(step) }

// PlantSamples 生成第 step 步样本但不接入。
func (r *Runner) PlantSamples(step int) []telemetry.Sample {
	return r.plant.SamplesAt(step)
}

// Ingest 接入一条样本。
func (r *Runner) Ingest(s telemetry.Sample, now time.Time) *telemetry.Reject {
	return r.store.Ingest(s, now)
}

// IngestBatch 接入一批样本。
func (r *Runner) IngestBatch(ss []telemetry.Sample, now time.Time) []telemetry.Reject {
	return r.store.IngestBatch(ss, now)
}

// TickOnly 只做决策并执行，不生成/接入样本（用于自定义接入方式测试）。
func (r *Runner) TickOnly(step int, ts time.Time) StepResult {
	d := r.ctrl.Tick(ts)
	// replay 模式下 Commands 已被清空，但控制状态仍可从 LoadStates 读出；
	// 物理模型需要与控制器状态对齐，以保证后续窗口的进线功率与首轮一致。
	for _, cmd := range d.Commands {
		switch cmd.Kind {
		case "storage_set_kw":
			r.plant.RecordStep(step, cmd.ValueKW)
		case "load_limit_kw":
			r.plant.SetLoadLimit(cmd.Target, cmd.ValueKW)
		case "load_restore":
			r.plant.RestoreLoad(cmd.Target, cmd.ValueKW)
		}
	}

	// replay：用控制器内存状态同步物理模型（储能最后执行功率/负载上限）。
	if r.ctrl.Replaying() {
		r.syncPlantFromController(step, ts)
	} else if len(d.Commands) == 0 {
		r.plant.RecordStep(step, r.plant.currentStorageKW())
	}
	return StepResult{Step: step, TS: ts, Decision: d}
}

// syncPlantFromController 用截至 ts 的历史指令对齐物理模型，
// 保证重启重放下后续窗口的进线功率与首轮完全一致。
func (r *Runner) syncPlantFromController(step int, ts time.Time) {
	for _, cmd := range r.ctrl.CommandsAtOrBefore(ts) {
		switch cmd.Kind {
		case "storage_set_kw":
			// 指令在该时刻起生效，功率持续到下一条储能指令。
			r.plant.SetStoragePower(cmd.ValueKW)
		case "load_limit_kw":
			r.plant.SetLoadLimit(cmd.Target, cmd.ValueKW)
		case "load_restore":
			r.plant.RestoreLoad(cmd.Target, cmd.ValueKW)
		}
	}
	r.plant.RecordStep(step, r.plant.currentStorageKW())
}

// NewRunner 创建运行器。
func NewRunner(plant *Plant, store *telemetry.Store, ctrl *controller.Controller) *Runner {
	return &Runner{plant: plant, store: store, ctrl: ctrl}
}

// RunStep 执行一步：生成样本→乱序安全接入→决策→执行指令。
// shuffle 非空时可重排样本顺序，用于验证乱序不影响结论。
func (r *Runner) RunStep(step int, shuffle func([]telemetry.Sample)) StepResult {
	ts := r.plant.TimeAt(step)
	samples := r.plant.SamplesAt(step)
	if shuffle != nil {
		shuffle(samples)
	}
	rejects := r.store.IngestBatch(samples, ts.Add(time.Second))

	d := r.ctrl.Tick(ts)

	// 执行指令回灌物理模型。
	for _, cmd := range d.Commands {
		switch cmd.Kind {
		case "storage_set_kw":
			r.plant.RecordStep(step, cmd.ValueKW)
		case "load_limit_kw":
			r.plant.SetLoadLimit(cmd.Target, cmd.ValueKW)
		case "load_restore":
			r.plant.RestoreLoad(cmd.Target, cmd.ValueKW)
		}
	}
	if len(d.Commands) == 0 {
		r.plant.RecordStep(step, r.plant.currentStorageKW())
	}

	return StepResult{Step: step, TS: ts, Samples: samples,
		Rejects: rejects, Decision: d}
}

func (p *Plant) currentStorageKW() float64 { return p.storageKW }
