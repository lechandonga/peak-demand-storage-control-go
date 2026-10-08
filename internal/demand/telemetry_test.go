package demand

import (
	"testing"
	"time"
)

func TestRejectReasons(t *testing.T) {
	f := newFixture(t)
	at := f.at(0)

	good := gridSample("ok", at, 100)
	res := f.feed(0, good)
	if len(res.Rejected) != 0 || !res.AcceptedID("ok") {
		t.Fatalf("valid sample should be accepted: %+v", res)
	}

	cases := []struct {
		name   string
		sample Sample
		want   RejectReason
	}{
		{"unknown_source", Sample{ID: "u1", Source: "mystery", At: at, PowerKW: 1}, RejectUnknownSource},
		{"missing_id", Sample{Source: SourceGrid, At: at, PowerKW: 1}, RejectMissingID},
		{"zero_time", Sample{ID: "u2", Source: SourceGrid, PowerKW: 1}, RejectZeroTime},
		{"future", gridSample("u3", at.Add(time.Hour), 1), RejectFuture},
		{"too_old", gridSample("u4", at.Add(-7*time.Hour), 1), RejectTooOld},
		{"not_finite", gridSample("u5", at, nanF()), RejectNaN},
		{"grid_out_of_range", gridSample("u6", at, 999999), RejectPowerRange},
		{"load_negative", loadSample("u7", at, "L1", -1), RejectPowerRange},
		{"soc_out_of_range", battSample("u8", at, 0, 120, 25, 0), RejectSOCRange},
		{"temp_out_of_range", battSample("u9", at, 0, 50, 200, 0), RejectTempRange},
		{"checksum", func() Sample {
			s := gridSample("u10", at, 100)
			s.Checksum = "deadbeef"
			return s
		}(), RejectChecksum},
	}
	for _, tc := range cases {
		// Validate against the minute-0 reference clock so future/old checks
		// are meaningful regardless of the sample's own timestamp.
		f.clock = at
		t.Run(tc.name, func(t *testing.T) {
			r := f.store.Ingest([]Sample{tc.sample})
			if len(r.Rejected) != 1 || r.Rejected[0].Reason != tc.want {
				t.Fatalf("want %s, got %+v", tc.want, r.Rejected)
			}
		})
	}

	res = f.feed(0, good)
	if rj, ok := res.RejectionFor("ok"); !ok || rj.Reason != RejectDuplicate {
		t.Fatalf("replay must be duplicate_id, got %+v", res.Rejected)
	}

	tampered := good
	tampered.At = at.Add(time.Minute)
	res = f.ingestAt(tampered)
	if rj, ok := res.RejectionFor("ok"); !ok || rj.Reason != RejectStaleReplay {
		t.Fatalf("id reuse must be stale_replay, got %+v", res.Rejected)
	}
}

func TestOutOfOrderAndDuplicates(t *testing.T) {
	f := newFixture(t)
	batch := []Sample{
		gridSample("g2", f.at(2), 200),
		gridSample("g1", f.at(1), 150),
		gridSample("g2", f.at(2), 200),
		gridSample("g0", f.at(0), 100),
	}
	res := f.feed(2, batch...)
	if len(res.Accepted) != 3 {
		t.Fatalf("want 3 accepted, got %d", len(res.Accepted))
	}
	if rj, ok := res.RejectionFor("g2"); !ok || rj.Reason != RejectDuplicate {
		t.Fatalf("in-batch duplicate must be flagged: %+v", res.Rejected)
	}
	if got := f.store.Snapshot()["grid:G"].PowerKW; got != 200 {
		t.Fatalf("newest-by-time must win regardless of arrival order, got %.1f", got)
	}

	res = f.feed(2, batch...)
	if len(res.Accepted) != 0 || len(res.Rejected) != 4 {
		t.Fatalf("replayed batch must accept nothing, got %+v", res)
	}
}

func TestSOCEnergyReconciliation(t *testing.T) {
	f := newFixture(t)
	// Tight model for this test: physically impossible SOC motion rejects.
	f.store.SetBatteryReconcile("B", ReconcileConfig{
		CapacityWh: 1e6, ChargeEff: .95, DischargeEff: .95, SOCNoisePct: .5,
	})
	rc := ReconcileConfig{CapacityWh: 1e6, ChargeEff: .95, DischargeEff: .95, SOCNoisePct: 2}
	f.store.Ingest([]Sample{battSample("b0", f.at(0), 200, 70, 25, 0)})
	expected := PredictSOC(rc, 70, 200, 1)

	good := battSample("b1", f.at(1), 200, expected, 25, 3333)
	if r := f.ingestAt(good); len(r.Rejected) != 0 {
		t.Fatalf("reconciled SOC must be accepted: %+v", r.Rejected)
	}

	bad := battSample("b2", f.at(2), 200, expected-5, 25, 3333)
	if r := f.ingestAt(bad); len(r.Rejected) != 1 ||
		r.Rejected[0].Reason != RejectSOCJump {
		t.Fatalf("impossible SOC motion must reject as soc_energy_mismatch: %+v", r.Rejected)
	}

	back := battSample("b3", f.at(3), 0, expected, 25, 10)
	if r := f.ingestAt(back); len(r.Rejected) != 1 ||
		r.Rejected[0].Reason != RejectSOCJump {
		t.Fatalf("backwards energy counter must reject: %+v", r.Rejected)
	}
}

func TestChecksumAcceptAndReject(t *testing.T) {
	f := newFixture(t)
	s := gridSample("c1", f.at(0), 400)
	s.Checksum = ChecksumOf(s)
	if r := f.store.Ingest([]Sample{s}); len(r.Rejected) != 0 {
		t.Fatalf("correct checksum must pass: %+v", r.Rejected)
	}
	s2 := gridSample("c2", f.at(1), 400)
	s2.PowerKW = 401
	s2.Checksum = ChecksumOf(gridSample("c2", f.at(1), 400))
	if r := f.ingestAt(s2); len(r.Rejected) != 1 ||
		r.Rejected[0].Reason != RejectChecksum {
		t.Fatalf("tampered frame must fail checksum: %+v", r.Rejected)
	}
}
