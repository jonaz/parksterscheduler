package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/teslamotors/fleet-telemetry/messages"
	"github.com/teslamotors/fleet-telemetry/protos"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func newTestCA(t *testing.T, cn string) *testCA {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return &testCA{cert: cert, key: key}
}

func (ca *testCA) issue(t *testing.T, cn string, client bool) tls.Certificate {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	if client {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	} else {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		tmpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func writePEM(t *testing.T, dir string, c tls.Certificate) (certFile, keyFile string) {
	t.Helper()
	certFile, keyFile = filepath.Join(dir, "server.crt"), filepath.Join(dir, "server.key")
	keyDER, _ := x509.MarshalECPrivateKey(c.PrivateKey.(*ecdsa.PrivateKey))
	_ = os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Certificate[0]}), 0o600)
	_ = os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600)
	return
}

// startTelemetry starts the receiver with a fake "Tesla Issuing CA" for client certs.
func startTelemetry(t *testing.T) (url string, vehicleCA, serverCA *testCA, store *Store, updates chan struct{}) {
	t.Helper()
	dir := t.TempDir()
	vehicleCA = newTestCA(t, "Tesla Issuing CA") // issuer name recognised by CreateIdentityFromCert
	serverCA = newTestCA(t, "my server CA")
	certFile, keyFile := writePEM(t, dir, serverCA.issue(t, "127.0.0.1", false))

	pool := x509.NewCertPool()
	pool.AddCert(vehicleCA.cert)
	tlsCfg, err := TelemetryTLSConfig(certFile, keyFile, pool)
	if err != nil {
		t.Fatal(err)
	}
	store, err = loadStore(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	updates = make(chan struct{}, 10)
	srv := httptest.NewUnstartedServer(NewTelemetryServer(map[string]bool{"5YJ3E7EB0KF000001": true}, store, func() { updates <- struct{}{} }))
	srv.TLS = tlsCfg
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return "wss://" + strings.TrimPrefix(srv.URL, "https://"), vehicleCA, serverCA, store, updates
}

func dialCar(url string, serverCA *testCA, clientCert tls.Certificate) (*websocket.Conn, error) {
	roots := x509.NewCertPool()
	roots.AddCert(serverCA.cert)
	d := websocket.Dialer{TLSClientConfig: &tls.Config{RootCAs: roots, Certificates: []tls.Certificate{clientCert}}}
	ws, _, err := d.Dial(url, nil)
	return ws, err
}

func TestTelemetryEndToEnd(t *testing.T) {
	url, vehicleCA, serverCA, store, updates := startTelemetry(t)
	ws, err := dialCar(url, serverCA, vehicleCA.issue(t, "5YJ3E7EB0KF000001", true))
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()

	created := time.Date(2026, 11, 17, 7, 35, 0, 0, time.UTC)
	payload, _ := proto.Marshal(&protos.Payload{
		CreatedAt: timestamppb.New(created),
		Data: []*protos.Datum{
			{Key: protos.Field_Location, Value: &protos.Value{Value: &protos.Value_LocationValue{LocationValue: &protos.LocationValue{Latitude: 59.3346, Longitude: 18.0632}}}},
			{Key: protos.Field_Gear, Value: &protos.Value{Value: &protos.Value_ShiftStateValue{ShiftStateValue: protos.ShiftState_ShiftStateD}}},
		},
	})
	msg, _ := (&messages.StreamMessage{
		TXID:         []byte("tx-1"),
		SenderID:     []byte("vehicle_device.5YJ3E7EB0KF000001"),
		MessageTopic: []byte("V"),
		Payload:      payload,
		CreatedAt:    uint32(created.Unix()),
	}).ToBytes()
	if err := ws.WriteMessage(websocket.BinaryMessage, msg); err != nil {
		t.Fatal(err)
	}

	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, ackData, err := ws.ReadMessage()
	if err != nil {
		t.Fatalf("no ack: %v", err)
	}
	ack, err := messages.StreamAckMessageFromBytes(ackData)
	if err != nil {
		t.Fatalf("bad ack: %v", err)
	}
	if string(ack.TXID) != "tx-1" || ack.Topic() != "V" {
		t.Fatalf("ack for wrong message: txid=%s topic=%s", ack.TXID, ack.Topic())
	}

	select {
	case <-updates:
	case <-time.After(5 * time.Second):
		t.Fatal("onUpdate not called")
	}
	st := store.Snapshot()
	if st.Lat != 59.3346 || st.Lon != 18.0632 || st.Gear != "D" || !st.LocationAt.Equal(created) {
		t.Fatalf("unexpected state: %+v", st)
	}
}

func TestTelemetryRejectsUnknownVIN(t *testing.T) {
	url, vehicleCA, serverCA, _, _ := startTelemetry(t)
	if _, err := dialCar(url, serverCA, vehicleCA.issue(t, "5YJ3E7EB0KF999999", true)); err == nil {
		t.Fatal("connection with unknown VIN should be rejected")
	}
}

func TestTelemetryRejectsUntrustedClientCert(t *testing.T) {
	url, _, serverCA, _, _ := startTelemetry(t)
	other := newTestCA(t, "Tesla Issuing CA") // same name, different key
	if _, err := dialCar(url, serverCA, other.issue(t, "5YJ3E7EB0KF000001", true)); err == nil {
		t.Fatal("certificate from an untrusted CA should be rejected")
	}
}

func TestLegacyLocationAndGearStrings(t *testing.T) {
	u, _ := extractUpdate(&protos.Payload{Data: []*protos.Datum{
		{Key: protos.Field_Location, Value: &protos.Value{Value: &protos.Value_StringValue{StringValue: "(37.412374 N, 122.145867 W)"}}},
		{Key: protos.Field_Gear, Value: &protos.Value{Value: &protos.Value_StringValue{StringValue: "ShiftStateR"}}},
	}}, time.Now())
	if u.Location == nil || u.Location[0] != 37.412374 || u.Location[1] != -122.145867 || u.Gear != "R" {
		t.Fatalf("unexpected update: %+v loc=%v", u, u.Location)
	}
}

func TestEmbeddedTeslaCA(t *testing.T) {
	if !x509.NewCertPool().AppendCertsFromPEM(teslaProdCA) {
		t.Fatal("embedded Tesla CA does not parse")
	}
}
