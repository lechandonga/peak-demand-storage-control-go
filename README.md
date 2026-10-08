# 园区需量控制与储能协同组件（Go）

纯本地、零外部依赖的园区进线需量控制组件：接入进线、储能、可调负载的遥测，
围绕合同需量上限做固定窗口滚动控制，优先用储能放电顶峰值、再按优先级降载，
低谷时在不抬高需量的前提下给储能充电；异常时自动进入“宁可多限电”的保守模式。

## 目录结构

| 路径 | 说明 |
| --- | --- |
| `internal/demand` | 核心组件：遥测校验/去重、需量窗口、储能模型、控制引擎、审计、持久化 |
| `sim` | 本地园区模拟器（进线 + 1 MWh 储能 + 三类可调负载） |
| `cmd/simulate` | 端到端演示：高峰、黑边、失联、恢复、重启续跑 |

## 需量口径与窗口规则

- **固定窗口**：默认 15 分钟，按时间戳整除窗口宽度对齐（`WindowBook.WindowStart`），
  不是滑动窗口；因此跨天、跨月只是窗口自然落入不同日历标签，统计口径完全一致。
- **需量定义**：窗口内进线功率的时间加权平均（sample-and-hold 积分）：
  `需求 = Σ(功率 × 持续时长) / Σ时长`，单位 kW。
- **历史峰值**：每个窗口结束时结算一次，只在完整窗口之间比较并保留最高值；
  当前进行中的窗口不覆盖历史峰值。
- **日历标签**：每个完成窗口携带 `Day(YYYY-MM-DD)` 与 `Month(YYYY-MM)`，
  使用配置时区，跨天/跨月边界无需特判。
- **缺口处理**：窗口边界跨越（包括遥测中断）时，已过去的整窗按最后保持功率结算，
  保证一个窗口都不丢、也不重复累计。

### 风险判断（是否会刷新峰值 / 突破合同）

- **当期投影**：控制投影取 `max(窗口当前均值, 最新一帧瞬时进线功率)`，
  既反映当期累计需量，也能对突发尖峰立即动作；窗口均值仍是需量/峰值的唯一结算口径。
- **防御目标**：`目标 = min(合同上限 ContractKW, 历史峰值)`——没有峰值记录时
  守合同上限，已有峰值后同时守“不刷新峰值”。
- **提前介入**：投影进入 `目标 - RiskLeadKW` 即动作，不等越限；
  原因为 `peak_projection_at_risk` 或 `cap_projection_at_risk`，可区分。

## 储能调度与效率模型

- 放电为正、充电为负。充/放各有单向效率 `ChargeEff`、`DischargeEff`：
  - 放电 `P` kW 持续 `dt`：`SOC 下降 = (P/DischargeEff)·dt / 容量`
  - 充电 `P` kW 持续 `dt`：`SOC 上升 = (P·ChargeEff)·dt / 容量`
- **备用下限**：`BackupSOC`（默认 20%）以下不允许主动放电；
  同时按“本 tick 内不得穿越备用下限”的能量上限对放电功率做裁剪。
- **温度降额**：高于 `TempHot` / 低于 `TempCold` 线性降功率，
  达到 `TempHotCut` / `TempColdCut` 时停机（`temperature_derated` /
  `temperature_shutdown`）。
- **SOC/能量对账**：接入侧可注册物理模型（`Store.SetBatteryReconcile`），
  每帧校验“上报 SOC 变化”是否与“功率 × 时间 × 效率”一致（带测量噪声容差），
  不一致或能量计数器回退一律以 `soc_energy_mismatch` 拒绝，不参与决策。
- **低谷充电**：投影 ≤ `ValleyThresholdKW` 时，按
  `目标 - ChargeHeadroomKW - 投影` 的余量、受充电功率与剩余电池容量双重约束充电，
  原因 `valley_surplus_charge`，保证充电本身不把需量顶回目标区间。

## 降载优先级与稳定条件

- 有风险时顺序固定：**储能先放电顶住 → 仍有缺口再降载**。
- 负载按 `Priority` 升序削减（数字小先切），支持降到 `MinKW` 保底运行、
  仍不够时全切（允许功率 0）；动作原因统一为 `storage_at_backup_or_limited`，
  表明“储能因备用下限/温度/功率受限无法再承担”。
- **防抖动滞回**：
  - 投入带：`目标 - RiskLeadKW`；恢复带：`目标 - RestoreLagKW`（恢复更松）；
  - 恢复投入还要连续 `MinStableTicks` 个健康 tick；
  - 恢复按优先级逆序（后切的先恢复），每 tick 只按剩余空间步进，不突跳。
- 指令按设备做“无变化不重发”（0.1 kW 量化），真实变化绝不丢。

## 人工强制与审计

- `Controller.ApplyOverride(operator, device, kind, kw, reason)` 强制设备指令，
  每个 tick 都优先尊重，直到 `ReleaseOverride`；自动指令对该设备被旁路。
- 每次强制生成一条不可变审计记录（操作员、时间、设备、指令、原因、引用号），
  随状态持久化；强制动作以 `manual_override` 原因下发并携带 `AuditRef`。

## 故障保守策略

以下情况进入保守模式（`FaultState` 带原因与起始时间）：

- 进线遥测缺失或超过 `StaleAfter` 未更新；
- 所有储能遥测失联；
- 时钟回退，或跳变超过 `MaxTimeJump`（此时重建窗口累加器，历史峰值保留）。

保守策略：**储能置空闲、所有已知可调负载全切**（哪怕其遥测已 stale，
失联正是要保守的原因），宁多限电不突破上限。遥测恢复后不立刻放开：
先保持保守输出 `MinStableTicks` 个健康 tick（恢复期 `recovery hold x/y`），
再一次性回到正常控制并发出唯一的 `fault_cleared` 标记，之后按稳定恢复条件
逐步投回负载，避免状态跳变、重复动作或漏动作。

## 遥测接入：拒绝原因（不会静默参与决策）

`Store.Ingest` 接受乱序、重复、延迟到达，按 `(At, ID)` 稳定排序后逐帧校验，
拒绝结果逐条返回：

| 原因 | 含义 |
| --- | --- |
| `unknown_source` | 来源不是 grid/battery/load |
| `missing_id` | 缺样本 ID（无法幂等去重） |
| `zero_timestamp` / `future_timestamp` / `too_old` | 时间异常 |
| `field_not_finite` | NaN/Inf |
| `power_out_of_range` / `soc_out_of_range` / `temperature_out_of_range` | 越界 |
| `checksum_mismatch` | 帧校验失败（`Sample.Checksum = ChecksumOf(s)`） |
| `duplicate_id` | 同一 ID 重放（批内/重启后同一批数据） |
| `stale_replay` | 同一 ID 携带不同时间戳（疑似伪造/错位重放） |
| `soc_energy_mismatch` | SOC 变化与充放电能量（含损耗）对不上，或能量计数回退 |

已接受样本的 ID 集合随快照持久化：**重启后重放同一批数据全部判为重复，
不重复累计；进行中窗口、峰值、设定值与“已发指令指纹”一并恢复，不丢动作。**

并发：`Ingest`/`Decide`/`ApplyOverride`/`Save`/`Load` 均持有同一把锁串行化，
同一批输入从任意入口或并发调用，结论（设定值 + `Decision.Fingerprint`）唯一。

## 本地验证

要求 Go 1.25+（本机用 Go 1.26 验证）。无需任何外部服务。

```bash
# 单元测试（覆盖乱序/重复/越界/对账、阈值与滞回、温度受限、
# 失联恢复、强制审计、跨天、重启续跑、并发）
go test ./...

# 竞态检测（并发用例必须通过）
CGO_ENABLED=1 go test -race -count=1 ./...

# 端到端演示（快照落盘后再跑一次可观察“重启续跑 + 重放去重”）
go run ./cmd/simulate -state /tmp/pd-state.json
go run ./cmd/simulate -state /tmp/pd-state.json   # 第二次：旧帧全部 duplicate
```

测试日志会打印每个 tick 的 `INPUT`（来源/设备/功率/SOC/温度）、
`REJECT`（样本 ID + 可区分原因）、`DECIDE`（当期需量/投影/峰值/剩余时间/故障）
与 `EMIT`（动作 + 设备 + 功率 + 原因），可直接看出输入、判定依据和最终状态。

## 最小用法

```go
now := clock.Now // 生产用 time.Now，测试可注入固定时钟
store := demand.NewStore(demand.DefaultLimits(), now)
store.SetBatteryReconcile("BESS-1", demand.ReconcileConfig{
    CapacityWh: 1_000_000, ChargeEff: .95, DischargeEff: .95, SOCNoisePct: 1,
})
ctl := demand.NewController(cfg, store, now)

res, decision := ctl.Ingest([]demand.Sample{grid, battery, loadA, loadB})
for _, r := range res.Rejected {       // 区分拒绝原因
    log.Print(r.Sample.ID, r.Reason, r.Detail)
}
for _, a := range decision.EmittedActions { // 只含真实变化
    dispatch(a)
}
_ = ctl.Save("state.json")            // 重启前快照；启动时 ctl.Load 即可续跑
```
