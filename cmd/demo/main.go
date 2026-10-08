// 本地演示：不依赖任何外部服务，完整跑通
// 乱序/重复遥测、需量越限、储能放电、负载削减、失联保守与恢复。
package main

import (
	"fmt"
	"math/rand"
	"time"

	"github.com/lechandonga/peak-demand-storage-control-go/controller"
	"github.com/lechandonga/peak-demand-storage-control-go/sim"
	"github.com/lechandonga/peak-demand-storage-control-go/telemetry"
)

func main() {
	loc, _ := time.LoadLocation("Asia/Shanghai")
	start := time.Date(2026, 9, 30, 13, 45, 0, 0, loc) // 窗口 13:45 起

	cfg := controller.DefaultConfig()
	loadIDs := []string{"hvac-1", "evcp-1", "pump-1"}

	sc := sim.Scenario{
		Start: start, Step: 30 * time.Second, Steps: 90, // 覆盖 3 个 15min 窗口
		BaseKW: 560, PeakKW: 880,
		PeakAt: 4, PeakFor: 40,
		LoadKW: map[string]float64{
			"hvac-1": 120, "evcp-1": 100, "pump-1": 80,
		},
		InitialSOC:   0.8,
		BlackoutFrom: 50, BlackoutFor: 6, // 进线失联 3 分钟
	}

	store := telemetry.NewStore(telemetry.DefaultConfig())
	ctrl, err := controller.New(cfg, store, "")
	if err != nil {
		panic(err)
	}
	plant := sim.NewPlant(sc, cfg.GridAssetID, cfg.StorageID, loadIDs)
	runner := sim.NewRunner(plant, store, ctrl)

	rng := rand.New(rand.NewSource(7))
	for step := 0; step < sc.Steps; step++ {
		res := runner.RunStep(step, func(s []telemetry.Sample) {
			rng.Shuffle(len(s), func(i, j int) { s[i], s[j] = s[j], s[i] })
		})
		d := res.Decision
		if step%4 == 0 || len(d.Commands) > 0 || d.Mode == controller.ModeConservative {
			fmt.Printf("[step %02d %s] mode=%-12s cur=%6.1f proj=%6.1f soc=%.2f "+
				"bess=%-9s shed=%v cmds=%d\n",
				step, d.TS.Format("15:04:05"), d.Mode,
				d.CurrentDemand, d.ProjectedDemand, d.SOC,
				fmt.Sprintf("%s%.0f", d.StorageMode, d.StorageKW),
				d.ShedLoads, len(d.Commands))
			for _, r := range res.Rejects {
				fmt.Printf("    reject: %s (%s)\n", r.Sample.ID, r.Reason)
			}
			for _, rs := range d.Reasons {
				if len(d.Commands) > 0 || d.Mode == controller.ModeConservative {
					fmt.Printf("    why: %s\n", rs)
				}
			}
		}

		// 演示人工强制：第 20 步强制储能以 20kW 充电，持续到第 28 步解锁。
		if step == 20 {
			ctrl.ApplyManual(controller.ManualOverride{
				TS: res.TS, Actor: "operator.li", Target: cfg.StorageID,
				Kind: "storage_force_charge", ValueKW: 20,
				Reason: "manual valley-charge for maintenance",
			})
		}
		if step == 28 {
			ctrl.ApplyManual(controller.ManualOverride{
				TS: res.TS, Actor: "operator.li", Target: cfg.StorageID,
				Kind: "storage_unlock", Reason: "manual window ended",
			})
		}
	}

	fmt.Println("\n== 月度峰值 ==")
	for _, mp := range ctrl.PeakBook().History() {
		fmt.Printf("%s: %.2f kW @ window %s\n", mp.Month, mp.Record.DemandKW,
			mp.Record.WindowStart.Format("01-02 15:04"))
	}

	fmt.Println("\n== 审计记录（动作/人工/故障） ==")
	for _, e := range ctrl.Journal().Entries() {
		if e.Category == "command" || e.Category == "manual" || e.Category == "fault" {
			actor := ""
			if e.Actor != "" {
				actor = " by " + e.Actor
			}
			fmt.Printf("%s %-8s %-24s target=%-7s %.1f kW%s\n",
				e.TS.Format("15:04:05"), e.Category, e.Reason, e.Target, e.Value, actor)
		}
	}
}
