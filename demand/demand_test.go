package demand

import (
	"math"
	"testing"
	"time"
)

func TestWindowAlignmentAndBoundary(t *testing.T) {
	loc := time.UTC
	anchor := time.Date(2026, 9, 30, 0, 0, 0, 0, loc)
	wl := 15 * time.Minute

	cases := []struct {
		h, m int
		ws   string
	}{
		{13, 0, "13:00"}, {13, 14, "13:00"}, {13, 15, "13:15"},
		{23, 59, "23:45"}, {0, 0, "00:00"},
	}
	for _, tc := range cases {
		now := time.Date(2026, 9, 30, tc.h, tc.m, 30, 0, loc)
		w := CurrentWindow(now, anchor, wl)
		if got := w.Start.Format("15:04"); got != tc.ws {
			t.Fatalf("now %s window start = %s, want %s", now.Format("15:04"), got, tc.ws)
		}
		if w.End.Sub(w.Start) != wl {
			t.Fatalf("window length %s", w.End.Sub(w.Start))
		}
		if !w.Contains(now) {
			t.Fatalf("window must contain now")
		}
	}

	// 跨天：23:50 落在 23:45 窗口，结束于次日 00:00。
	late := time.Date(2026, 9, 30, 23, 50, 0, 0, loc)
	w := CurrentWindow(late, anchor, wl)
	wantEnd := time.Date(2026, 10, 1, 0, 0, 0, 0, loc)
	if !w.End.Equal(wantEnd) {
		t.Fatalf("cross-day end = %s, want %s", w.End, wantEnd)
	}
	// 00:04 则属于 00:00 窗口。
	early := CurrentWindow(time.Date(2026, 10, 1, 0, 4, 0, 0, loc), anchor, wl)
	if !early.Start.Equal(wantEnd) {
		t.Fatalf("next-day window start = %s, want %s", early.Start, wantEnd)
	}
}

func TestAverageDemandTrapezoid(t *testing.T) {
	from := time.Date(2026, 9, 30, 13, 0, 0, 0, time.UTC)
	to := from.Add(time.Hour)
	// 恒定 800kW 一小时 -> 平均 800。
	var pts []Point
	for i := 0; i <= 12; i++ {
		pts = append(pts, Point{TS: from.Add(time.Duration(i) * 5 * time.Minute), Value: 800})
	}
	avg, ok := AverageDemand(pts, from, to)
	if !ok || math.Abs(avg-800) > 1e-9 {
		t.Fatalf("constant avg = %v ok=%v", avg, ok)
	}

	// 从 0 线性升到 600：平均 300。
	pts = nil
	for i := 0; i <= 60; i++ {
		pts = append(pts, Point{TS: from.Add(time.Duration(i) * time.Minute),
			Value: float64(i) * 10})
	}
	avg, ok = AverageDemand(pts, from, to)
	if !ok || math.Abs(avg-300) > 0.2 {
		t.Fatalf("ramp avg = %v, want 300", avg)
	}

	// 起点之前有点、起点上无点：零阶保持覆盖。
	pts = []Point{
		{TS: from.Add(-time.Minute), Value: 400},
		{TS: from.Add(30 * time.Minute), Value: 400},
	}
	if avg, ok = AverageDemand(pts, from, to); !ok || math.Abs(avg-400) > 1e-6 {
		t.Fatalf("hold avg = %v ok=%v", avg, ok)
	}

	// 起点无覆盖 -> ok=false。
	if _, ok := AverageDemand([]Point{{TS: from.Add(time.Minute), Value: 1}}, from, to); ok {
		t.Fatalf("uncovered start must fail")
	}
}

func TestProjectEnd(t *testing.T) {
	from := time.Date(2026, 9, 30, 13, 0, 0, 0, time.UTC)
	w := Window{Start: from, End: from.Add(15 * time.Minute)}
	// 前 5 分钟 600kW，之后保持 900kW（末点零阶保持预测）。
	now := from.Add(5 * time.Minute)
	pts := []Point{
		{TS: from, Value: 600},
		{TS: now, Value: 900},
	}
	proj, ok := ProjectEnd(pts, w, now)
	if !ok {
		t.Fatal("project failed")
	}
	// (600+900)/2 *5min 后，900kW 持续 10min。
	want := 800.0 // 梯形积分：(600+900)/2*5 + 900*10，除以 15
	if math.Abs(proj-want) > 1e-6 {
		t.Fatalf("project = %v, want %v", proj, want)
	}
}

func TestPeakBookMonthlyAndReplay(t *testing.T) {
	loc := time.UTC
	b := NewPeakBook(loc)
	sep30 := time.Date(2026, 9, 30, 13, 0, 0, 0, loc)
	oct1 := time.Date(2026, 10, 1, 0, 0, 0, 0, loc)

	w1 := PeakRecord{sep30, sep30.Add(15 * time.Minute), 700}
	w2 := PeakRecord{sep30.Add(15 * time.Minute), sep30.Add(30 * time.Minute), 820}
	w3 := PeakRecord{oct1, oct1.Add(15 * time.Minute), 600}
	for _, w := range []PeakRecord{w1, w2, w3} {
		b.CloseWindow(w)
	}
	// 重复回放同月窗口，峰值不重复累计。
	b.CloseWindow(w2)
	b.CloseWindow(w1)

	sep, ok := b.MonthPeak("2026-09")
	if !ok || sep.DemandKW != 820 {
		t.Fatalf("sep peak = %v ok=%v", sep, ok)
	}
	octP, ok := b.MonthPeak("2026-10")
	if !ok || octP.DemandKW != 600 {
		t.Fatalf("oct peak = %v", octP)
	}

	// 跨月窗口按起点月份归档：9/30 23:45 窗口归入 9 月。
	cross := PeakRecord{
		time.Date(2026, 9, 30, 23, 45, 0, 0, loc),
		time.Date(2026, 10, 1, 0, 0, 0, 0, loc), 900}
	b.CloseWindow(cross)
	sep, _ = b.MonthPeak("2026-09")
	if sep.DemandKW != 900 {
		t.Fatalf("cross-boundary window must count in start month: %v", sep.DemandKW)
	}
	if len(b.ClosedWindows()) != 4 { // 3 个窗口 + 跨月窗口；回放不重复追加
		t.Fatalf("closed window records = %d", len(b.ClosedWindows()))
	}
}
