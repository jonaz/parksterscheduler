package main

import (
	"fmt"
	"log"
	"strings"
	"time"
)

const retryInterval = 2 * time.Minute

// Decision is which car should be on the permit right now, and why.
type Decision struct {
	Plate  string
	Reason string
}

// Controller decides which car should be on the permit and applies it.
// Run it from a single goroutine: Evaluate is not safe for concurrent use.
type Controller struct {
	cfg      *Config
	store    *Store // nil when telemetry is disabled
	schedule Schedule
	parkster CarSetter
	notify   Notifier
	now      func() time.Time

	trigger chan struct{}

	confirmedDay   string // local date the confirmed plate applies to
	confirmedPlate string
	lastAttempt    time.Time
	lastAttemptFor string
	staleWarnedDay string
}

func NewController(cfg *Config, store *Store, schedule Schedule, parkster CarSetter, notify Notifier) *Controller {
	c := &Controller{
		cfg: cfg, store: store, schedule: schedule, parkster: parkster, notify: notify,
		now:     time.Now,
		trigger: make(chan struct{}, 1),
	}
	if store != nil {
		st := store.Snapshot()
		c.confirmedDay, c.confirmedPlate = st.ConfirmedDay, st.ConfirmedPlate
	}
	return c
}

// setConfirmed updates the confirmed plate and persists it when there is a store.
func (c *Controller) setConfirmed(day, plate string) {
	c.confirmedDay, c.confirmedPlate = day, plate
	if c.store == nil {
		return
	}
	if err := c.store.SetConfirmed(day, plate); err != nil {
		log.Printf("❌ Could not save confirmed plate: %v", err)
	}
}

// Trigger asks the controller to re-evaluate soon. Safe to call from any goroutine.
func (c *Controller) Trigger() {
	select {
	case c.trigger <- struct{}{}:
	default:
	}
}

// Run evaluates every minute and whenever Trigger is called. It never returns.
func (c *Controller) Run() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	c.Evaluate()
	for {
		select {
		case <-ticker.C:
		case <-c.trigger:
		}
		c.Evaluate()
	}
}

// Decide returns the car that should be on the permit at the given time.
// ok is false when the controller should not touch the permit.
func (c *Controller) Decide(now time.Time) (d Decision, ok bool) {
	local := now.In(c.cfg.Location)
	if !c.cfg.Workdays[local.Weekday()] || timeOfDay(local) < c.cfg.MorningStart {
		return Decision{}, false
	}

	if !c.cfg.TelemetryEnabled() {
		return c.fromSchedule(local, "schedule")
	}

	st := c.store.Snapshot()
	if st.LastSeen.IsZero() || now.Sub(st.LastSeen) > c.cfg.TelemetryStale {
		day := dayKey(local)
		if c.staleWarnedDay != day {
			c.staleWarnedDay = day
			last := "never"
			if !st.LastSeen.IsZero() {
				last = st.LastSeen.In(c.cfg.Location).Format("2006-01-02 15:04")
			}
			c.notify.Error(fmt.Sprintf("No Tesla telemetry since %s. Falling back to the schedule today.", last))
		}
		return c.fromSchedule(local, "no recent telemetry, using schedule")
	}

	// Someone else may be driving the Tesla, so only a Tesla that is parked at
	// work counts. Driving past work (gear D/R/N) does not.
	parked := st.Gear != "D" && st.Gear != "R" && st.Gear != "N"
	if !st.LocationAt.IsZero() && parked && c.cfg.Work.Contains(st.Lat, st.Lon) {
		return Decision{c.cfg.TeslaPlate, "Tesla is parked at work"}, true
	}
	return Decision{c.cfg.DefaultPlate, "Tesla is not parked at work"}, true
}

func (c *Controller) fromSchedule(local time.Time, reason string) (Decision, bool) {
	plate, ok := c.schedule[local.Weekday().String()]
	if !ok || plate == "" {
		return Decision{}, false
	}
	return Decision{plate, reason}, true
}

// Evaluate decides and, if needed, updates Parkster.
func (c *Controller) Evaluate() {
	now := c.now()
	d, ok := c.Decide(now)
	if !ok {
		return
	}
	local := now.In(c.cfg.Location)
	day := dayKey(local)

	if c.confirmedDay == day && strings.EqualFold(c.confirmedPlate, d.Plate) {
		return
	}
	// Late in the day, never switch back from the Tesla to the default car: the Tesla
	// was parked at work earlier and has just left (lunch, or driving home).
	if c.confirmedDay == day && c.cfg.TelemetryEnabled() &&
		strings.EqualFold(c.confirmedPlate, c.cfg.TeslaPlate) && strings.EqualFold(d.Plate, c.cfg.DefaultPlate) &&
		timeOfDay(local) >= c.cfg.MorningEnd {
		return
	}
	if strings.EqualFold(c.lastAttemptFor, d.Plate) && now.Sub(c.lastAttempt) < retryInterval {
		return
	}
	c.lastAttempt, c.lastAttemptFor = now, d.Plate

	log.Printf("🚗 Want %s on the permit (%s).", d.Plate, d.Reason)
	previous, changed, err := c.parkster.EnsureCar(d.Plate)
	if err != nil {
		c.notify.Error(fmt.Sprintf("Could not set %s on Parkster (%s): %v", d.Plate, d.Reason, err))
		// The change may have gone through even though verification failed, so we
		// no longer know which plate is on the permit.
		c.setConfirmed("", "")
		return
	}
	c.setConfirmed(day, d.Plate)
	if changed {
		c.notify.Info(fmt.Sprintf("Parkster switched from %s to %s: %s.", previous, d.Plate, d.Reason))
	} else {
		log.Printf("✅ %s is already on the permit.", d.Plate)
	}
}

func timeOfDay(t time.Time) time.Duration {
	return time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute
}

func dayKey(t time.Time) string {
	return t.Format("2006-01-02")
}
