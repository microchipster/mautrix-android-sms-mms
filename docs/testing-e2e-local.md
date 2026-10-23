# Local end-to-end test: app pairing

This runbook exercises the whole app-pairing flow of the bridge against a throwaway
[Synapse](https://github.com/element-hq/synapse) in Docker, with `tools/pairsim` standing in for
the phone app:

```
!sms login app-pair -> QR/code -> GET /info -> POST /pair/start -> ECDSA P-256 signature
  -> POST /pair/complete -> token -> GET /queue (long-poll) -> POST /events -> POST /heartbeat
  -> portal room created from a message.new event -> Matrix message -> send_sms command
  -> send.result accepted -> echo suppressed -> mark_read command
```

**This is a local development test.** It uses plain HTTP, fake phone numbers, a throwaway
homeserver and a fixed test password. Do not reuse any of it in production, and do not run
`pairsim` against a real bridge.

Everything below was executed on 2026-10-23 with `matrixdotorg/synapse:latest` and the bridge at
commit `63d83ba` plus the pairing-logger fix. Paths use the scratch directory
`/tmp/opencode/e2e-pairing`; substitute your own if you like.

## Prerequisites

- Docker (the test uses `--network host`, so no port publishing is needed).
- Go toolchain (to build the bridge and `pairsim`).
- `curl` and `python3` (JSON plumbing).
- Ports 8008 (Synapse), 29321 (appservice) and 29322 (pairing API) free on the host.

```sh
WORK=/tmp/opencode/e2e-pairing
mkdir -p "$WORK/bin" "$WORK/bridge/logs" "$WORK/synapse"
docker pull matrixdotorg/synapse:latest   # the only image used
```

## 1. Build the bridge and the simulator

```sh
cd /path/to/mautrix-android-sms-mms
go build -o "$WORK/bin/mautrix-android-sms-mms" ./cmd/mautrix-android-sms-mms
go build -o "$WORK/bin/pairsim" ./tools/pairsim
"$WORK/bin/pairsim" --help
```

## 2. Throwaway Synapse

Generate the config (server name `localhost`, stats off):

```sh
docker run --rm --network host \
  -e SYNAPSE_SERVER_NAME=localhost -e SYNAPSE_REPORT_STATS=no \
  -v "$WORK/synapse:/data" matrixdotorg/synapse:latest generate
```

`generate` already writes a `registration_shared_secret`; append the appservice registration
path (the file is installed in step 4):

```sh
printf 'app_service_config_files:\n  - /data/appservice-registration.yaml\n' \
  | sudo tee -a "$WORK/synapse/homeserver.yaml"
```

We run Synapse with `--network host` so it can reach the appservice at `http://127.0.0.1:29321`
and the pairing API at `:29322` without `host.docker.internal` tricks.

## 3. Bridge configuration

```sh
cd "$WORK/bridge"
"$WORK/bin/mautrix-android-sms-mms" -c config.yaml -e
```

`-e` writes the sample `config.yaml` but not the network config; edit `config.yaml` so the
relevant options are:

```yaml
network:
    simple-config.yaml

bridge:
    command_prefix: '!sms'
    personal_filtering_spaces: true
    permissions:
        "*": user
        "@testuser:localhost": admin

database:
    type: sqlite3-fk-wal
    uri: file:/tmp/opencode/e2e-pairing/bridge/bridge.db?_txlock=immediate

homeserver:
    address: http://localhost:8008
    domain: localhost

appservice:
    address: http://127.0.0.1:29321
    public_address:            # empty is fine locally
    hostname: 127.0.0.1
    port: 29321

matrix:
    federate_rooms: false

encryption:
    allow: true                # required by the management-room encryption gate
```

Write `simple-config.yaml` (the pairing listener is a connector option, not a bridge-core one):

```yaml
pairing:
    enabled: true
    listen: ":29322"
    public_url: "http://127.0.0.1:29322"
```

Generate the appservice registration (also writes `as_token`/`hs_token` back into `config.yaml`):

```sh
"$WORK/bin/mautrix-android-sms-mms" -c config.yaml -r registration.yaml -g
```

Note the bot username from the registration (`sender_localpart` namespace), here
`@android-sms-mmsbot:localhost`.

## 4. Start Synapse and register the test user

```sh
sudo cp "$WORK/bridge/registration.yaml" "$WORK/synapse/appservice-registration.yaml"
sudo chown 991:991 "$WORK/synapse/appservice-registration.yaml"

docker run -d --name e2e-synapse --network host \
  -v "$WORK/synapse:/data" matrixdotorg/synapse:latest

docker exec e2e-synapse register_new_matrix_user \
  -u testuser -p testpass123 -a \
  -c /data/homeserver.yaml http://localhost:8008
```

## 5. Start the bridge

```sh
cd "$WORK/bridge"
nohup "$WORK/bin/mautrix-android-sms-mms" -c config.yaml > "$WORK/logs/bridge-stdout.log" 2>&1 &
sleep 5
curl -sS http://127.0.0.1:29322/_smsmms/v1/info
# {"protocol_version":1,"bridge_name":"mautrix-android-sms-mms","homeserver_url":"http://localhost:8008","pairing_enabled":true}
grep -a 'App pairing API listening' "$WORK/logs/bridge-stdout.log"
```

## 6. Log the test user in and create the management room

```sh
TOKEN=$(curl -sS -XPOST http://localhost:8008/_matrix/client/v3/login \
  -H 'Content-Type: application/json' \
  -d '{"type":"m.login.password","identifier":{"type":"m.id.user","user":"testuser"},"password":"testpass123"}' \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["access_token"])')

ROOM=$(curl -sS -XPOST "http://localhost:8008/_matrix/client/v3/createRoom?access_token=$TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"preset":"private_chat","invite":["@android-sms-mmsbot:localhost"],"is_direct":true,"name":"e2e mgmt"}' \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["room_id"])')
```

The bot accepts the invite and marks the room as the management room (see the bridge log:
`Accepted invite to room as bot`).

Grant the bot power to enable encryption in the room:

```sh
python3 - "$TOKEN" "$ROOM" <<'EOF'
import json, sys, urllib.request
token, room = sys.argv[1], sys.argv[2]
base = f"http://localhost:8008/_matrix/client/v3/rooms/{room}/state/m.room.power_levels"
req = urllib.request.Request(base, headers={"Authorization": f"Bearer {token}"})
cur = json.load(urllib.request.urlopen(req))
cur.setdefault("users", {})["@android-sms-mmsbot:localhost"] = 100
cur.setdefault("events", {})["m.room.encryption"] = 0
put = urllib.request.Request(base, data=json.dumps(cur).encode(), method="PUT",
                             headers={"Authorization": f"Bearer {token}", "Content-Type": "application/json"})
urllib.request.urlopen(put)
EOF
```

Why: Synapse's `private_chat` preset requires power level 100 for `m.room.encryption` while the
bot is level 0, so the management-room encryption gate cannot establish encryption and every
command gets a "room encryption could not be verified" notice. (The room must be encrypted
before the bridge processes commands in it; a real client's room owner would do the same.)

Send the login command and read the pairing code from the bridge's debug log:

```sh
curl -sS -XPUT "http://localhost:8008/_matrix/client/v3/rooms/$ROOM/send/m.room.message/e2e-login-1" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"msgtype":"m.text","body":"!sms login app-pair"}'

grep -a 'Created login process' "$WORK/logs/bridge-stdout.log" | tail -1
```

The `first_step=` JSON contains the payload and the code, e.g.:

```
"data":"smsmms://pair?b=http%3A%2F%2F127.0.0.1%3A29322&code=M3KRJB3X&hs=http%3A%2F%2Flocalhost%3A8008&v=1"
```

The same QR image/message is posted into the room, but it is megolm-encrypted (the test user
has no E2EE client), so for scripted tests read the code from the log. A real user scans the QR
or reads the text in a Matrix client.

## 7. Pair the simulated phone

```sh
"$WORK/bin/pairsim" \
  -code M3KRJB3X \
  -device-id android-sim-01 \
  -device-name 'Pairsim Pixel'
```

Expected output:

```
using bridge http://127.0.0.1:29322 as device "android-sim-01" (Pairsim Pixel)
bridge info: name="mautrix-android-sms-mms" protocol=1 pairing_enabled=true homeserver="http://localhost:8008"
pairing session pr_example started, expires_at_ms=1792760447673
paired: device_id="android-sim-01" user="@testuser:localhost" displayname="testuser"
device token: 0hQzRSskPSmZej5IlONKoUUT4bIDd5YYq-b-iGpxZG0
homeserver="http://localhost:8008" bridge="http://127.0.0.1:29322" protocol=1 queue.wait_max_ms=30000
posted 1 event(s), accepted=1 event_cursor=1
seeded incoming SMS sms:1:9001 from +15551234567 in thread 7
listening for commands (Ctrl-C to stop)
heartbeat sent
```

The bridge log now shows the pairing flow (these lines come from the app-pairing component):

```
INF App pairing API listening component=app-pairing listen=[::]:29322
INF Pairing session started by app component=app-pairing device_id=android-sim-01 device_name="Pairsim Pixel" pairing_id=pr_example
INF App completed pairing component=app-pairing device_id=android-sim-01 mxid=@testuser:localhost pairing_id=pr_example
INF Created app-paired login ... device_id=android-sim-01 login_id=<uuid> user_mxid=@testuser:localhost
```

The login metadata is persisted on the bridgev2 login (device ID, SPKI pubkey, token SHA-256):

```sh
python3 - <<'EOF'
import sqlite3
con = sqlite3.connect('/tmp/opencode/e2e-pairing/bridge/bridge.db')
for row in con.execute("select id, remote_name, metadata from user_login"):
    print(row)
EOF
```

`pairsim` flags worth knowing (see `--help` for all): `-bridge-url` (default
`http://127.0.0.1:29322`), `-code`, `-device-id`, `-device-name`, `-mode pair|listen`,
`-seed` (post one fake incoming SMS after pairing), `-thread-id`, `-first-row`/`-first-seq`
(bump them when re-running against the same simulated device so fake IDs and event sequences
stay monotonic), `-remote-number`/`-local-number`.

## 8. Phone events create a portal, Matrix message round-trips

The simulated incoming SMS from step 7 already created a portal. Find the room and join it:

```sh
grep -a 'Matrix room created' "$WORK/logs/bridge-stdout.log" | tail -1
# ... portal_id=thread-45960705-1-7 ... room_id=!portal-room:example.com

PORTAL='!portal-room:example.com'
curl -sS -XPOST "http://localhost:8008/_matrix/client/v3/join/$PORTAL" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -d '{}'

curl -sS -XPUT "http://localhost:8008/_matrix/client/v3/rooms/$PORTAL/send/m.room.message/e2e-m2p-1" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"msgtype":"m.text","body":"hello from Matrix"}'
```

`pairsim` receives the command and answers it:

```
command seq=1 type=send_sms {"seq":1,"type":"send_sms","send_id":"$event_id","thread_id":7,"sub_id":1,"recipients":["+15550001111","+15551234567"],"text":"hello from Matrix"}
posted 2 event(s), accepted=2 event_cursor=3
answered send_sms send_id="$BcQk..." with sms:1:9002 (row exists, pending=false)
```

The bridge resolves the send and does not import the phone's echo as a second message:

```
INF Successfully sent message to phone ... message_id=sms:1:9002 pending=false portal_id=thread-45960705-1-7
DBG Ignoring duplicate message ... existing_mxid=$event_id portal_id=thread-45960705-1-7
```

Check the room timeline (one incoming SMS, one outgoing Matrix message, no duplicate):

```sh
curl -sS "http://localhost:8008/_matrix/client/v3/rooms/$PORTAL/messages?dir=b&limit=10" \
  -H "Authorization: Bearer $TOKEN" | python3 -c "
import json,sys
for e in reversed(json.load(sys.stdin)['chunk']):
    if e['type'] == 'm.room.message':
        print(e['sender'], '|', e['content']['body'])"
```

## 9. mark_read and heartbeat

Send a read receipt in the portal room; the bridge queues a `mark_read` command:

```sh
curl -sS -XPOST "http://localhost:8008/_matrix/client/v3/rooms/$PORTAL/read_markers" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"m.read":"$event_id"}'
```

`pairsim` logs `command seq=2 type=mark_read {...}` and posts `thread.updated` + `ack`
(`accepted=2`). `pairsim` also posts a heartbeat every 30 seconds; a manual probe:

```sh
curl -sS -XPOST http://127.0.0.1:29322/_smsmms/v1/heartbeat \
  -H 'Authorization: Bearer <device token>' -H 'Content-Type: application/json' \
  -d '{"device_id":"android-sim-01","battery_pct":88,"default_sms_app":true,
       "permissions":{"receive_sms":true,"send_mms":true,"mark_read":true},"app_version":"pairsim/0.1.0"}'
# {"ok":true,"server_time_ms":1792759965765}
```

## 10. What to verify (checklist)

| Check | Evidence |
| --- | --- |
| Pairing API up, pairing enabled | `GET /_smsmms/v1/info` returns protocol 1 and `pairing_enabled: true` |
| Session created by `!sms login app-pair` | bridge log `Pairing session started by app`, QR payload in `Created login process` |
| Signature accepted, token issued | `pairsim` prints `paired:` + `device token:`; bridge log `App completed pairing` |
| Device persisted | `user_login.metadata` has `transport: "app"`, `device_id`, `pubkey_spki`, `token_sha256` |
| Queue/events/heartbeat authenticated | bad token → HTTP 401; heartbeat → `{"ok":true}`; events → `accepted` count |
| Phone event creates portal | `Matrix room created ... portal_id=thread-<scope>-<thread>` |
| Matrix → phone send delivered | `pairsim` prints the `send_sms` command with the Matrix event ID as `send_id` |
| `send.result` accepted, no duplicate | bridge log `Successfully sent message to phone ... pending=false`; no second Matrix event with the same body |
| `mark_read` delivered | `pairsim` prints `command ... type=mark_read`; posts `thread.updated` + `ack` |
| Room sane | `m.room.name` matches the participant set; members are the user, the bot and the phone-number ghost(s) |

## Known local quirks and observations

- **Management-room encryption gate**: the test must grant the bot power level 100 and add
  `m.room.encryption: 0` to the room's power levels (step 6); without it the gate cannot enable
  encryption and commands only produce "room encryption could not be verified" notices.
- **Acked non-send commands stay queued**: `ack` events are accepted but only `send.result`
  removes a command (`removeCommand` is keyed by `send_id`). After the read receipt above, the
  in-memory queue still lists the `mark_read` command. This is harmless while the app's command
  cursor keeps advancing, but diverges from `docs/PAIRING_PROTOCOL.md` ("commands are removed
  once acknowledged by an `events` batch that includes their `send.result`/`ack`").
- **Simulated phone's own number**: app logins do not know the phone's SIM numbers, so the
  fake local number becomes a ghost member and the portal is named like a group
  (`+15550001111, +15551234567`). With the real app, own numbers would be filtered.
- The pairing code is visible in the bridge debug log (`logging.min_level: debug` is the
  generated default). With the log level raised, read it from a Matrix client instead.

## Cleanup

```sh
kill %1 %2 2>/dev/null            # bridge and pairsim if started as background jobs
docker rm -f e2e-synapse
rm -rf "$WORK"
```
