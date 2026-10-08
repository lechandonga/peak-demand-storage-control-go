package telemetry

import (
	"testing"
	"time"
)

func TestStoreRejectReasons(t *testing.T) {
	now := time.Date(2026, 10, 8, 10, 0, 0, 0, time.UTC)
	st := NewStore(DefaultConfig())

	mk := func(id string, ts time.Time, kind Kind, v float64, ck bool) Sample {
		s := Sample{ID: id, Source: SourceGrid, AssetID: "g1", Kind: kind, TS: ts, Value: v}
		if ck {
			s.Checksum = Checksum(s)
		} else {
			s.Checksum = "deadbeef"
		}
		return s
	}

	// 接受首条基线。
	if r := st.Ingest(mk("ok1", now, KindGridPower, 500, true), now); r != nil {
		t.Fatalf("baseline rejected: %v", r)
	}

	cases := []struct {
		name   string
		s      Sample
		reason Reason
	}{
		{"duplicate", mk("ok1", now, KindGridPower, 500, true), RejectDuplicate},
		{"conflict_id", mk("ok1", now.Add(time.Second), KindGridPower, 999, true), RejectConflict},
		{"missing_id", Sample{Source: SourceGrid, AssetID: "g1", Kind: KindGridPower, TS: now, Value: 1}, RejectMissingField},
		{"out_of_range", mk("oor", now.Add(time.Second), KindGridPower, 9e9, true), RejectOutOfRange},
		{"nan", mk("nan", now.Add(2*time.Second), KindGridPower, float64NaN(), true), RejectOutOfRange},
		{"stale", mk("stale", now.Add(-20*time.Minute), KindGridPower, 500, true), RejectStale},
		{"future", mk("future", now.Add(30*time.Minute), KindGridPower, 500, true), RejectFuture},
		{"expired", mk("exp", now.Add(-25*time.Hour), KindGridPower, 500, true), RejectExpired},
		{"jump", mk("jump", now.Add(time.Second), KindGridPower, 9000, true), RejectJump},
		{"checksum", mk("badck", now.Add(2*time.Second), KindGridPower, 500, false), RejectChecksum},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := st.Ingest(tc.s, now)
			if r == nil {
				t.Fatalf("expected reject %s, got acceptance", tc.reason)
			}
			if r.Reason != tc.reason {
				t.Fatalf("reason = %s, want %s (%s)", r.Reason, tc.reason, r.Detail)
			}
			t.Logf("input id=%s value=%v -> reject reason=%s detail=%s",
				tc.s.ID, tc.s.Value, r.Reason, r.Detail)
		})
	}

	// 同时间戳同值不同 ID -> duplicate；不同值 -> conflict。
	same := mk("sameval", now.Add(5*time.Second), KindLoadPower, 100, true)
	same.Source = SourceLoad
	same.Checksum = Checksum(same)
	_ = st.Ingest(same, now)
	dup := mk("sameval2", now.Add(5*time.Second), KindLoadPower, 100, true)
	dup.Source = SourceLoad
	dup.Checksum = Checksum(dup)
	if r := st.Ingest(dup, now); r == nil || r.Reason != RejectDuplicate {
		t.Fatalf("want duplicate at same ts, got %v", r)
	}
	con := mk("con", now.Add(5*time.Second), KindLoadPower, 101, true)
	con.Source = SourceLoad
	con.Checksum = Checksum(con)
	if r := st.Ingest(con, now); r == nil || r.Reason != RejectConflict {
		t.Fatalf("want conflict at same ts, got %v", r)
	}
}

func TestStoreOutOfOrderAndReplay(t *testing.T) {
	now := time.Date(2026, 10, 8, 10, 0, 0, 0, time.UTC)
	mk := func(id string, ts time.Time, v float64) Sample {
		s := Sample{ID: id, Source: SourceGrid, AssetID: "g1",
			Kind: KindGridPower, TS: ts, Value: v}
		s.Checksum = Checksum(s)
		return s
	}
	base := []Sample{
		mk("a", now, 100),
		mk("b", now.Add(30*time.Second), 110),
		mk("c", now.Add(60*time.Second), 120),
	}

	// 乱序批处理。
	st1 := NewStore(DefaultConfig())
	shuffled := []Sample{base[2], base[0], base[1]}
	if rs := st1.IngestBatch(shuffled, now.Add(2*time.Minute)); len(rs) != 0 {
		t.Fatalf("shuffled batch rejected: %+v", rs)
	}
	// 顺序逐条处理。
	st2 := NewStore(DefaultConfig())
	for _, s := range base {
		if r := st2.Ingest(s, now.Add(2*time.Minute)); r != nil {
			t.Fatalf("ordered ingest rejected: %v", r)
		}
	}
	got1 := st1.Range(SourceGrid, "g1", KindGridPower, now, now.Add(2*time.Minute))
	got2 := st2.Range(SourceGrid, "g1", KindGridPower, now, now.Add(2*time.Minute))
	if len(got1) != 3 || len(got2) != 3 {
		t.Fatalf("range len %d vs %d", len(got1), len(got2))
	}
	for i := range got1 {
		if got1[i].Value != got2[i].Value || !got1[i].TS.Equal(got2[i].TS) {
			t.Fatalf("out-of-order vs ordered divergence at %d: %+v %+v", i, got1[i], got2[i])
		}
	}

	// 重复回放：全部 duplicate，状态不变。
	rs := st1.IngestBatch(base, now.Add(2*time.Minute))
	if len(rs) != 3 {
		t.Fatalf("replay rejects = %d, want 3", len(rs))
	}
	for _, r := range rs {
		if r.Reason != RejectDuplicate {
			t.Fatalf("replay reason = %s, want duplicate", r.Reason)
		}
	}
	again := st1.Range(SourceGrid, "g1", KindGridPower, now, now.Add(2*time.Minute))
	if len(again) != 3 {
		t.Fatalf("replay changed state: len=%d", len(again))
	}
}

func TestStoreConcurrentUniqueness(t *testing.T) {
	now := time.Date(2026, 10, 8, 10, 0, 0, 0, time.UTC)
	st := NewStore(DefaultConfig())

	// 同一批样本从多个 goroutine 重复并发提交，最终只能有一个接受。
	var s []Sample
	for i := 0; i < 50; i++ {
		sm := Sample{ID: "x", Source: SourceGrid, AssetID: "g1",
			Kind: KindGridPower, TS: now, Value: 500}
		sm.Checksum = Checksum(sm)
		s = append(s, sm)
	}
	done := make(chan *Reject, len(s))
	for _, sm := range s {
		go func(sm Sample) { done <- st.Ingest(sm, now.Add(time.Second)) }(sm)
	}
	accepted := 0
	for range s {
		if r := <-done; r == nil {
			accepted++
		} else if r.Reason != RejectDuplicate {
			t.Fatalf("unexpected reason %s", r.Reason)
		}
	}
	if accepted != 1 {
		t.Fatalf("accepted = %d, want exactly 1", accepted)
	}
}

func float64NaN() float64 {
	// 用变量规避编译期常量转换。
	v := 0.0
	return v / (v - v)
}
