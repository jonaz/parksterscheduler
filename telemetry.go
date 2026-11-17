package main

// Minimal Fleet Telemetry receiver. The wire format (mTLS + websocket +
// flatbuffers envelope + protobuf payload) follows Tesla's reference server
// github.com/teslamotors/fleet-telemetry; we reuse its message and proto
// packages but not the server itself.

import (
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/teslamotors/fleet-telemetry/messages"
	"github.com/teslamotors/fleet-telemetry/messages/tesla"
	"github.com/teslamotors/fleet-telemetry/protos"
	"google.golang.org/protobuf/proto"
)

// Tesla's production CA bundle for vehicle client certificates, copied from
// fleet-telemetry config/files/prod_ca.crt (Apache-2.0).
//
//go:embed tesla_prod_ca.crt
var teslaProdCA []byte

const vehicleDataTopic = "V"

// TelemetryServer accepts websocket connections from cars and feeds the store.
type TelemetryServer struct {
	allowedVINs map[string]bool
	store       *Store
	onUpdate    func()
	upgrader    websocket.Upgrader
}

func NewTelemetryServer(allowedVINs map[string]bool, store *Store, onUpdate func()) *TelemetryServer {
	return &TelemetryServer{
		allowedVINs: allowedVINs,
		store:       store,
		onUpdate:    onUpdate,
		upgrader: websocket.Upgrader{
			CheckOrigin: func(*http.Request) bool { return true }, // cars, not browsers
		},
	}
}

// TLSConfig requires a client certificate issued by Tesla.
func TelemetryTLSConfig(certFile, keyFile string, clientCAs *x509.CertPool) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load server certificate: %w", err)
	}
	if clientCAs == nil {
		clientCAs = x509.NewCertPool()
		if !clientCAs.AppendCertsFromPEM(teslaProdCA) {
			return nil, fmt.Errorf("could not load embedded Tesla CA")
		}
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientCAs:    clientCAs,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS12,
	}, nil
}

func (s *TelemetryServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	vin, err := s.identify(r)
	if err != nil {
		log.Printf("🚫 Telemetry connection from %s rejected: %v", r.RemoteAddr, err)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	ws, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("❌ Telemetry websocket upgrade failed: %v", err)
		return
	}
	defer ws.Close()
	ws.SetReadLimit(4 << 20)
	log.Printf("🔌 Telemetry connected: %s", vin)
	defer log.Printf("🔌 Telemetry disconnected: %s", vin)

	for {
		msgType, data, err := ws.ReadMessage()
		if err != nil {
			return
		}
		if msgType != websocket.BinaryMessage {
			continue
		}
		ack := s.handleMessage(vin, data)
		if ack == nil {
			continue
		}
		_ = ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if err := ws.WriteMessage(websocket.BinaryMessage, ack); err != nil {
			log.Printf("❌ Telemetry ack failed for %s: %v", vin, err)
			return
		}
	}
}

// identify returns the VIN from the verified client certificate.
func (s *TelemetryServer) identify(r *http.Request) (string, error) {
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.VerifiedChains[0]) == 0 {
		return "", fmt.Errorf("no verified client certificate")
	}
	clientType, vin, err := messages.CreateIdentityFromCert(r.TLS.VerifiedChains[0][0])
	if err != nil {
		return "", err
	}
	if clientType != "vehicle_device" {
		return "", fmt.Errorf("client type %q is not a vehicle", clientType)
	}
	if !s.allowedVINs[strings.ToUpper(vin)] {
		return "", fmt.Errorf("VIN %s is not in TESLA_VIN", vin)
	}
	return strings.ToUpper(vin), nil
}

// handleMessage decodes one message and returns the ack to send back, or nil.
// Every decodable message is acked, otherwise the car keeps resending it.
func (s *TelemetryServer) handleMessage(vin string, data []byte) []byte {
	sm, err := messages.StreamMessageFromBytes(data)
	if err != nil {
		// Not a stream message; ack by txid if we can so the car does not resend it.
		env, _, envErr := tesla.FlatbuffersEnvelopeFromBytes(data)
		if envErr != nil {
			log.Printf("⚠️ Unparseable telemetry message from %s: %v", vin, err)
			return nil
		}
		return ackBytes(env.TxidBytes(), env.TopicBytes())
	}

	if sm.Topic() == vehicleDataTopic {
		var payload protos.Payload
		if err := proto.Unmarshal(sm.Payload, &payload); err != nil {
			log.Printf("⚠️ Bad vehicle data payload from %s: %v", vin, err)
		} else if u, ok := extractUpdate(&payload, time.Now()); ok {
			if err := s.store.Apply(u); err != nil {
				log.Printf("❌ Could not save vehicle state: %v", err)
			}
			if s.onUpdate != nil {
				s.onUpdate()
			}
		}
	}
	return ackBytes(sm.TXID, sm.MessageTopic)
}

func ackBytes(txid, topic []byte) []byte {
	b, _ := (&messages.StreamAckMessage{TXID: txid, MessageTopic: topic}).ToBytes()
	return b
}

// extractUpdate pulls location and gear out of a vehicle data payload.
func extractUpdate(p *protos.Payload, received time.Time) (TelemetryUpdate, bool) {
	u := TelemetryUpdate{At: received}
	if ts := p.GetCreatedAt(); ts != nil && ts.IsValid() {
		u.At = ts.AsTime()
	}
	if u.At.After(received.Add(5 * time.Minute)) {
		u.At = received // don't trust timestamps from the future
	}

	for _, d := range p.GetData() {
		v := d.GetValue()
		if v == nil || v.GetInvalid() {
			continue
		}
		switch d.GetKey() {
		case protos.Field_Location:
			if loc := v.GetLocationValue(); loc != nil {
				u.Location = &[2]float64{loc.GetLatitude(), loc.GetLongitude()}
			} else if lat, lon, ok := parseLocationString(v.GetStringValue()); ok {
				u.Location = &[2]float64{lat, lon}
			}
		case protos.Field_Gear:
			u.Gear = gearFromValue(v)
		}
	}
	return u, true
}

func gearFromValue(v *protos.Value) string {
	if _, ok := v.GetValue().(*protos.Value_ShiftStateValue); ok {
		return gearLetter(v.GetShiftStateValue().String())
	}
	return gearLetter(v.GetStringValue())
}

// gearLetter maps "ShiftStateD", "D" etc. to P/R/N/D, or "" when unknown.
func gearLetter(s string) string {
	switch strings.TrimPrefix(strings.TrimSpace(s), "ShiftState") {
	case "P":
		return "P"
	case "R":
		return "R"
	case "N":
		return "N"
	case "D":
		return "D"
	}
	return ""
}

// parseLocationString parses the legacy string form "(59.334591 N, 18.063240 E)".
func parseLocationString(s string) (lat, lon float64, ok bool) {
	var latQ, lonQ string
	n, err := fmt.Sscanf(s, "(%f %1s, %f %1s)", &lat, &latQ, &lon, &lonQ)
	if err != nil || n != 4 || !strings.Contains("NS", latQ) || !strings.Contains("EW", lonQ) {
		return 0, 0, false
	}
	if latQ == "S" {
		lat = -lat
	}
	if lonQ == "W" {
		lon = -lon
	}
	return lat, lon, true
}
