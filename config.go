package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

// Config holds all settings. Secrets come from flags or environment variables
// (config.env); everything else comes from the JSON config file.
type Config struct {
	// Secrets (flags / environment).
	ParksterUsername string
	ParksterPassword string
	PushoverAPIKey   string
	PushoverUserKey  string

	// From the JSON config file.
	TeslaPlate       string
	DefaultPlate     string
	TeslaVINs        map[string]bool
	Work             Geofence
	TelemetryListen  string
	TelemetryHost    string // public hostname the car connects to; only used by -print-telemetry-config
	TelemetryPort    int
	TLSCert          string
	TLSKey           string
	TLSCA            string // CA that signed TLSCert; only used by -print-telemetry-config
	StatePath        string
	Workdays         map[time.Weekday]bool
	MorningStart     time.Duration // time of day when the daily decision starts
	MorningEnd       time.Duration // after this, never switch Tesla -> default car the same day
	TelemetryStale   time.Duration // no telemetry for this long => fall back to schedule
	Location         *time.Location
	FallbackSchedule Schedule

	PrintTelemetryConfig bool
}

// TelemetryEnabled reports whether car detection via Fleet Telemetry is configured.
func (c *Config) TelemetryEnabled() bool {
	return len(c.TeslaVINs) > 0
}

// fileConfig is the JSON config file. Optional fields have defaults.
type fileConfig struct {
	TeslaPlate   string   `json:"teslaPlate"`
	DefaultPlate string   `json:"defaultPlate"`
	TeslaVINs    []string `json:"teslaVins"`
	Work         *struct {
		Lat     *float64 `json:"lat"`
		Lon     *float64 `json:"lon"`
		RadiusM float64  `json:"radiusM"`
	} `json:"work"`
	Telemetry struct {
		Listen  string `json:"listen"`
		Host    string `json:"host"`
		Port    int    `json:"port"`
		TLSCert string `json:"tlsCert"`
		TLSKey  string `json:"tlsKey"`
		TLSCA   string `json:"tlsCa"`
	} `json:"telemetry"`
	StatePath        string            `json:"statePath"`
	Workdays         []string          `json:"workdays"`
	MorningStart     string            `json:"morningStart"`
	MorningEnd       string            `json:"morningEnd"`
	TelemetryStale   string            `json:"telemetryStale"`
	Timezone         string            `json:"timezone"`
	FallbackSchedule map[string]string `json:"fallbackSchedule"`
}

func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func loadConfig(args []string) (*Config, error) {
	fs := flag.NewFlagSet("parksterscheduler", flag.ContinueOnError)
	c := &Config{}
	var configPath string

	fs.StringVar(&configPath, "config", envOr("CONFIG_PATH", "config.json"), "JSON config file (CONFIG_PATH)")
	fs.StringVar(&c.ParksterUsername, "username", envOr("PARKSTER_USERNAME", ""), "Parkster account email (PARKSTER_USERNAME)")
	fs.StringVar(&c.ParksterPassword, "password", envOr("PARKSTER_PASSWORD", ""), "Parkster account password (PARKSTER_PASSWORD)")
	fs.StringVar(&c.PushoverAPIKey, "pushover-api-key", envOr("PUSHOVER_API_KEY", ""), "Pushover API token (PUSHOVER_API_KEY)")
	fs.StringVar(&c.PushoverUserKey, "pushover-user-key", envOr("PUSHOVER_USER_KEY", ""), "Pushover user key (PUSHOVER_USER_KEY)")
	fs.BoolVar(&c.PrintTelemetryConfig, "print-telemetry-config", false, "Print the fleet_telemetry_config request body and exit")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	if err := c.applyFile(raw); err != nil {
		return nil, fmt.Errorf("%s: %w", configPath, err)
	}

	if !c.PrintTelemetryConfig && (c.ParksterUsername == "" || c.ParksterPassword == "") {
		return nil, fmt.Errorf("PARKSTER_USERNAME and PARKSTER_PASSWORD are required")
	}
	return c, nil
}

// applyFile parses and validates the JSON config file into c.
func (c *Config) applyFile(raw []byte) error {
	var f fileConfig
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields() // catch typos in field names
	if err := dec.Decode(&f); err != nil {
		return err
	}

	var err error
	if c.Location, err = time.LoadLocation(orDefault(f.Timezone, "Europe/Stockholm")); err != nil {
		return fmt.Errorf("timezone: %w", err)
	}
	if c.Workdays, err = parseWorkdays(f.Workdays); err != nil {
		return err
	}
	if c.MorningStart, err = parseClock(orDefault(f.MorningStart, "05:00")); err != nil {
		return fmt.Errorf("morningStart: %w", err)
	}
	if c.MorningEnd, err = parseClock(orDefault(f.MorningEnd, "10:00")); err != nil {
		return fmt.Errorf("morningEnd: %w", err)
	}
	if c.TelemetryStale, err = time.ParseDuration(orDefault(f.TelemetryStale, "72h")); err != nil {
		return fmt.Errorf("telemetryStale: %w", err)
	}

	c.FallbackSchedule = Schedule{}
	for day, plate := range f.FallbackSchedule {
		d, ok := weekdayByName(day)
		if !ok {
			return fmt.Errorf("fallbackSchedule: unknown weekday %q", day)
		}
		c.FallbackSchedule[d.String()] = plate
	}

	c.TeslaPlate, c.DefaultPlate = f.TeslaPlate, f.DefaultPlate
	c.TeslaVINs = map[string]bool{}
	for _, v := range f.TeslaVINs {
		if v = strings.ToUpper(strings.TrimSpace(v)); v != "" {
			c.TeslaVINs[v] = true
		}
	}
	c.TelemetryListen = orDefault(f.Telemetry.Listen, "127.0.0.1:8443")
	c.TelemetryHost = f.Telemetry.Host
	c.TelemetryPort = f.Telemetry.Port
	if c.TelemetryPort == 0 {
		c.TelemetryPort = 443
	}
	c.TLSCert, c.TLSKey, c.TLSCA = f.Telemetry.TLSCert, f.Telemetry.TLSKey, f.Telemetry.TLSCA
	c.StatePath = orDefault(f.StatePath, "state.json")

	if !c.TelemetryEnabled() {
		if len(c.FallbackSchedule) == 0 {
			return fmt.Errorf("set teslaVins (telemetry) or fallbackSchedule, or both")
		}
		return nil
	}

	if c.TeslaPlate == "" || c.DefaultPlate == "" {
		return fmt.Errorf("teslaPlate and defaultPlate are required when teslaVins is set")
	}
	if f.Work == nil || f.Work.Lat == nil || f.Work.Lon == nil {
		return fmt.Errorf("work.lat and work.lon are required when teslaVins is set")
	}
	c.Work = Geofence{Lat: *f.Work.Lat, Lon: *f.Work.Lon, RadiusM: f.Work.RadiusM}
	if c.Work.RadiusM == 0 {
		c.Work.RadiusM = 250
	}
	if c.Work.Lat < -90 || c.Work.Lat > 90 || c.Work.Lon < -180 || c.Work.Lon > 180 || c.Work.RadiusM < 0 {
		return fmt.Errorf("work location is out of range: %+v", c.Work)
	}
	if !c.PrintTelemetryConfig && (c.TLSCert == "" || c.TLSKey == "") {
		return fmt.Errorf("telemetry.tlsCert and telemetry.tlsKey are required when teslaVins is set")
	}
	return nil
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func parseClock(s string) (time.Duration, error) {
	t, err := time.Parse("15:04", strings.TrimSpace(s))
	if err != nil {
		return 0, err
	}
	return time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute, nil
}

// parseWorkdays defaults to Monday-Friday when the list is empty.
func parseWorkdays(days []string) (map[time.Weekday]bool, error) {
	if len(days) == 0 {
		days = []string{"Mon", "Tue", "Wed", "Thu", "Fri"}
	}
	out := map[time.Weekday]bool{}
	for _, p := range days {
		d, ok := weekdayByName(p)
		if !ok {
			return nil, fmt.Errorf("workdays: unknown weekday %q", p)
		}
		out[d] = true
	}
	return out, nil
}

// weekdayByName accepts "Monday", "monday" or "Mon".
func weekdayByName(s string) (time.Weekday, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	for d := time.Sunday; d <= time.Saturday; d++ {
		name := strings.ToLower(d.String())
		if s == name || s == name[:3] {
			return d, true
		}
	}
	return 0, false
}
