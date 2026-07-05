PYTHON    ?= $(HOME)/meshtastic-env/bin/python3
BRIDGE    ?= ./meshtastic_bridge.py
DTND      ?= dtnd
DTNCLIENT ?= dtnclient
UUID_FILE ?= /tmp/uuid2.txt

.PHONY: A1 A2 A3 A4 A5 B1 B2 B3 B4 clean

# starting order: B1 → A1 → B2 → A2 → B3 → A3 → A4 → B4 → A5.

# === Node A (node2 / dtnd2 / receiver) ===================================

A1:
	$(PYTHON) $(BRIDGE) /dev/ttyUSB1 /tmp/mesh_node2.sock

A2:
	$(DTND) config/dtnd2.toml

A3:
	go run cmd/gateway/main.go dtn://node2/bridge http://localhost:8081/rest /tmp/mesh_node2.sock

A4:
	@UUID2=$$(curl -s http://localhost:8081/rest/register -X POST \
		-H "Content-Type: application/json" \
		-d '{"endpoint_id":"dtn://node2/app"}' \
		| $(PYTHON) -c "import sys,json; print(json.load(sys.stdin)['uuid'])"); \
	echo "$$UUID2" > $(UUID_FILE); \
	echo "UUID: $$UUID2 (saved to $(UUID_FILE))"

A5:
	@UUID2=$$(cat $(UUID_FILE)); \
	curl -s http://localhost:8081/rest/fetch -X POST \
		-H "Content-Type: application/json" \
		-d "{\"uuid\":\"$$UUID2\"}" | $(PYTHON) -c "\
import sys,json,base64;\
data=json.load(sys.stdin);\
bundles=data.get('bundles') or [];\
print('Bundles received:', len(bundles));\
print('Content:', base64.b64decode(bundles[0]['payloadBlock']['data'])) if bundles else None"

# === Node B (node1 / dtnd1 / sender) ======================================

B1:
	$(PYTHON) $(BRIDGE) /dev/ttyUSB0 /tmp/mesh_node1.sock

B2:
	$(DTND) config/dtnd1.toml

B3:
	go run cmd/gateway/main.go dtn://node1/bridge1 http://localhost:8080/rest /tmp/mesh_node1.sock

B4:
	echo -n "Hello from LoRa" > /tmp/test.txt
	$(DTNCLIENT) create -a /tmp/dtnd.socket -s "dtn://node1/" -d "dtn://node1/bridge1" -p /tmp/test.txt

clean:
	-pkill -f dtnd
	-rm -f /tmp/dtnd.socket /tmp/dtnd2.socket
	-rm -rf /tmp/dtn_store /tmp/dtn_store2
	-rm -f $(UUID_FILE)
