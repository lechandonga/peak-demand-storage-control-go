package telemetry

import (
	"fmt"
	"strings"
	"time"
)

// Kind 是遥测样本的物理量类型。
type Kind string

const (
	KindGridPower      Kind = "grid_power_kw"    // 进线有功功率，kW，正=从电网取电
	KindStoragePower   Kind = "storage_power_kw" // 储能交流侧功率，kW，正=充电，负=放电
	KindStorageSOC     Kind = "storage_soc"      // 储能荷电状态，0~1
	KindStorageTemp    Kind = "storage_temp_c"   // 储能电芯温度，℃
	KindLoadPower      Kind = "load_power_kw"    // 负载实时功率，kW，>=0
	KindLoadSettableKW Kind = "load_settable_kw" // 负载当前可调上限，kW
)

// Source 标识样本来源。
type Source string

const (
	SourceGrid    Source = "grid"
	SourceStorage Source = "storage"
	SourceLoad    Source = "load"
)

// Sample 是统一的遥测样本。
// ID 是样本唯一标识（网关序号/内容摘要），用于乱序、重复、回放去重；
// Checksum 为可选的样本校验和（CRC32/IEEE，hex），为空表示不校验。
type Sample struct {
	ID       string
	Source   Source
	AssetID  string
	Kind     Kind
	TS       time.Time
	Value    float64
	Checksum string
}

func (s Sample) key() string {
	return string(s.Source) + "|" + s.AssetID + "|" + string(s.Kind)
}

// Reason 是可区分的拒绝原因。
type Reason string

const (
	AcceptOK           Reason = ""
	RejectDuplicate    Reason = "duplicate"     // 同一 ID 已收到
	RejectConflict     Reason = "conflict"      // 同资产同类型同时间戳但取值不一致
	RejectStale        Reason = "stale"         // 落后最新已接受样本太多
	RejectFuture       Reason = "future"        // 时间戳超前当前时间太多
	RejectExpired      Reason = "expired"       // 时间戳早于保留窗口起点
	RejectMissingField Reason = "missing_field" // 必填字段缺失
	RejectOutOfRange   Reason = "out_of_range"  // 取值明显越界
	RejectJump         Reason = "jump"          // 与相邻样本相比变化率异常
	RejectChecksum     Reason = "checksum"      // 校验和不匹配
)

// Reject 记录一条样本被拒绝的原因与解释。
type Reject struct {
	Sample Sample
	Reason Reason
	Detail string
}

func (r Reject) Error() string {
	return fmt.Sprintf("telemetry reject: sample=%s source=%s asset=%s kind=%s ts=%s reason=%s detail=%s",
		r.Sample.ID, r.Sample.Source, r.Sample.AssetID, r.Sample.Kind,
		r.Sample.TS.Format(time.RFC3339), r.Reason, r.Detail)
}

func (s Sample) validateIdentity() *Reject {
	switch {
	case strings.TrimSpace(s.ID) == "":
		return &Reject{Sample: s, Reason: RejectMissingField, Detail: "empty sample id"}
	case s.Source == "":
		return &Reject{Sample: s, Reason: RejectMissingField, Detail: "empty source"}
	case strings.TrimSpace(s.AssetID) == "":
		return &Reject{Sample: s, Reason: RejectMissingField, Detail: "empty asset id"}
	case s.Kind == "":
		return &Reject{Sample: s, Reason: RejectMissingField, Detail: "empty kind"}
	case s.TS.IsZero():
		return &Reject{Sample: s, Reason: RejectMissingField, Detail: "zero timestamp"}
	}
	return nil
}
