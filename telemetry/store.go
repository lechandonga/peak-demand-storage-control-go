package telemetry

import (
	"fmt"
	"hash/crc32"
	"math"
	"sort"
	"time"
)

// Limits 定义某类遥测的取值边界与最大变化率（绝对值每秒）。
// MaxRate<=0 表示不做跳变检查。
type Limits struct {
	Min     float64
	Max     float64
	MaxRate float64
}

// Config 是 Store 的校验配置。
type Config struct {
	// MaxFuture 允许样本时间戳领先"现在"的最大幅度（时钟偏差容忍）。
	MaxFuture time.Duration
	// MaxStale 允许乱序样本落后最新已接受样本的最大幅度。
	MaxStale time.Duration
	// Retention 历史保留时长（拒绝早于 now-Retention 的样本）。
	Retention time.Duration
	// Limits 按 Kind 配置取值范围和变化率。
	Limits map[Kind]Limits
}

// DefaultConfig 给出园区场景下的保守默认边界。
func DefaultConfig() Config {
	return Config{
		MaxFuture: 5 * time.Minute,
		MaxStale:  10 * time.Minute,
		Retention: 24 * time.Hour,
		Limits: map[Kind]Limits{
			KindGridPower:      {Min: -1e4, Max: 1e5, MaxRate: 100}, // kW/s
			KindStoragePower:   {Min: -1e4, Max: 1e4, MaxRate: 100},
			KindStorageSOC:     {Min: 0, Max: 1, MaxRate: 0.05},
			KindStorageTemp:    {Min: -40, Max: 100, MaxRate: 5},
			KindLoadPower:      {Min: 0, Max: 1e5, MaxRate: 200},
			KindLoadSettableKW: {Min: 0, Max: 1e5, MaxRate: 0},
		},
	}
}

type point struct {
	ts    time.Time
	value float64
	id    string
}

type series struct {
	points []point // 按时间戳升序
	latest time.Time
}

// Store 维护多来源遥测状态。所有方法可并发调用；
// 同一批输入无论从哪个入口、以什么并发方式提交，接受/拒绝结论唯一。
type Store struct {
	cfg Config

	mu      chan struct{}
	seriesM map[string]*series
	idIndex map[string]Sample // 样本 ID -> 首次处理时的原始样本
}

// NewStore 创建遥测存储。
func NewStore(cfg Config) *Store {
	mu := make(chan struct{}, 1)
	mu <- struct{}{}
	return &Store{
		cfg:     cfg,
		mu:      mu,
		seriesM: make(map[string]*series),
		idIndex: make(map[string]Sample),
	}
}

func (s *Store) lock()   { <-s.mu }
func (s *Store) unlock() { s.mu <- struct{}{} }

// Ingest 提交一条样本：合法则纳入状态维护，非法返回带明确原因的 Reject。
func (s *Store) Ingest(sm Sample, now time.Time) *Reject {
	if r := sm.validateIdentity(); r != nil {
		return r
	}

	s.lock()
	defer s.unlock()

	// 重复样本：ID 已出现过时结论与第一次一致，且不产生任何状态变化。
	if first, ok := s.idIndex[sm.ID]; ok {
		if samplesEqual(first, sm) {
			return &Reject{Sample: sm, Reason: RejectDuplicate,
				Detail: "sample id already processed"}
		}
		return &Reject{Sample: sm, Reason: RejectConflict,
			Detail: fmt.Sprintf("same id with different payload: first ts=%s value=%v",
				first.TS.Format(time.RFC3339), first.Value)}
	}

	if r := s.validateLocked(sm, now); r != nil {
		s.idIndex[sm.ID] = sm
		return r
	}

	s.idIndex[sm.ID] = sm
	s.upsertLocked(sm)
	return nil
}

// IngestBatch 批量提交。批内乱序样本先按时间排序再做相邻跳变检查，
// 因此整批乱序提交与逐条提交、并发分片提交的结论一致。
func (s *Store) IngestBatch(samples []Sample, now time.Time) []Reject {
	s.lock()
	defer s.unlock()

	var rejects []Reject
	pending := make(map[string][]Sample)

	for _, sm := range samples {
		if r := sm.validateIdentity(); r != nil {
			rejects = append(rejects, *r)
			continue
		}
		if first, ok := s.idIndex[sm.ID]; ok {
			if samplesEqual(first, sm) {
				rejects = append(rejects, Reject{Sample: sm, Reason: RejectDuplicate,
					Detail: "sample id already processed"})
			} else {
				rejects = append(rejects, Reject{Sample: sm, Reason: RejectConflict,
					Detail: "same id with different payload"})
			}
			continue
		}
		if r := s.validateLocked(sm, now); r != nil {
			s.idIndex[sm.ID] = sm
			rejects = append(rejects, *r)
			continue
		}
		pending[sm.key()] = append(pending[sm.key()], sm)
	}

	keys := make([]string, 0, len(pending))
	for k := range pending {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		pts := pending[k]
		sort.Slice(pts, func(i, j int) bool { return pts[i].TS.Before(pts[j].TS) })
		for _, sm := range pts {
			if r := s.jumpCheckLocked(sm); r != nil {
				s.idIndex[sm.ID] = sm
				rejects = append(rejects, *r)
				continue
			}
			s.idIndex[sm.ID] = sm
			s.upsertLocked(sm)
		}
	}

	sort.Slice(rejects, func(i, j int) bool {
		if rejects[i].Sample.ID != rejects[j].Sample.ID {
			return rejects[i].Sample.ID < rejects[j].Sample.ID
		}
		return rejects[i].Reason < rejects[j].Reason
	})
	return rejects
}

// VerifyChecksum 用 CRC32-IEEE（hex）核对样本负载 source|asset|kind|unixns|value。
func VerifyChecksum(sm Sample) bool {
	if sm.Checksum == "" {
		return true
	}
	payload := checksumPayload(sm)
	return fmt.Sprintf("%08x", crc32.ChecksumIEEE([]byte(payload))) == sm.Checksum
}

// Checksum 生成与 VerifyChecksum 对应的校验和。
func Checksum(sm Sample) string {
	return fmt.Sprintf("%08x", crc32.ChecksumIEEE([]byte(checksumPayload(sm))))
}

func checksumPayload(sm Sample) string {
	return fmt.Sprintf("%s|%s|%s|%d|%v", sm.Source, sm.AssetID, sm.Kind,
		sm.TS.UnixNano(), sm.Value)
}

func (s *Store) validateLocked(sm Sample, now time.Time) *Reject {
	if !VerifyChecksum(sm) {
		return &Reject{Sample: sm, Reason: RejectChecksum, Detail: "crc32 mismatch"}
	}
	if math.IsNaN(sm.Value) || math.IsInf(sm.Value, 0) {
		return &Reject{Sample: sm, Reason: RejectOutOfRange, Detail: "non-finite value"}
	}
	lim, ok := s.cfg.Limits[sm.Kind]
	if !ok {
		return &Reject{Sample: sm, Reason: RejectMissingField,
			Detail: "unknown kind: " + string(sm.Kind)}
	}
	if sm.Value < lim.Min || sm.Value > lim.Max {
		return &Reject{Sample: sm, Reason: RejectOutOfRange,
			Detail: fmt.Sprintf("value %v outside [%v,%v]", sm.Value, lim.Min, lim.Max)}
	}
	if s.cfg.Retention > 0 && sm.TS.Before(now.Add(-s.cfg.Retention)) {
		return &Reject{Sample: sm, Reason: RejectExpired,
			Detail: "older than retention window"}
	}
	if s.cfg.MaxFuture > 0 && sm.TS.After(now.Add(s.cfg.MaxFuture)) {
		return &Reject{Sample: sm, Reason: RejectFuture,
			Detail: "timestamp too far in the future"}
	}
	if sr := s.seriesM[sm.key()]; sr != nil {
		if s.cfg.MaxStale > 0 && sm.TS.Before(sr.latest.Add(-s.cfg.MaxStale)) {
			return &Reject{Sample: sm, Reason: RejectStale,
				Detail: fmt.Sprintf("ts %s more than %s behind latest accepted %s",
					sm.TS.Format(time.RFC3339), s.cfg.MaxStale,
					sr.latest.Format(time.RFC3339))}
		}
		if i := s.searchAtOrAfter(sr, sm.TS); i < len(sr.points) && sr.points[i].ts.Equal(sm.TS) {
			if sr.points[i].value != sm.Value {
				return &Reject{Sample: sm, Reason: RejectConflict,
					Detail: fmt.Sprintf("different value %v at same timestamp, existing %v",
						sm.Value, sr.points[i].value)}
			}
			return &Reject{Sample: sm, Reason: RejectDuplicate,
				Detail: "identical value at same timestamp already accepted"}
		}
	}
	return s.jumpCheckLocked(sm)
}

// jumpCheckLocked 只与已接受的时间相邻点比较，避免批内顺序依赖。
func (s *Store) jumpCheckLocked(sm Sample) *Reject {
	lim, ok := s.cfg.Limits[sm.Kind]
	if !ok || lim.MaxRate <= 0 {
		return nil
	}
	sr := s.seriesM[sm.key()]
	if sr == nil {
		return nil
	}
	neighbors := s.neighbors(sr, sm.TS)
	for _, p := range neighbors {
		d := math.Abs(sm.TS.Sub(p.ts).Seconds())
		if d <= 0 || d > 120 {
			continue
		}
		if rate := math.Abs(sm.Value-p.value) / d; rate > lim.MaxRate {
			return &Reject{Sample: sm, Reason: RejectJump,
				Detail: fmt.Sprintf("rate %.3f/s exceeds limit %.3f/s vs point at %s",
					rate, lim.MaxRate, p.ts.Format(time.RFC3339))}
		}
	}
	return nil
}

func (s *Store) neighbors(sr *series, ts time.Time) []point {
	i := s.searchAtOrAfter(sr, ts)
	var out []point
	if i > 0 {
		out = append(out, sr.points[i-1])
	}
	if i < len(sr.points) && !sr.points[i].ts.Equal(ts) {
		out = append(out, sr.points[i])
	}
	return out
}

func (s *Store) searchAtOrAfter(sr *series, ts time.Time) int {
	return sort.Search(len(sr.points), func(i int) bool {
		return !sr.points[i].ts.Before(ts)
	})
}

func (s *Store) upsertLocked(sm Sample) {
	sr := s.seriesM[sm.key()]
	if sr == nil {
		sr = &series{}
		s.seriesM[sm.key()] = sr
	}
	np := point{ts: sm.TS, value: sm.Value, id: sm.ID}
	i := s.searchAtOrAfter(sr, np.ts)
	if i < len(sr.points) && sr.points[i].ts.Equal(np.ts) {
		sr.points[i] = np
	} else {
		sr.points = append(sr.points, point{})
		copy(sr.points[i+1:], sr.points[i:])
		sr.points[i] = np
	}
	if sm.TS.After(sr.latest) {
		sr.latest = sm.TS
	}
}

func samplesEqual(a, b Sample) bool {
	return a.Source == b.Source && a.AssetID == b.AssetID && a.Kind == b.Kind &&
		a.TS.Equal(b.TS) && a.Value == b.Value
}

// Latest 返回某资产某物理量最新已接受样本；不存在时 ok=false。
func (s *Store) Latest(src Source, assetID string, kind Kind) (Sample, bool) {
	s.lock()
	defer s.unlock()
	sr := s.seriesM[(Sample{Source: src, AssetID: assetID, Kind: kind}).key()]
	if sr == nil || len(sr.points) == 0 {
		return Sample{}, false
	}
	p := sr.points[len(sr.points)-1]
	return Sample{ID: p.id, Source: src, AssetID: assetID, Kind: kind,
		TS: p.ts, Value: p.value}, true
}

// Range 返回 [from,to] 内已接受样本（按时间升序）。
func (s *Store) Range(src Source, assetID string, kind Kind, from, to time.Time) []Sample {
	s.lock()
	defer s.unlock()
	sr := s.seriesM[(Sample{Source: src, AssetID: assetID, Kind: kind}).key()]
	if sr == nil {
		return nil
	}
	var out []Sample
	for _, p := range sr.points {
		if (p.ts.Equal(from) || p.ts.After(from)) && (p.ts.Equal(to) || p.ts.Before(to)) {
			out = append(out, Sample{ID: p.id, Source: src, AssetID: assetID,
				Kind: kind, TS: p.ts, Value: p.value})
		}
	}
	return out
}

// All 返回某物理量全部已接受样本（按时间升序）。
func (s *Store) All(src Source, assetID string, kind Kind) []Sample {
	s.lock()
	defer s.unlock()
	sr := s.seriesM[(Sample{Source: src, AssetID: assetID, Kind: kind}).key()]
	if sr == nil {
		return nil
	}
	out := make([]Sample, 0, len(sr.points))
	for _, p := range sr.points {
		out = append(out, Sample{ID: p.id, Source: src, AssetID: assetID,
			Kind: kind, TS: p.ts, Value: p.value})
	}
	return out
}

// Prune 删除保留窗口之外的历史点。
func (s *Store) Prune(now time.Time) {
	s.lock()
	defer s.unlock()
	cutoff := now.Add(-s.cfg.Retention)
	for k, sr := range s.seriesM {
		i := s.searchAtOrAfter(sr, cutoff)
		if i == 0 {
			continue
		}
		trimmed := make([]point, len(sr.points)-i)
		copy(trimmed, sr.points[i:])
		s.seriesM[k] = &series{points: trimmed, latest: sr.latest}
	}
}
