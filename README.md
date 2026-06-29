# MD7 Bridge — Hardware Implementation

**Protocol translation bridge between Meshtastic LoRa mesh networking and DTN7 Bundle Protocol v7**

Deployed on two Raspberry Pi 5 units, each connected to a Heltec WiFi LoRa 32 V3 device.
A DTN bundle injected at one node travels over real 868 MHz LoRa radio and arrives at the other node intact.

**Status: Goal 1 hardware COMPLETE** — end-to-end bundle delivery proven on 2026-06-27.

---

## Why this repo exists alongside a simulation repo

The simulation repo proves the protocol translation and fragmentation logic in pure software on a single machine, using fake TCP-connected Meshtastic nodes. This repo proves the same thing on real hardware — actual LoRa radio transmission between physical devices.

The key differences from simulation:

| | Simulation | Hardware (this repo) |

| Meshtastic transport | TCP connection (fake nodes) | Real 868 MHz LoRa radio |
| Fragment encoding | JSON (~230 bytes per packet) | Binary (50 bytes per packet) |
| Meshtastic interface | Not needed | Python sidecar for PKC handling |
| Duty cycle constraint | Not applicable | EU868 1% limits burst rate |
| Proven payload | 600 bytes, 8 fragments | 5 bytes end-to-end proven |

The simulation proves the logic at scale. This hardware deployment proves the integration on real devices in real radio conditions.

---

## Architecture

dtnclient -> dtnd1 -> Bridge1 -> [Python sidecar] -> Heltec fa4371a4 -> 868 MHz LoRa -> Heltec fa6c00d0 -> [Python sidecar] -> Bridge2 -> dtnd2 -> dtnclient


### Why a Python sidecar?

In an ideal world the Go bridge would connect directly to the Heltec device over USB serial.
The project does include a Go Meshtastic serial library (meshtastic-go). However the specific
hardware in this deployment has a problem: both Heltec devices have "hasPKC=true" with cached
public keys. Meshtastic firmware 2.7.15 automatically applies PKC (end-to-end encryption) to
direct messages between nodes that know each other's public keys.

The Go Meshtastic library cannot perform the PKC session handshake. Packets sent from Go
arrive at the receiving device encrypted in a way Go cannot decrypt — they arrive with empty
decoded fields and are silently discarded.

The Python "meshtastic" library (version 2.7.9) handles PKC transparently. The sidecar
("meshtastic_bridge.py") runs alongside the Go bridge on each RPi, holds the Heltec serial
connection, and exposes a simple Unix socket for the Go bridge to read and write raw bytes.

In deployments where PKC is not an issue (devices without cached public keys, or firmware
that allows PKC to be disabled), the sidecar can be replaced by a direct Go serial connection
with no changes to the bridge logic.

---

## Hardware

| Device | Role | IP | Node ID |

| Raspberry Pi 5 (rpi5) | Bridge 1, dtnd1, Heltec sender | 10.3.3.187 | — |
| Raspberry Pi 5 (rpi4) | Bridge 2, dtnd2, Heltec receiver | 10.3.9.93 | — |
| Heltec WiFi LoRa 32 V3 | LoRa radio at rpi5 | /dev/ttyUSB0 | fa4371a4 |
| Heltec WiFi LoRa 32 V3 | LoRa radio at rpi4 | /dev/ttyUSB0 | fa6c00d0 |

**Heltec firmware:** Meshtastic 2.7.15.567b8ea | **Region:** EU868 | **Modem:** LONG_FAST

---

## File structure

"""
md7-bridge-hardware/
├── cmd/gateway/main.go                  Main bridge — runs on both RPis
├── internal/
│   ├── meshtastic/
│   │   ├── packet.go                    Address translation: NodeIDToEID / EIDToNodeID (Implemented but not used)
│   │   └── serial_client.go             Go socket client for the Python sidecar
│   └── fragment/
│       ├── fragmenter.go                Binary fragment encoding (50 bytes max)
│       └── reassembler.go               Out-of-order fragment reassembly
├── config/
│   ├── dtnd1.toml                       DTN7 config for rpi5 (dtn://test/, port 8080)
│   └── dtnd2.toml                       DTN7 config for rpi4 (dtn://node2/, port 8081)
├── meshtastic_bridge.py                 Python sidecar — Heltec serial + PKC
├── go.mod
└── README.md
"""

### What each file does

**"cmd/gateway/main.go"** is the core bridge. In hardware mode (when a socket path is passed as the 4th argument) it uses "serial_client.go" to talk to the sidecar. Bridge 1 polls dtnd1 every 5 seconds, fragments any bundles it finds using binary encoding, and sends them to the sidecar. Bridge 2 listens for incoming fragments from the sidecar, reassembles them, and injects the complete payload into dtnd2.

**"internal/fragment/fragmenter.go"** splits a payload into binary-encoded fragments. Each fragment is a 12-byte header plus up to 38 bytes of data = 50 bytes total. This fits within the reliable LoRa transmission limit observed on these devices with PKC overhead.

**"internal/fragment/reassembler.go"** collects incoming fragments and returns the complete payload when all fragments for a bundle have arrived. Handles out-of-order delivery. Discards incomplete bundles after 10 minutes.

**"internal/meshtastic/serial_client.go"** is the Go side of the sidecar connection. It connects to the Unix socket, writes outbound fragment bytes with a 2-byte length prefix, and reads incoming bytes forwarded by the sidecar from the LoRa mesh.

**"internal/meshtastic/packet.go"** contains the address translation functions "NodeIDToEID" and "EIDToNodeID" that map between Meshtastic's uint32 node IDs and DTN7's URI-style Endpoint IDs. Implemented but not yet used in the active routing path. Current hardware uses fixed bridge EIDs instead.

**"meshtastic_bridge.py"** is the Python sidecar. It holds the connection to the Heltec device over "/dev/ttyUSB0", subscribes to incoming packets via pubsub, and exposes a Unix socket for the Go bridge.


## Protocol translation

Three types of translation happen as a bundle moves through the system:

**1. Address translation** — Meshtastic node IDs are uint32 integers. DTN7 uses URI-style EIDs. The bridge maps between them: "0xFA4371A4 -> dtn://meshtastic/fa4371a4". In the current implementation the bridge uses its own fixed EIDs ("dtn://test/bridge1", "dtn://node2/bridge") for routing; Meshtastic node IDs are embedded in the payload metadata. Full dynamic address routing is the next step.

**2. Data framing** — A BPv7 bundle (CBOR-encoded primary block + payload block managed by dtnd) has its payload extracted by the bridge and re-injected into a new bundle at the destination. The bridge sits at the boundary between the two protocol worlds.

**3. Fragmentation** — DTN bundles can be large. LoRa packets are small. The bridge fragments the payload into 38-byte chunks and reassembles them at the destination, handling the out-of-order delivery that LoRa networks commonly produce.


## Fragment encoding — why binary not JSON

The simulation uses JSON-encoded fragments for readability. JSON cannot be used on hardware because the encoded packet size is too large.

A JSON fragment with 80 bytes of data looks like:
  json
{"BundleID":"aabbccdd11223344","Index":0,"Total":6,"PayloadSize":200,"Data":"QUFB..."}

This is 114–230 bytes per packet. Testing showed that on EU868 with PKC active, only packets under approximately 50 bytes are reliably received.

The binary format encodes the same information in exactly 12 bytes of header:

[8 bytes BundleID][1 byte Index][1 byte Total][2 bytes PayloadSize][N bytes Data]

With 38 bytes of data: 12 + 38 = 50 bytes total. This fits reliably.

---

## Setup and dependencies

### Go dependencies

"""bash
# Clone the Meshtastic Go library alongside this repo
git clone https://github.com/meshnet-gophers/meshtastic-go ../meshtastic-go

# Build the bridge
cd md7-bridge-hardware
go build ./...
"""

### Python dependencies

"""bash
# Create a virtual environment
python3 -m venv ~/meshtastic-env
source ~/meshtastic-env/bin/activate
pip install meshtastic==2.7.9
"""

### DTN7

"""bash
# Clone and build dtn7-go
git clone https://github.com/dtn7/dtn7-go
cd dtn7-go && go install ./...
# dtnd and dtnclient will be in $GOPATH/bin
"""

---

## Running the hardware test

**Check EU868 duty cycle before starting — must be 0.0:**
"""bash
meshtastic --port /dev/ttyUSB0 --info 2>/dev/null | grep channelUtilization
"""

**On rpi4 — start sidecar first:**
"""bash
source ~/meshtastic-env/bin/activate
python3 ~/md7-bridge-hardware/meshtastic_bridge.py /dev/ttyUSB0 /tmp/mesh_node2.sock
"""

**On rpi5 — start sidecar first:**
"""bash
source ~/meshtastic-env/bin/activate
python3 ~/md7-bridge-hardware/meshtastic_bridge.py /dev/ttyUSB0 /tmp/mesh_node1.sock
"""

**On rpi5 — start dtnd1:**
"""bash
pkill -f dtnd; rm -f /tmp/dtnd.socket; rm -rf /tmp/dtn_store
cd ~/md7-bridge-hardware && dtnd config/dtnd1.toml
"""

**On rpi4 — start dtnd2:**
"""bash
pkill -f dtnd; rm -f /tmp/dtnd2.socket; rm -rf /tmp/dtn_store2
cd ~/md7-bridge-hardware && dtnd config/dtnd2.toml
"""

**On rpi5 — start Bridge 1:**
"""bash
cd ~/md7-bridge-hardware && go run cmd/gateway/main.go :9000 dtn://test/bridge1 http://localhost:8080/rest /tmp/mesh_node1.sock
"""

**On rpi4 — start Bridge 2:**
"""bash
cd ~/md7-bridge-hardware && go run cmd/gateway/main.go :9001 dtn://node2/bridge http://localhost:8081/rest /tmp/mesh_node2.sock
"""

**On rpi4 — register app endpoint:**
"""bash
UUID2=$(curl -s http://localhost:8081/rest/register -X POST \
  -H "Content-Type: application/json" \
  -d '{"endpoint_id":"dtn://node2/app"}' | python3 -c "import sys,json; print(json.load(sys.stdin)['uuid'])")
echo "UUID: $UUID2"
"""

**On rpi5 — inject a bundle:**
"""bash
echo -n "Hello from LoRa" > /tmp/test.txt
dtnclient create -a /tmp/dtnd.socket -s "dtn://test/" -d "dtn://test/bridge1" -p /tmp/test.txt
"""

**On rpi4 — retrieve the delivered bundle:**
"""bash
curl -s http://localhost:8081/rest/fetch -X POST \
  -H "Content-Type: application/json" \
  -d "{\"uuid\":\"$UUID2\"}" | python3 -c "
import sys,json,base64
data=json.load(sys.stdin)
bundles=data.get('bundles') or []
print('Bundles received:', len(bundles))
if bundles:
    print('Content:', base64.b64decode(bundles[0]['payloadBlock']['data']))
"
"""

**Expected output:**
"""
Bundles received: 1
Content: b'Hello from LoRa'
"""

---

## Known constraints

**EU868 duty cycle (1%)** — EU radio law limits transmission to 1% of time per channel, measured over a 15-minute sliding window. The Heltec firmware enforces this strictly. For multi-fragment bundles (140+ bytes = 4+ fragments), some fragments may not be transmitted in a single burst. The DTN store-carry-forward mechanism handles retransmission automatically over subsequent fetch cycles.

Check before transmitting:
"""bash
meshtastic --port /dev/ttyUSB0 --info 2>/dev/null | grep channelUtilization
"""
Wait until "channelUtilization: 0.0".

**PKC with hasPKC=true** — Direct messages between nodes that have cached each other's public keys are PKC-encrypted by the firmware. The Python sidecar handles this but it means the Go library cannot be used directly for serial communication on these specific devices.

**Heltec LED as diagnostic** — The white LED blinks when the radio is active. If it stops blinking, the device may have entered duty cycle lockout or lost its configuration. Unplug and replug the USB to reboot the device.