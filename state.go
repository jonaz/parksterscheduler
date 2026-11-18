package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// VehicleState is the last known state of the Tesla, persisted across restarts.
type VehicleState struct {
	LastSeen   time.Time `json:"lastSeen"`   // last telemetry message of any kind
	Lat        float64   `json:"lat"`        // last known location
	Lon        float64   `json:"lon"`        //
	LocationAt time.Time `json:"locationAt"` // zero if no location received yet
	Gear       string    `json:"gear"`       // P, R, N, D or "" if unknown
	GearAt     time.Time `json:"gearAt"`     //

	// The plate the controller last confirmed on the permit, and the local date it
	// applies to. Persisted so the same-day Tesla hold survives restarts.
	ConfirmedDay   string `json:"confirmedDay,omitempty"`
	ConfirmedPlate string `json:"confirmedPlate,omitempty"`
}

// TelemetryUpdate is the subset of a telemetry payload we care about.
type TelemetryUpdate struct {
	At       time.Time
	Location *[2]float64 // lat, lon
	Gear     string      // "" if not present in this payload
}

// Store holds the vehicle state and writes it to disk on every change.
type Store struct {
	mu   sync.Mutex
	path string
	st   VehicleState
}

func loadStore(path string) (*Store, error) {
	s := &Store{path: path}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &s.st); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return s, nil
}

// Snapshot returns a copy of the current state.
func (s *Store) Snapshot() VehicleState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st
}

// Apply merges an update into the state. Older data (for example resent
// messages) never overwrites newer data.
func (s *Store) Apply(u TelemetryUpdate) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if u.At.After(s.st.LastSeen) {
		s.st.LastSeen = u.At
	}
	if u.Location != nil && !u.At.Before(s.st.LocationAt) {
		s.st.Lat, s.st.Lon, s.st.LocationAt = u.Location[0], u.Location[1], u.At
	}
	if u.Gear != "" && !u.At.Before(s.st.GearAt) {
		s.st.Gear, s.st.GearAt = u.Gear, u.At
	}
	return s.save()
}

// SetConfirmed records which plate the controller has confirmed on the permit
// for the given day. An empty plate clears it.
func (s *Store) SetConfirmed(day, plate string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.ConfirmedDay, s.st.ConfirmedPlate = day, plate
	return s.save()
}

func (s *Store) save() error {
	b, err := json.MarshalIndent(s.st, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".state-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path)
}
