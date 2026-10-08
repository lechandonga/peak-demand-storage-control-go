package sim

import (
	"math/rand"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/lechandonga/peak-demand-storage-control-go/controller"
	"github.com/lechandonga/peak-demand-storage-control-go/storage"
	"github.com/lechandonga/peak-demand-storage-control-go/telemetry"
)

// rand7 返回固定种子随机源（与场景 2 乱序效果一致）。
func rand7() *rand.Rand { return rand.New(rand.NewSource(7)) }

// 场景 7：人工强制负载投入必须被尊重，且留下带操作人的审计记录。
func TestManualOverrideRespectedAndAudited(t *testing.T) {
	sc := peakScenario()
	r, _, ctrl := newHarness(t, sc, "")

	for i := 0; i < 14; i++ {
		ts := r.PlantTime(i)
		if i == 4 {
			ctrl.ApplyManual(controller.ManualOverride{
				TS: ts, Actor: "operator.wang", Target: "hvac-1",
				Kind: "load_force_restore", ValueKW: 120,
				Reason: "production line must keep running",
			})
		}
		res := r.RunStep(i, nil)
		logStep(t, res)
		if i >= 4 {
			st := ctrl.LoadStates()["hvac-1"]
			if st.Shed || !st.Manual || st.LimitKW != 120 {
				t.Fatalf("manual restore not respected at step %d: %+v", i, st)
			}
		}
	}

	var manualEntry *struct {
		reason, actor, target string
	}
	for _, e := range ctrl.Journal().Entries() {
		if e.Category == "manual" && e.Target == "hvac-1" {
			manualEntry = &struct{ reason, actor, target string }{
				e.Reason, e.Actor, e.Target}
		}
	}
	if manualEntry == nil {
		t.Fatal("manual override not audited")
	}
	if manualEntry.actor != "operator.wang" || manualEntry.reason != "load_force_restore" {
		t.Fatalf("manual audit wrong: %+v", manualEntry)
	}

	// 解锁后自动控制可再次削减（后续高峰仍在）。
	ctrl.ApplyManual(controller.ManualOverride{
		TS: r.PlantTime(14), Actor: "operator.wang", Target: "hvac-1",
		Kind: "load_unlock", Reason: "manual window ended",
	})
	autoShed := false
	for i := 14; i < 24; i++ {
		res := r.RunStep(i, nil)
		for _, cmd := range res.Decision.Commands {
			if cmd.Target == "hvac-1" && cmd.Kind == "load_limit_kw" {
				autoShed = true
			}
		}
	}
	if !autoShed {
		t.Fatal("auto control did not resume shedding after unlock")
	}
}

// 场景 8：重启续跑。同一批数据用新控制器重放：
// 储能电量不重复累计、动作不重复下发、峰值不重复抬升。
func TestRestartReplayNoDoubleCountOrDuplicateActions(t *testing.T) {
	journalPath := tmpJournal(t)

	sc := peakScenario()

	// 第一次运行。
	var firstSOC float64
	var firstSig []string
	var firstPeak float64
	var firstState storage.State
	func() {
		r, _, ctrl := newHarness(t, sc, journalPath)
		res := runAll(t, r, sc.Steps, 3)
		firstSig = commandSig(res)
		firstSOC = ctrl.Ledger().Snapshot().SOC
		firstState = ctrl.ExportState()
		if h := ctrl.PeakBook().History(); len(h) == 1 {
			firstPeak = h[0].Record.DemandKW
		}
	}()

	// 审计文件已落盘。
	data, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Fatal("journal not persisted")
	}

	// 重启：新 store + 新 ledger，回放同一批遥测与同一批 tick；
	// 审计日志从文件续上（去重索引重建）。
	r2, store2, ctrl2 := newHarness(t, sc, journalPath)
	// 回放历史审计以建立动作去重水位（模拟进程恢复后加载日志）。
	entries, err := controller.LoadJournal(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	fileLinesBefore := len(entries)
	ctrl2.RestoreState(firstState)
	ctrl2.BeginReplay(entries)

	var replayCommands []string
	for i := 0; i < sc.Steps; i++ {
		ts := r2.PlantTime(i)
		// 乱序重放同一批样本：全部应被识别（首放）或无副作用。
		samples := r2.PlantSamples(i)
		rng := rand7()
		rng.Shuffle(len(samples), func(a, b int) { samples[a], samples[b] = samples[b], samples[a] })
		if rs := store2.IngestBatch(samples, ts.Add(time.Second)); len(rs) != 0 {
			t.Fatalf("fresh store replay should accept all, got rejects: %d", len(rs))
		}
		d := r2.TickOnly(i, ts).Decision // replay 模式下 Commands 必为空
		if len(d.Commands) != 0 {
			t.Fatalf("replay must not emit new commands at step %d: %+v", i, d.Commands)
		}
		for _, c := range d.Commands {
			replayCommands = append(replayCommands,
				string(c.Kind)+"|"+c.Target+"|"+c.Reason)
		}
	}
	ctrl2.EndReplay()

	// 能量账本重放后 SOC 与首次一致（没有重复累计，也没有丢失积分）。
	secondSOC := ctrl2.Ledger().Snapshot().SOC
	if d := firstSOC - secondSOC; d > 1e-6 || d < -1e-6 {
		t.Fatalf("SOC diverged after replay: first=%.6f second=%.6f", firstSOC, secondSOC)
	}
	t.Logf("SOC after first=%.6f, after replay=%.6f", firstSOC, secondSOC)

	// 峰值一致。
	h := ctrl2.PeakBook().History()
	if len(h) != 1 || h[0].Record.DemandKW != firstPeak {
		t.Fatalf("peak diverged after replay: %+v want %.2f", h, firstPeak)
	}

	// 关键：重放不得让审计文件追加任何新行（动作不重复）。
	entriesAfter, _ := controller.LoadJournal(journalPath)
	if len(entriesAfter) != fileLinesBefore {
		t.Fatalf("journal file changed during replay: before=%d after=%d",
			fileLinesBefore, len(entriesAfter))
	}
	if len(firstSig) == 0 {
		t.Fatal("first run produced no commands")
	}
	t.Logf("first-run commands=%d, replay new commands=%d, journal lines=%d",
		len(firstSig), len(replayCommands), fileLinesBefore)

	// 同进程内重放 tick（相同 now、相同输入）：储能设定值与负载指令不得重复下发。
	r3, _, _ := newHarness(t, sc, "")
	res3 := runAll(t, r3, sc.Steps, 3)
	secondSig := commandSig(res3)
	if len(secondSig) != len(firstSig) {
		t.Fatalf("independent rerun command count differs: %d vs %d",
			len(secondSig), len(firstSig))
	}
	for i := range firstSig {
		if firstSig[i] != secondSig[i] {
			t.Fatalf("rerun not deterministic at %d: %s vs %s",
				i, firstSig[i], secondSig[i])
		}
	}
}

// 场景 9：拒绝原因可区分（坏校验和/越界/重复/乱序过期），且坏样本不参与决策。
func TestBadSamplesRejectedWithDistinctReasons(t *testing.T) {
	sc := peakScenario()
	r, store, _ := newHarness(t, sc, "")
	ts := r.PlantTime(0)

	good := r.PlantSamples(0)
	if rs := store.IngestBatch(good, ts.Add(time.Second)); len(rs) != 0 {
		t.Fatalf("good batch rejected: %+v", rs)
	}

	// 重复回放整批。
	dup := store.IngestBatch(r.PlantSamples(0), ts.Add(time.Second))
	dupKinds := map[string]bool{}
	for _, x := range dup {
		dupKinds[string(x.Reason)] = true
	}
	if !dupKinds["duplicate"] {
		t.Fatalf("replay not classified duplicate: %+v", dupKinds)
	}

	// 构造各种坏样本（使用不存在的 ID）。
	bad := []telemetry.Sample{
		telemetry.Sample{ID: "bad-oor", Source: telemetry.SourceGrid,
			AssetID: "grid-1", Kind: telemetry.KindGridPower, TS: ts.Add(2 * time.Second),
			Value: 9e9, Checksum: ""},
	}
	for i := range bad {
		bad[i].Checksum = telemetry.Checksum(bad[i])
	}
	rs := store.IngestBatch(bad, ts.Add(3*time.Second))
	if len(rs) != 1 || rs[0].Reason != telemetry.RejectOutOfRange {
		t.Fatalf("out-of-range classification wrong: %+v", rs)
	}

	// 坏校验和。
	ck := telemetry.Sample{ID: "bad-ck", Source: telemetry.SourceGrid,
		AssetID: "grid-1", Kind: telemetry.KindGridPower, TS: ts.Add(3 * time.Second),
		Value: 600, Checksum: "00000000"}
	if rj := store.Ingest(ck, ts.Add(3*time.Second)); rj == nil ||
		rj.Reason != telemetry.RejectChecksum {
		t.Fatalf("checksum classification wrong: %v", rj)
	}

	// 过期样本（24h 之前）。
	old := telemetry.Sample{ID: "bad-old", Source: telemetry.SourceGrid,
		AssetID: "grid-1", Kind: telemetry.KindGridPower, TS: ts.Add(-25 * time.Hour),
		Value: 600}
	old.Checksum = telemetry.Checksum(old)
	if rj := store.Ingest(old, ts.Add(3*time.Second)); rj == nil ||
		rj.Reason != telemetry.RejectExpired {
		t.Fatalf("expired classification wrong: %v", rj)
	}

	// 决策仍只基于好样本：首 tick 无峰值异常动作。
	d := r.TickOnly(0, ts).Decision
	if d.Mode == controller.ModeConservative {
		t.Fatalf("rejected samples must not poison decision: mode=%s", d.Mode)
	}
}

// 场景 10：低谷充电——负荷低、余量足时允许充电，SOC 上升。
func TestValleyCharging(t *testing.T) {
	sc := Scenario{
		Start: testStart(), Step: 30 * time.Second, Steps: 12,
		BaseKW: 300, PeakKW: 300, PeakAt: 100, PeakFor: 0,
		LoadKW: map[string]float64{
			"hvac-1": 0, "evcp-1": 0, "pump-1": 0,
		},
		InitialSOC: 0.3,
	}
	r, _, ctrl := newHarness(t, sc, "")
	res := runAll(t, r, sc.Steps, 0)

	sawCharge := false
	for _, x := range res {
		for _, cmd := range x.Decision.Commands {
			if cmd.Kind == "storage_set_kw" && cmd.ValueKW > 0 &&
				strings.Contains(cmd.Reason, "valley") {
				sawCharge = true
			}
		}
	}
	if !sawCharge {
		t.Fatal("expected valley charging command")
	}
	if ctrl.Ledger().Snapshot().SOC <= 0.3 {
		t.Fatalf("SOC did not rise during valley charge: %.4f",
			ctrl.Ledger().Snapshot().SOC)
	}
}
