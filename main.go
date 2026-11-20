package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"
)

// Schedule maps a day of the week ("Monday") to a license plate number. With
// telemetry enabled it is only used when the car has not reported in.
type Schedule map[string]string

func main() {
	cfg, err := loadConfig(os.Args[1:])
	if err != nil {
		log.Fatalf("❌ %v", err)
	}

	if cfg.PrintTelemetryConfig {
		if err := printTelemetryConfig(cfg); err != nil {
			log.Fatalf("❌ %v", err)
		}
		return
	}

	if cfg.TelemetryEnabled() && len(cfg.FallbackSchedule) == 0 {
		log.Println("ℹ️ No fallbackSchedule; nothing is changed if telemetry stops.")
	}

	notifier := newPushover(cfg.PushoverAPIKey, cfg.PushoverUserKey)
	parkster := newParksterClient(cfg.ParksterUsername, cfg.ParksterPassword)

	var store *Store
	if cfg.TelemetryEnabled() {
		if store, err = loadStore(cfg.StatePath); err != nil {
			log.Fatalf("❌ Failed to load state: %v", err)
		}
	}

	ctrl := NewController(cfg, store, cfg.FallbackSchedule, parkster, notifier)

	if cfg.TelemetryEnabled() {
		tlsCfg, err := TelemetryTLSConfig(cfg.TLSCert, cfg.TLSKey, nil)
		if err != nil {
			log.Fatalf("❌ %v", err)
		}
		srv := &http.Server{
			Addr:              cfg.TelemetryListen,
			Handler:           NewTelemetryServer(cfg.TeslaVINs, store, ctrl.Trigger),
			TLSConfig:         tlsCfg,
			ReadHeaderTimeout: 10 * time.Second,
		}
		go func() {
			log.Printf("📡 Fleet Telemetry server listening on %s", cfg.TelemetryListen)
			if err := srv.ListenAndServeTLS("", ""); err != nil {
				log.Fatalf("❌ Telemetry server stopped: %v", err)
			}
		}()
		log.Printf("🚗 Tesla (%s) when it is parked at work, otherwise the default car (%s).", cfg.TeslaPlate, cfg.DefaultPlate)
	} else {
		log.Println("🗓️ teslaVins not set: using fallbackSchedule only.")
	}

	ctrl.Run()
}

// printTelemetryConfig prints the body for the fleet_telemetry_config request,
// to be sent through Tesla's vehicle-command proxy (which signs it).
func printTelemetryConfig(cfg *Config) error {
	if cfg.TelemetryHost == "" || cfg.TLSCA == "" || len(cfg.TeslaVINs) == 0 {
		return fmt.Errorf("teslaVins, telemetry.host and telemetry.tlsCa are required in the config file")
	}
	ca, err := os.ReadFile(cfg.TLSCA)
	if err != nil {
		return err
	}
	vins := make([]string, 0, len(cfg.TeslaVINs))
	for v := range cfg.TeslaVINs {
		vins = append(vins, v)
	}
	body := map[string]any{
		"vins": vins,
		"config": map[string]any{
			"hostname":     cfg.TelemetryHost,
			"port":         cfg.TelemetryPort,
			"ca":           string(ca),
			"prefer_typed": true,
			"exp":          time.Now().AddDate(1, 0, 0).Unix(),
			"fields": map[string]any{
				"Location": map[string]int{"interval_seconds": 10},
				"Gear":     map[string]int{"interval_seconds": 1},
			},
		},
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(body)
}
