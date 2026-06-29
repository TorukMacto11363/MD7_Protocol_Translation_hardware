#!/usr/bin/env python3
"""
meshtastic_bridge.py — Python sidecar for the MD7 Bridge hardware deployment.

Why Python instead of Go?
The Heltec WiFi LoRa 32 V3 devices in this project have hasPKC=true. Both devices
have each other's public keys cached, so the Meshtastic firmware automatically
encrypts all direct messages using PKC (end-to-end encryption). The Go Meshtastic
library cannot perform the PKC handshake. The Python meshtastic library (v2.7.9)
handles PKC transparently, so it is used as the hardware interface layer.

This script bridges the Go bridge process and the Heltec LoRa device:
  Go bridge  <-->  Unix socket  <-->  this script  <-->  /dev/ttyUSB0  <-->  Heltec

Socket protocol: every message is prefixed with a 2-byte big-endian uint16
indicating the length of the payload bytes that follow.

Usage:
    python3 meshtastic_bridge.py /dev/ttyUSB0 /tmp/mesh_node1.sock
"""

import sys
import socket
import os
import threading
import time
import struct
import logging

import meshtastic.serial_interface
from pubsub import pub

# Suppress meshtastic library info/warning logs — they are noisy during normal operation
logging.getLogger('meshtastic').setLevel(logging.ERROR)

SERIAL_PORT = sys.argv[1]   # e.g. /dev/ttyUSB0
SOCK_PATH   = sys.argv[2]   # e.g. /tmp/mesh_node1.sock

# receive_conn is set after the Go bridge connects.
# Incoming LoRa packets are forwarded to this connection.
receive_conn = None
receive_lock = threading.Lock()


def on_receive(packet, interface=None, **kwargs):
    """Called by the Meshtastic library when a packet arrives from the LoRa mesh.

    We filter for DTN bridge packets (PRIVATE_APP or TEXT_MESSAGE_APP) and
    forward the raw payload bytes to the Go bridge via the Unix socket.

    TEXT_MESSAGE_APP (portNum=1) is used instead of PRIVATE_APP (portNum=256)
    because binary payloads in PRIVATE_APP trigger a UTF-8 decode error in the
    Python library that silently drops the packet before this callback fires.
    """
    decoded = packet.get("decoded", {})
    portnum = decoded.get("portnum", "")
    data    = decoded.get("payload", b"")
    from_id = packet.get("fromId", "?")

    print(f"[SIDECAR] Received port={portnum} from={from_id} size={len(data)}", flush=True)

    # Only forward DTN fragment packets — ignore telemetry, position, etc.
    if portnum not in ("PRIVATE_APP", 256, "256", "TEXT_MESSAGE_APP", 1, "1"):
        return

    print(f"[SIDECAR] Forwarding {len(data)} bytes to Go bridge", flush=True)

    if data and receive_conn:
        with receive_lock:
            # 2-byte big-endian length prefix so Go knows how many bytes to read
            receive_conn.sendall(struct.pack(">H", len(data)) + data)


# Subscribe before creating the Meshtastic interface so no packets are missed
pub.subscribe(on_receive, "meshtastic.receive")

# Connect to the Heltec LoRa device over USB serial
iface = meshtastic.serial_interface.SerialInterface(SERIAL_PORT)
time.sleep(2)   # allow the interface to initialise before accepting connections

# Create the Unix socket that the Go bridge will connect to
if os.path.exists(SOCK_PATH):
    os.remove(SOCK_PATH)

srv = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
srv.bind(SOCK_PATH)
srv.listen(1)
print(f"Bridge ready on {SOCK_PATH}", flush=True)

# Block until the Go bridge connects
conn, _ = srv.accept()
receive_conn = conn
print("Go bridge connected", flush=True)


def send_loop():
    """Reads outbound fragment bytes from the Go bridge and sends them over LoRa.

    Runs in a background daemon thread so the main thread stays free for
    pubsub callbacks.

    portNum=1 (TEXT_MESSAGE_APP) is used here for the same reason described in
    on_receive — binary payloads in PRIVATE_APP are sometimes dropped by the
    receiving device's firmware when PKC public keys are cached.
    """
    while True:
        # Read the 2-byte length prefix
        hdr = conn.recv(2)
        if not hdr:
            break

        length = struct.unpack(">H", hdr)[0]
        data = conn.recv(length)

        if data:
            print(f"[SIDECAR] Sending {len(data)} bytes over LoRa", flush=True)
            iface.sendData(data, portNum=1, wantAck=False)
            # Small delay between packets to stay within EU868 1% duty cycle
            time.sleep(0.5)


threading.Thread(target=send_loop, daemon=True).start()

print("[SIDECAR] Running — waiting for packets", flush=True)

# Keep the main thread alive without spinning so pubsub callbacks can fire
threading.Event().wait()