package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestExampleConfigLoads(t *testing.T) {
	raw, err := os.ReadFile("config.example.json")
	if err != nil {
		t.Fatal(err)
	}
	c := &Config{}
	if err := c.applyFile(raw); err != nil {
		t.Fatalf("config.example.json: %v", err)
	}
	if !c.TelemetryEnabled() || c.Work.RadiusM != 250 || c.MorningStart != 5*time.Hour ||
		c.FallbackSchedule["Tuesday"] != "XYZ789" || !c.Workdays[time.Friday] || c.Workdays[time.Saturday] {
		t.Fatalf("unexpected config: %+v", c)
	}
}

func TestConfigDefaults(t *testing.T) {
	c := &Config{}
	err := c.applyFile([]byte(`{
		"teslaPlate": "A", "zoePlate": "B", "teslaVins": ["vin1"],
		"work": {"lat": 59.3, "lon": 18.0},
		"telemetry": {"tlsCert": "c", "tlsKey": "k"}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Work.RadiusM != 250 || c.TelemetryListen != "127.0.0.1:8443" || c.TelemetryPort != 443 ||
		c.MorningEnd != 10*time.Hour || c.TelemetryStale != 72*time.Hour || !c.TeslaVINs["VIN1"] ||
		len(c.Workdays) != 5 || c.StatePath != "state.json" {
		t.Fatalf("unexpected defaults: %+v", c)
	}
}

func TestConfigScheduleOnly(t *testing.T) {
	c := &Config{}
	if err := c.applyFile([]byte(`{"fallbackSchedule": {"mon": "A", "Friday": "B"}}`)); err != nil {
		t.Fatal(err)
	}
	if c.TelemetryEnabled() || c.FallbackSchedule["Monday"] != "A" || c.FallbackSchedule["Friday"] != "B" {
		t.Fatalf("unexpected config: %+v", c)
	}
}

func TestConfigErrors(t *testing.T) {
	for name, tc := range map[string]struct{ json, want string }{
		"typo":         {`{"teslaPlates": "A"}`, "unknown field"},
		"empty":        {`{}`, "teslaVins"},
		"no work":      {`{"teslaPlate": "A", "zoePlate": "B", "teslaVins": ["V"]}`, "work.lat"},
		"no plates":    {`{"teslaVins": ["V"], "work": {"lat": 1, "lon": 2}}`, "teslaPlate"},
		"bad weekday":  {`{"fallbackSchedule": {"Funday": "A"}}`, "Funday"},
		"bad lat":      {`{"teslaPlate": "A", "zoePlate": "B", "teslaVins": ["V"], "work": {"lat": 91, "lon": 2}, "telemetry": {"tlsCert": "c", "tlsKey": "k"}}`, "out of range"},
		"no tls":       {`{"teslaPlate": "A", "zoePlate": "B", "teslaVins": ["V"], "work": {"lat": 1, "lon": 2}}`, "tlsCert"},
		"bad duration": {`{"telemetryStale": "3 days", "fallbackSchedule": {"Mon": "A"}}`, "telemetryStale"},
	} {
		t.Run(name, func(t *testing.T) {
			err := (&Config{}).applyFile([]byte(tc.json))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}
