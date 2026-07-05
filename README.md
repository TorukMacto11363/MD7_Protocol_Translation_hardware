# MD7 Bridge — Hardware Implementation

**Protocol translation bridge between Meshtastic LoRa mesh networking and DTN7 Bundle Protocol v7**

Deployed on two Raspberry Pi 5 units, each connected to a Heltec WiFi LoRa 32 V3 device.
A DTN bundle injected at one node travels over real 868 MHz LoRa radio and arrives at the other node intact.

**Status:** end-to-end bundle delivery works on real hardware, including automatic fragment retry
for dropped packets (see "Fragment retry / NACK" below). Multi-fragment payloads up to 600 bytes /
17 fragments deliver reliably and byte-for-byte over real 868 MHz LoRa radio between the two RPis.

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
|---|---|---|---|
| Raspberry Pi 5 (rpi5) | Bridge 1, dtnd1, Heltec sender | 10.3.3.187 | — |
| Raspberry Pi 5 (rpi4) | Bridge 2, dtnd2, Heltec receiver | 10.3.9.93 | — |
| Heltec WiFi LoRa 32 V3 | LoRa radio at rpi5 | /dev/ttyUSB0 | fa4371a4 |
| Heltec WiFi LoRa 32 V3 | LoRa radio at rpi4 | /dev/ttyUSB0 | fa6c00d0 |

**Heltec firmware:** Meshtastic 2.7.15.567b8ea | **Region:** EU868 | **Modem:** LONG_FAST

Each RPi runs its own dtnd + bridge + sidecar, talking to its own Heltec device over USB serial.

---

## File structure

```
md7-bridge-hardware/
├── cmd/gateway/main.go                  Main bridge — runs on both RPis
├── internal/
│   ├── meshtastic/
│   │   └── serial_client.go             Go socket client for the Python sidecar
│   └── fragment/
│       ├── fragmenter.go                Binary fragment encoding (50 bytes max)
│       ├── reassembler.go               Out-of-order fragment reassembly + stall detection
│       └── nack.go                      Retry request message — asks the sender for missing fragments
├── config/
│   ├── dtnd1.toml                       DTN7 config for rpi5 (dtn://node1/, port 8080)
│   └── dtnd2.toml                       DTN7 config for rpi4 (dtn://node2/, port 8081)
├── meshtastic_bridge.py                 Python sidecar — Heltec serial + PKC
├── go.mod
└── README.md
```

### What each file does

**"cmd/gateway/main.go"** is the core bridge. It takes three arguments — bridge EID, dtnd REST URL, serial socket path — and uses "serial_client.go" to talk to the sidecar over that socket. Bridge 1 polls dtnd1 every 5 seconds, fragments any bundles it finds using binary encoding, and sends them to the sidecar. Bridge 2 listens for incoming fragments from the sidecar, reassembles them, and injects the complete payload into dtnd2. It also caches whatever it last sent (`sentCache`) so it can respond to retry requests without re-fragmenting, and runs a background check every 5s to nudge its own reassembler into asking for missing fragments if a bundle's gone quiet.

**"internal/fragment/fragmenter.go"** splits a payload into binary-encoded fragments. Each fragment is a 13-byte header (message type byte + bundle ID + index + total + payload size) plus up to 37 bytes of data — 50 bytes total, which is what's held up reliably on this hardware with PKC in the mix.

**"internal/fragment/reassembler.go"** collects incoming fragments and returns the complete payload once everything for a bundle has arrived. Handles out-of-order delivery and drops exact duplicates. Also tracks how long it's been since a bundle last got a new fragment — if that goes quiet for 20s while the bundle's still incomplete, it hands back a NACK request for whatever's missing (see below). Fully incomplete bundles get discarded after 10 minutes either way.

**"internal/fragment/nack.go"** is the retry request format — small message (bundle ID + list of missing fragment indices) that a receiver sends back over the mesh when it's given up waiting.

**"internal/meshtastic/serial_client.go"** is the Go side of the sidecar connection. It connects to the Unix socket, writes outbound fragment bytes with a 2-byte length prefix, and reads incoming bytes forwarded by the sidecar from the LoRa mesh.

**"meshtastic_bridge.py"** is the Python sidecar. It holds the connection to the Heltec device over "/dev/ttyUSB0", subscribes to incoming packets via pubsub, and exposes a Unix socket for the Go bridge. Paces outgoing sends at a fixed 3 seconds apart to respect the EU868 duty cycle — see "Fragment retry / NACK" for why it's a fixed gap and not an ACK-based wait.


## Protocol translation

Three types of translation happen as a bundle moves through the system:

**1. Address handling** — the bridge doesn't translate individual Meshtastic node IDs into DTN EIDs. Every bundle is routed using one of two fixed EIDs, `dtn://node1/bridge1` or `dtn://node2/bridge`, depending on which side sent it — not by which physical node the packet actually came from. That's fine for now since this deployment only has two nodes, a fixed sender and a fixed receiver, so there's nothing to dynamically route between yet.

**2. Data framing** — A BPv7 bundle (CBOR-encoded primary block + payload block managed by dtnd) has its payload extracted by the bridge and re-injected into a new bundle at the destination. The bridge sits at the boundary between the two protocol worlds.

**3. Fragmentation** — DTN bundles can be large. LoRa packets are small. The bridge fragments the payload into 37-byte chunks and reassembles them at the destination, handling the out-of-order delivery (and the occasional lost fragment) that LoRa networks commonly produce.


## Fragment encoding — why binary not JSON

JSON was the obvious first choice for encoding a fragment — easy to read, easy to debug. It doesn't
work here because the encoded packet size is too large for what this hardware can reliably send.

A JSON fragment with 80 bytes of data looks like:
```json
{"BundleID":"aabbccdd11223344","Index":0,"Total":6,"PayloadSize":200,"Data":"QUFB..."}
```

This is 114–230 bytes per packet — way too big for what this hardware can reliably send.

The binary format packs the same info into a 13-byte header:

```
[1 byte MsgType][8 bytes BundleID][1 byte Index][1 byte Total][2 bytes PayloadSize][N bytes Data]
```

With 37 bytes of data: 13 + 37 = 50 bytes total. The message type byte is what lets a fragment and
a retry request travel over the same channel without getting confused for one another — `0x01` for
fragment data, `0x02` for a NACK request.

### How reliable is "50 bytes" really?

Measured directly rather than assumed: a standalone script sent raw payloads of increasing size
straight over the radio (bypassing the bridge/fragmentation stack entirely, same `sendData()` call
our sidecar uses) between the two Heltec units, 5 tries per size, and counted how many arrived.

| Payload size | Arrived | Rate |
|---|---|---|
| 20 bytes | 5/5 | 100% |
| 37 bytes (our fragment data size) | 4/5 | 80% |
| 50 bytes (our full fragment size) | 3/5 | 60% |
| 60 bytes | 2/5 | 40% |
| 80 bytes | 2/5 | 40% |
| 100 bytes | 0/5 | 0% |
| 150 bytes | 0/5 | 0% |

So "only packets under ~50 bytes are reliably received" is optimistic — reliability is already
sliding well before 50 bytes, and even our own fragment size only landed 4/5 in this run. It's a
gradual decline, not a clean cutoff, and it's fully dead by ~100 bytes. Caveat: these 7 sizes were
run in increasing order back-to-back in one sitting, so larger sizes also carry more accumulated
EU868 duty-cycle usage from the smaller sizes tested just before them — some of the drop-off at the
high end is probably duty cycle exhaustion compounding with packet size, not packet size alone. A
tighter measurement would randomize the order and space runs further apart.

**If individual packets aren't that reliable even at 50 bytes, how does a full 600-byte / 17-fragment
bundle still arrive intact?** Keeping fragments small only gets a *better* base chance per packet, not
a good one on its own — the retry mechanism described in "Fragment retry / NACK" below is what
actually closes the gap to reliable end-to-end delivery, and it's worth reading the "probabilistic
recovery, not guaranteed delivery" part of that section for how much retrying it actually takes.

---

## Fragment retry / NACK

First pass at this hardware deployment didn't have any recovery if a fragment got dropped —
which happens more than you'd like on LoRa, especially once EU868's 1% duty cycle kicks in on a
burst of a dozen-plus packets. A partially-reassembled bundle would just sit in memory for 10
minutes and then get thrown away, silently. Fine for a single 5-byte fragment, not fine for
anything bigger.

How it works now:

1. Bridge 1 fragments a bundle and sends it, same as before, but also keeps a copy of the
   fragments around in memory (`sentCache` in `main.go`) in case they're needed again.
2. Bridge 2's reassembler tracks the last time each in-progress bundle received a new fragment.
   If a bundle goes 20 seconds without one and it's still incomplete, the reassembler hands
   back a `NackRequest` listing exactly which indices are missing.
3. Bridge 2 sends that request back over the mesh (same channel, same radio).
4. Bridge 1 sees it come in through the normal receive path, checks its cache, and resends
   just the missing fragments — not the whole bundle.
5. This repeats up to 6 times per bundle before giving up and discarding it.

### This is probabilistic recovery, not guaranteed delivery

Worth being precise about what "reliable" means here, since it's easy to read "delivers byte-for-byte"
as "every fragment arrives." It doesn't — most of them need a second (or third) try. From an actual
test campaign, tracking exactly how many transmissions it took to get every fragment through:

| Payload | Fragments that needed a retry | Total transmissions sent |
|---|---|---|
| 16B / 1 chunk | 0/1 | 1 |
| 100B / 3 chunks | 1/3 | 4 |
| 200B / 6 chunks | 0/6 | 6 |
| 350B / 10 chunks | **7/10** | 19 |
| 600B / 17 chunks | 5/17 | 24 |

350 bytes needed 19 transmissions to land 10 fragments' worth of data — that's not a rare edge case,
that's most of the bundle failing its first attempt and getting recovered.

**This can still fail outright, and did.** The very first attempt at a 74-byte / 2-fragment payload
lost *both* fragments completely — 0 out of 2 arrived. Since no fragment ever reached the receiver,
the reassembler never had anything to open a buffer for, so there was nothing to hang a NACK request
on either — the transfer just died silently with no retry ever attempted. It took a fresh, manually
re-injected attempt (a new bundle, not a retry of the failed one) to get that payload through. The
NACK mechanism only helps once it has a foothold — at least one fragment has to arrive before there's
anything to recover. Total loss of a short bundle before that happens is a real gap this design
doesn't cover.

Turning `wantAck` on made every fragment take about 23-24 extra seconds waiting for an
acknowledgement that rarely arrived in a useful way. This threw off the retry mechanism's
timing, since it assumes fragments arrive roughly every 3 seconds — with `wantAck` slowing
things down, it kept mistakenly asking for fragments that were still on their way, not lost. Each
mistaken request added more retransmissions on top of the ones already in progress, and the
message failed to complete before running out of retry attempts. Turning `wantAck` off removed
this problem entirely: fragments went out at a steady, predictable pace, and the same message
completed successfully, three times faster. This is why the final design does not use `wantAck` at
all.

This was confirmed directly with a controlled side-by-side test — same 300-byte payload, same
hardware, same NACK mechanism active, only `wantAck` changed:

| | `wantAck=True` + NACK | `wantAck=False` + NACK |
|---|---|---|
| Outcome | **Failed** — gave up after 6 retry rounds | **Succeeded**, verified byte-for-byte |
| Radio transmissions used | 45 | 13 |
| Fragments confirmed received | 6 of 9 | 9 of 9 |
| Time elapsed | 240.9s, then abandoned | 81.0s, completed |

With `wantAck=True`, every ACK wait took ~23-24 seconds before timing out, stretching each fragment's
send time from 3 seconds to roughly 26-27 seconds. The NACK mechanism's 20-second "assume it's missing"
threshold was tuned around the normal 3-second pace, so it kept firing while the sender was still
partway through a slow ACK wait for a *different* fragment — not because anything was actually lost.
Each false alarm triggered an extra resend on top of the ones already queued, and the two overlapping
streams of transmissions ate through all 6 retry attempts before the transfer could complete. Turning
`wantAck` on didn't just fail to help — it actively broke the retry mechanism it was combined with.

Also worth knowing: killing the bridge/sidecar processes does **not** flush whatever the Heltec
firmware still has queued internally. If you're restarting after something went sideways mid-burst,
a stale message can still show up minutes later from a "fresh" restart. A quick unplug/replug of
the USB devices clears it — the "Heltec LED as diagnostic" note below covers the same fix for
duty cycle lockouts.

---

## Setup and dependencies

### Go dependencies

```bash
# Clone the Meshtastic Go library alongside this repo
git clone https://github.com/meshnet-gophers/meshtastic-go ../meshtastic-go

# Build the bridge
cd md7-bridge-hardware
go build ./...
```

### Python dependencies

```bash
# Create a virtual environment
python3 -m venv ~/meshtastic-env
source ~/meshtastic-env/bin/activate
pip install meshtastic==2.7.9
```

### DTN7

```bash
# Clone and build dtn7-go
git clone https://github.com/dtn7/dtn7-go
cd dtn7-go && go install ./...
# dtnd and dtnclient will be in $GOPATH/bin
```

---

## Running the hardware test

There's a `Makefile` that wraps the commands below into short targets, one per terminal — `B1`
through `B4` for the sender side (sidecar, dtnd1, bridge1, inject), `A1` through `A5` for the
receiver side (sidecar, dtnd2, bridge2, register, fetch), run in the order noted at the top of the
Makefile, plus `make clean` to tear it all down. The manual steps are still here below since
they're clearer about what's actually happening and useful if you need to run things on two
separate RPis over ssh.

**Check EU868 duty cycle before starting — must be 0.0:**
```bash
meshtastic --port /dev/ttyUSB0 --info 2>/dev/null | grep channelUtilization
```

**On rpi4 — start sidecar first:**
```bash
source ~/meshtastic-env/bin/activate
python3 ~/md7-bridge-hardware/meshtastic_bridge.py /dev/ttyUSB0 /tmp/mesh_node2.sock
```

**On rpi5 — start sidecar first:**
```bash
source ~/meshtastic-env/bin/activate
python3 ~/md7-bridge-hardware/meshtastic_bridge.py /dev/ttyUSB0 /tmp/mesh_node1.sock
```

**On rpi5 — start dtnd1:**
```bash
pkill -f dtnd; rm -f /tmp/dtnd.socket; rm -rf /tmp/dtn_store
cd ~/md7-bridge-hardware && dtnd config/dtnd1.toml
```

**On rpi4 — start dtnd2:**
```bash
pkill -f dtnd; rm -f /tmp/dtnd2.socket; rm -rf /tmp/dtn_store2
cd ~/md7-bridge-hardware && dtnd config/dtnd2.toml
```

**On rpi5 — start Bridge 1:**
```bash
cd ~/md7-bridge-hardware && go run cmd/gateway/main.go dtn://node1/bridge1 http://localhost:8080/rest /tmp/mesh_node1.sock
```

**On rpi4 — start Bridge 2:**
```bash
cd ~/md7-bridge-hardware && go run cmd/gateway/main.go dtn://node2/bridge http://localhost:8081/rest /tmp/mesh_node2.sock
```

**On rpi4 — register app endpoint:**
```bash
UUID2=$(curl -s http://localhost:8081/rest/register -X POST \
  -H "Content-Type: application/json" \
  -d '{"endpoint_id":"dtn://node2/app"}' | python3 -c "import sys,json; print(json.load(sys.stdin)['uuid'])")
echo "UUID: $UUID2"
```

**On rpi5 — inject a bundle:**
```bash
echo -n "Hello from LoRa" > /tmp/test.txt
dtnclient create -a /tmp/dtnd.socket -s "dtn://node1/" -d "dtn://node1/bridge1" -p /tmp/test.txt
```

**On rpi4 — retrieve the delivered bundle:**
```bash
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
```

**Expected output:**
```
Bundles received: 1
Content: b'Hello from LoRa'
```

If you're testing without two physically separate machines (no RPis handy, say), all of the above
still works the same way on a single host with two Heltec devices plugged in — just run each step
in its own terminal on the one machine instead of splitting across two. `dtnd1`/`dtnd2` bind to
different local ports and `mesh_node1.sock`/`mesh_node2.sock` are separate sockets, so there's no
conflict either way.

---

## Known constraints

**EU868 duty cycle (1%)** — EU radio law limits transmission to 1% of time per channel, measured over a 15-minute sliding window. The Heltec firmware enforces this strictly. For multi-fragment bundles (140+ bytes = 4+ fragments), it's normal for some fragments to not make it through on the first pass — the fragment retry mechanism described above handles this automatically now, asking for exactly what's missing rather than resending everything or losing the bundle outright.

Check before transmitting:
```bash
meshtastic --port /dev/ttyUSB0 --info 2>/dev/null | grep channelUtilization
```
Wait until "channelUtilization: 0.0".

**PKC with hasPKC=true** — Direct messages between nodes that have cached each other's public keys are PKC-encrypted by the firmware. The Python sidecar handles this but it means the Go library cannot be used directly for serial communication on these specific devices.
