package demand

import (
	"sort"
	"sync"
	"time"
)

// PeakRecord 是一个已关闭窗口的需量峰值记录。
type PeakRecord struct {
	WindowStart time.Time
	WindowEnd   time.Time
	DemandKW    float64
}

// MonthlyPeak 是某个自然月的峰值及其所属窗口。
type MonthlyPeak struct {
	Month  string // YYYY-MM（按配置时区）
	Record PeakRecord
}

// PeakBook 保留每个自然月的历史需量峰值。
// 窗口按固定锚点对齐，跨天/跨月的窗口归入其起点所在月份，口径唯一。
type PeakBook struct {
	mu sync.Mutex

	loc       *time.Location
	current   string                // 当前月 YYYY-MM
	monthly   map[string]PeakRecord // 月 -> 峰值记录
	closed    []PeakRecord          // 全部已关闭窗口（保留期内）
	closedIdx map[string]int        // 窗口起点 -> closed 下标，回放幂等
}

// NewPeakBook 创建峰值台账，loc 为跨天/跨月统计时区。
func NewPeakBook(loc *time.Location) *PeakBook {
	if loc == nil {
		loc = time.Local
	}
	return &PeakBook{loc: loc, monthly: make(map[string]PeakRecord),
		closedIdx: make(map[string]int)}
}

func monthKey(t time.Time, loc *time.Location) string {
	y, m, _ := t.In(loc).Date()
	return formatMonth(y, m)
}

func formatMonth(y int, m time.Month) string {
	const digits = "0123456789"
	s := []byte{'0', '0', '0', '0', '-', '0', '0'}
	s[0] = digits[(y/1000)%10]
	s[1] = digits[(y/100)%10]
	s[2] = digits[(y/10)%10]
	s[3] = digits[y%10]
	s[5] = digits[(int(m)/10)%10]
	s[6] = digits[int(m)%10]
	return string(s)
}

// CloseWindow 登记一个刚关闭窗口的需量；重复回放同一窗口不会重复抬高峰值，
// 只有相同窗口起点的更高值才更新（同值重放结果一致）。
func (b *PeakBook) CloseWindow(rec PeakRecord) {
	b.mu.Lock()
	defer b.mu.Unlock()

	month := monthKey(rec.WindowStart, b.loc)
	if old, ok := b.monthly[month]; !ok || rec.DemandKW > old.DemandKW {
		b.monthly[month] = rec
	}
	key := rec.WindowStart.Format(time.RFC3339Nano)
	if idx, ok := b.closedIdx[key]; ok {
		b.closed[idx] = rec
	} else {
		b.closedIdx[key] = len(b.closed)
		b.closed = append(b.closed, rec)
	}
	b.current = month
}

// CurrentMonthPeak 返回 now 所在月份的峰值记录；本月尚无关闭窗口时 ok=false。
func (b *PeakBook) CurrentMonthPeak(now time.Time) (PeakRecord, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	key := monthKey(now, b.loc)
	r, ok := b.monthly[key]
	return r, ok
}

// MonthPeak 返回指定月份（YYYY-MM）的峰值。
func (b *PeakBook) MonthPeak(month string) (PeakRecord, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	r, ok := b.monthly[month]
	return r, ok
}

// History 返回所有已有月份的峰值，按月份升序。
func (b *PeakBook) History() []MonthlyPeak {
	b.mu.Lock()
	defer b.mu.Unlock()
	keys := make([]string, 0, len(b.monthly))
	for k := range b.monthly {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]MonthlyPeak, 0, len(keys))
	for _, k := range keys {
		out = append(out, MonthlyPeak{Month: k, Record: b.monthly[k]})
	}
	return out
}

// ClosedWindows 返回登记过的已关闭窗口记录（按窗口起点升序）。
func (b *PeakBook) ClosedWindows() []PeakRecord {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]PeakRecord, len(b.closed))
	copy(out, b.closed)
	sort.Slice(out, func(i, j int) bool { return out[i].WindowStart.Before(out[j].WindowStart) })
	return out
}
