package demand

import (
	"math"
	"time"
)

// Point 是参与需量统计的功率样本。
type Point struct {
	TS    time.Time
	Value float64 // kW
}

// Window 描述一个需量统计窗口（滑动推进，对齐固定锚点，例如每 15 分钟整点）。
type Window struct {
	Start time.Time
	End   time.Time // 开区间
}

// Contains 报告 t 是否落在窗口 [Start, End) 内。
func (w Window) Contains(t time.Time) bool {
	return (t.Equal(w.Start) || t.After(w.Start)) && t.Before(w.End)
}

// Elapsed 返回窗口已经过的时长（受 now 截断，不超过窗口长度）。
func (w Window) Elapsed(now time.Time) time.Duration {
	if !now.After(w.Start) {
		return 0
	}
	if now.After(w.End) {
		return w.End.Sub(w.Start)
	}
	return now.Sub(w.Start)
}

// Remaining 返回窗口剩余时长。
func (w Window) Remaining(now time.Time) time.Duration {
	if !now.Before(w.End) {
		return 0
	}
	if now.Before(w.Start) {
		return w.End.Sub(w.Start)
	}
	return w.End.Sub(now)
}

// CurrentWindow 返回包含 now（锚点对齐）的窗口。
// anchor 通常为当天 00:00；length 为窗口长度（如 15min）。
func CurrentWindow(now time.Time, anchor time.Time, length time.Duration) Window {
	elapsed := now.Sub(anchor)
	n := floorDiv(elapsed, length)
	start := anchor.Add(time.Duration(n) * length)
	return Window{Start: start, End: start.Add(length)}
}

// floorDiv 是时间纳秒尺度的数学整除（向负无穷取整）。
func floorDiv(a, b time.Duration) int64 {
	q := int64(a) / int64(b)
	r := int64(a) % int64(b)
	if r != 0 && (r < 0) != (int64(b) < 0) {
		q--
	}
	return q
}

// DayAnchor 返回 loc 时区下 date 当天 00:00。
func DayAnchor(date time.Time, loc *time.Location) time.Time {
	if loc == nil {
		loc = time.Local
	}
	y, m, d := date.In(loc).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, loc)
}

// AverageDemand 用梯形积分计算 [from,to) 内的平均功率（kW）。
// 没有完整覆盖区间时返回 ok=false（统计口径宁缺毋滥）。
// 区间起点之前的最后一个点用于外推区间起点；区间内最末点外推到 to。
func AverageDemand(points []Point, from, to time.Time) (float64, bool) {
	if !to.After(from) || len(points) == 0 {
		return 0, false
	}
	// 找到区间内/边界上的点，以及 from 之前最近的点。
	var prev *Point
	idx := 0
	for idx < len(points) && points[idx].TS.Before(from) {
		p := points[idx]
		prev = &p
		idx++
	}
	if idx >= len(points) {
		// 区间内没有任何点。
		return 0, false
	}
	// 起点值：from 上有点用该点，否则用前一点零阶保持；没有前一点则无法覆盖。
	first := points[idx]
	if !first.TS.Equal(from) && prev == nil {
		return 0, false
	}

	energy := 0.0 // kWh
	curTS := from
	curVal := first.Value
	if !first.TS.Equal(from) {
		curVal = prev.Value
	}

	for j := idx; j < len(points) && points[j].TS.Before(to); j++ {
		p := points[j]
		if p.TS.Equal(curTS) {
			curVal = p.Value
			continue
		}
		energy += (curVal + p.Value) / 2 * p.TS.Sub(curTS).Hours()
		curTS, curVal = p.TS, p.Value
	}
	// 末点零阶保持到窗口结束。
	energy += curVal * to.Sub(curTS).Hours()

	avg := energy / to.Sub(from).Hours()
	if math.IsNaN(avg) || math.IsInf(avg, 0) {
		return 0, false
	}
	return avg, true
}

// CurrentDemand 计算窗口当期需量：从窗口起点到 now 的平均功率。
// 要求窗口起点已被样本覆盖（有点或其前有点），否则 ok=false。
func CurrentDemand(points []Point, w Window, now time.Time) (float64, bool) {
	end := now
	if now.After(w.End) {
		end = w.End
	}
	if !end.After(w.Start) {
		return 0, false
	}
	avg, ok := AverageDemand(points, w.Start, end)
	if ok {
		return avg, true
	}
	// 窗口起点上/之前没有点，但窗口内已有首个点：
	// 用该点零阶保持回推到窗口起点（遥测周期较短，误差有限且偏保守方向由调用方处理）。
	firstIn := -1
	for i, p := range points {
		if !p.TS.Before(w.Start) && p.TS.Before(end) {
			firstIn = i
			break
		}
	}
	if firstIn < 0 {
		return 0, false
	}
	extended := make([]Point, 0, len(points)-firstIn+1)
	extended = append(extended, Point{TS: w.Start, Value: points[firstIn].Value})
	for _, p := range points[firstIn:] {
		if p.TS.Before(end) {
			extended = append(extended, p)
		}
	}
	return AverageDemand(extended, w.Start, end)
}

// ProjectEnd 依据当期数据预测窗口结束时的需量。
// 采用零阶保持：最后一个点的功率持续到窗口结束。
// 这给出"若负载维持当前水平"的保守预测，配合趋势预测可进一步加风险裕量。
func ProjectEnd(points []Point, w Window, now time.Time) (float64, bool) {
	cur, ok := CurrentDemand(points, w, now)
	if !ok {
		return 0, false
	}
	last, ok2 := LastAtOrBefore(points, now)
	if !ok2 {
		return cur, true
	}
	elapsed := w.Elapsed(now).Seconds()
	remain := w.Remaining(now).Seconds()
	total := elapsed + remain
	if total <= 0 {
		return cur, true
	}
	return (cur*elapsed + last.Value*remain) / total, true
}

// LastAtOrBefore 返回 ts 时刻（含）之前的最后一个点。
func LastAtOrBefore(points []Point, ts time.Time) (Point, bool) {
	var best Point
	found := false
	for _, p := range points {
		if !p.TS.After(ts) {
			best, found = p, true
		}
	}
	return best, found
}
