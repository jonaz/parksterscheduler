package main

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

const (
	teslaPlate   = "TES111"
	defaultPlate = "DEF222"
)

var (
	home      = Geofence{Lat: 59.4000, Lon: 17.9500}
	work      = Geofence{Lat: 59.3346, Lon: 18.0632, RadiusM: 250}
	elsewhere = Geofence{Lat: 59.3700, Lon: 18.0000} // e.g. the other driver's work
)

type fakeParkster struct {
	current string
	calls   []string
	err     error
}

func (f *fakeParkster) EnsureCar(plate string) (string, bool, error) {
	f.calls = append(f.calls, plate)
	if f.err != nil {
		return f.current, false, f.err
	}
	prev := f.current
	f.current = plate
	return prev, prev != plate, nil
}

type fakeNotifier struct{ infos, errors []string }

func (n *fakeNotifier) Info(m string)  { n.infos = append(n.infos, m) }
func (n *fakeNotifier) Error(m string) { n.errors = append(n.errors, m) }

func testConfig(t *testing.T) *Config {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Stockholm")
	if err != nil {
		t.Fatal(err)
	}
	wd, _ := parseWorkdays(nil)
	return &Config{
		TeslaPlate: teslaPlate, DefaultPlate: defaultPlate,
		TeslaVINs:      map[string]bool{"VIN1": true},
		Work:           work,
		Workdays:       wd,
		MorningStart:   5 * time.Hour,
		MorningEnd:     10 * time.Hour,
		TelemetryStale: 72 * time.Hour,
		Location:       loc,
	}
}

type harness struct {
	ctrl     *Controller
	store    *Store
	parkster *fakeParkster
	notify   *fakeNotifier
	now      time.Time
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	store, err := loadStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{store: store, parkster: &fakeParkster{current: teslaPlate}, notify: &fakeNotifier{}}
	h.ctrl = NewController(testConfig(t), store, Schedule{"Tuesday": teslaPlate}, h.parkster, h.notify)
	h.ctrl.now = func() time.Time { return h.now }
	return h
}

// at returns a time on Tuesday 2026-11-17 (a workday) in Stockholm.
func at(hhmm string) time.Time {
	loc, _ := time.LoadLocation("Europe/Stockholm")
	t, _ := time.ParseInLocation("2006-01-02 15:04", "2026-11-17 "+hhmm, loc)
	return t
}

func (h *harness) report(t *testing.T, ts time.Time, g Geofence, gear string) {
	t.Helper()
	if err := h.store.Apply(TelemetryUpdate{At: ts, Location: &[2]float64{g.Lat, g.Lon}, Gear: gear}); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) evalAt(ts time.Time) {
	h.now = ts
	h.ctrl.Evaluate()
}

func (h *harness) want(t *testing.T, plate, when string) {
	t.Helper()
	if h.parkster.current != plate {
		t.Fatalf("%s: want %s on the permit, got %s", when, plate, h.parkster.current)
	}
}

func TestDefaultWhenTeslaIsNotAtWork(t *testing.T) {
	h := newHarness(t)
	h.report(t, at("04:00"), home, "P")
	h.evalAt(at("05:00"))
	h.want(t, defaultPlate, "05:00")
	if len(h.notify.infos) != 1 {
		t.Fatalf("want one change notification, got %v", h.notify.infos)
	}
	// Further evaluations do not hit Parkster again.
	h.evalAt(at("05:01"))
	h.evalAt(at("08:00"))
	if len(h.parkster.calls) != 1 {
		t.Fatalf("want 1 Parkster call, got %v", h.parkster.calls)
	}
}

func TestTeslaDrivenElsewhereKeepsDefault(t *testing.T) {
	h := newHarness(t)
	h.report(t, at("04:00"), home, "P")
	h.evalAt(at("05:00"))
	h.report(t, at("07:30"), home, "D")
	h.evalAt(at("07:30"))
	h.report(t, at("07:50"), elsewhere, "P")
	h.evalAt(at("07:50"))
	h.want(t, defaultPlate, "Tesla parked at the other driver's work")
}

func TestTeslaDrivingPastWorkKeepsDefault(t *testing.T) {
	h := newHarness(t)
	h.report(t, at("04:00"), home, "P")
	h.evalAt(at("05:00"))
	h.report(t, at("07:40"), work, "D")
	h.evalAt(at("07:40"))
	h.want(t, defaultPlate, "Tesla driving past work")
}

func TestTeslaParkedAtWork(t *testing.T) {
	h := newHarness(t)
	h.report(t, at("04:00"), home, "P")
	h.evalAt(at("05:00"))
	h.report(t, at("07:40"), work, "D")
	h.evalAt(at("07:40"))
	h.report(t, at("07:41"), work, "P")
	h.evalAt(at("07:41"))
	h.want(t, teslaPlate, "Tesla parked at work")
}

func TestTeslaParkedAtWorkOvernight(t *testing.T) {
	h := newHarness(t)
	h.parkster.current = defaultPlate
	h.report(t, at("18:00").AddDate(0, 0, -1), work, "P")
	h.evalAt(at("05:00"))
	h.want(t, teslaPlate, "05:00")
}

func TestTeslaLeavesWorkBeforeMorningEnd(t *testing.T) {
	h := newHarness(t)
	h.report(t, at("07:40"), work, "P")
	h.evalAt(at("07:40"))
	h.want(t, teslaPlate, "parked at work")
	h.report(t, at("08:30"), elsewhere, "P") // dropped off at work, then driven on
	h.evalAt(at("08:30"))
	h.want(t, defaultPlate, "Tesla left work before MorningEnd")
}

func TestNoSwitchBackAfterMorningEnd(t *testing.T) {
	h := newHarness(t)
	h.report(t, at("07:40"), work, "P")
	h.evalAt(at("07:40"))
	h.report(t, at("17:30"), home, "P")
	h.evalAt(at("17:30"))
	h.want(t, teslaPlate, "after MorningEnd")
}

func TestLateArrivalStillSwitchesToTesla(t *testing.T) {
	h := newHarness(t)
	h.report(t, at("04:00"), home, "P")
	h.evalAt(at("05:00"))
	h.report(t, at("13:00"), work, "P")
	h.evalAt(at("13:00"))
	h.want(t, teslaPlate, "arrived after lunch")
}

func TestNothingBeforeMorningStartOrOnWeekend(t *testing.T) {
	h := newHarness(t)
	h.report(t, at("04:00"), home, "P")
	h.evalAt(at("04:59"))
	h.evalAt(at("09:00").AddDate(0, 0, 4)) // Saturday
	if len(h.parkster.calls) != 0 {
		t.Fatalf("want no Parkster calls, got %v", h.parkster.calls)
	}
}

func TestStaleTelemetryFallsBackToSchedule(t *testing.T) {
	h := newHarness(t)
	h.parkster.current = defaultPlate
	h.report(t, at("05:00").Add(-100*time.Hour), home, "P")
	h.evalAt(at("05:00"))
	h.want(t, teslaPlate, "schedule says Tesla on Tuesdays")
	if len(h.notify.errors) != 1 {
		t.Fatalf("want one stale warning, got %v", h.notify.errors)
	}
	h.evalAt(at("06:00"))
	if len(h.notify.errors) != 1 {
		t.Fatalf("stale warning should be sent once per day, got %v", h.notify.errors)
	}
}

func TestParksterErrorIsRetried(t *testing.T) {
	h := newHarness(t)
	h.report(t, at("04:00"), home, "P")
	h.parkster.err = errors.New("boom")
	h.evalAt(at("05:00"))
	h.evalAt(at("05:01")) // within retry interval
	if len(h.parkster.calls) != 1 || len(h.notify.errors) != 1 {
		t.Fatalf("calls=%v errors=%v", h.parkster.calls, h.notify.errors)
	}
	h.parkster.err = nil
	h.evalAt(at("05:03"))
	h.want(t, defaultPlate, "after retry")
}

func TestOlderDataDoesNotOverwrite(t *testing.T) {
	h := newHarness(t)
	h.report(t, at("07:40"), work, "P")
	h.report(t, at("07:20"), elsewhere, "D") // resent old message
	st := h.store.Snapshot()
	if !work.Contains(st.Lat, st.Lon) || st.Gear != "P" {
		t.Fatalf("old data overwrote newer: %+v", st)
	}
}

func TestDistance(t *testing.T) {
	// 0.0028° north (~311 m) and 0.0043° east at 59.3° (~245 m): ~396 m.
	d := distanceMeters(59.3303, 18.0586, 59.3275, 18.0543)
	if d < 385 || d > 405 {
		t.Fatalf("unexpected distance %.0f m", d)
	}
}
