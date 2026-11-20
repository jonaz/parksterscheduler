# parksterscheduler

Keeps the right car on a Parkster long-term parking permit.

The Tesla streams its location and gear to this service with
[Tesla Fleet Telemetry](https://developer.tesla.com/docs/fleet-api/fleet-telemetry).
The Tesla is driven by more than one person, so only a Tesla **parked at your
work** counts. On workdays:

- From `morningStart` (05:00) the **default car** (`defaultPlate`, the Zoe) is on the permit.
- When the Tesla is **parked** (not in D/R/N) within `work.radiusM` of your work
  parking, the permit switches to the **Tesla**, a few seconds after parking.
  Driving past your work does not count.
- If the Tesla leaves work again before `morningEnd` (10:00), the permit goes
  back to the default car. After `morningEnd` it never switches back from the Tesla
  the same day, so driving home does not change anything.
- If no telemetry has arrived for `telemetryStale` (72h), the
  `fallbackSchedule` is used and a Pushover warning is sent.

Every permit change and every error is sent to Pushover. If `teslaVins` is
empty the service runs on `fallbackSchedule` only, like before.

## Build and install

```sh
go build -o parksterscheduler .
cp config.example.json config.json   # then edit it
sudo cp parksterscheduler config.json config.env /opt/parksterscheduler/
sudo cp parksterscheduler.service /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable --now parksterscheduler
```

See the comments at the top of `parksterscheduler.service` for user and
permission setup.

Configuration is split in two files. Don't commit your real values: the repo is
public, `config.env` here is only a template, and `config.json` is gitignored.

- `config.env`: only the Parkster and Pushover credentials, loaded by systemd.
- `config.json`: everything else. See `config.example.json`; optional fields
  can be left out and get the defaults shown there. Unknown fields are an
  error, so typos are caught at startup.

`work` is your work parking in decimal degrees: right-click the spot in Google
Maps and copy the numbers.

## One-time setup for telemetry

### 1. Server certificate

The car connects with mutual TLS, so this service holds its own certificate.
A long-lived private CA is simplest; its certificate goes into the car's config.

```sh
cd /opt/parksterscheduler/tls
HOST=tesla.example.com
openssl ecparam -name prime256v1 -genkey -noout -out ca.key
openssl req -x509 -new -key ca.key -sha256 -days 3650 -subj "/CN=parksterscheduler CA" -out ca.crt
openssl ecparam -name prime256v1 -genkey -noout -out server.key
openssl req -new -key server.key -subj "/CN=$HOST" -out server.csr
openssl x509 -req -in server.csr -CA ca.crt -CAkey ca.key -CAcreateserial -days 3650 -sha256 \
  -extfile <(printf "subjectAltName=DNS:$HOST\nextendedKeyUsage=serverAuth") -out server.crt
```

Keep `ca.key` offline once `server.crt` is made.

### 2. Traefik (TLS passthrough)

Traefik must not terminate TLS for this hostname, or the car's client
certificate never reaches the service. File provider example:

```yaml
tcp:
  routers:
    tesla-telemetry:
      entryPoints: [websecure]
      rule: "HostSNI(`tesla.example.com`)"
      service: parksterscheduler-telemetry
      tls:
        passthrough: true
  services:
    parksterscheduler-telemetry:
      loadBalancer:
        servers:
          - address: "127.0.0.1:8443"   # telemetry.listen; use the host IP if Traefik runs in Docker
```

Check it from outside with Tesla's
[check_server_cert.sh](https://github.com/teslamotors/fleet-telemetry/blob/main/tools/check_server_cert.sh).

### 3. Tesla developer app

Follow Tesla's [Fleet API setup](https://developer.tesla.com/docs/fleet-api/getting-started/what-is-fleet-api):

1. Create an application on developer.tesla.com with the `vehicle_device_data`
   and `vehicle_location` scopes. Add a payment method and set a low billing
   limit (it starts at 0, which disables the app).
2. Generate the application key pair and host the public key at
   `https://<your domain>/.well-known/appspecific/com.tesla.3p.public-key.pem`.
3. Get a partner token and call the `register` endpoint for your region (EU).
4. Pair the virtual key with the car: open `https://tesla.com/_ak/<your domain>`
   on the phone with the Tesla app, and approve it in the car.
5. Log in once with OAuth to get a user token for your account.

### 4. Configure the car

Generate the request body (needs `teslaVins`, `telemetry.host` and `telemetry.tlsCa`):

```sh
/opt/parksterscheduler/parksterscheduler -config /opt/parksterscheduler/config.json \
  -print-telemetry-config > telemetry_config.json
```

It streams `Location` (every 10 s at most, only while it changes) and `Gear`. Check the
fields against the current `fleet_telemetry_config` documentation before sending.

The config must be signed with the application private key, so send it through
Tesla's [vehicle-command HTTP proxy](https://github.com/teslamotors/vehicle-command):

```sh
curl --cacert proxy-cert.pem -H "Authorization: Bearer $USER_TOKEN" \
  -H "Content-Type: application/json" --data @telemetry_config.json \
  https://localhost:4443/api/1/vehicles/fleet_telemetry_config
```

Then wait until `GET /api/1/vehicles/<VIN>/fleet_telemetry_config` reports
`synced: true`. Connection problems show up in `fleet_telemetry_errors` and in
this service's log (`journalctl -u parksterscheduler`). The config expires
after a year (`exp`); re-send it before then.

The vehicle needs firmware 2024.26 or later.
