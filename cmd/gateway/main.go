// arguments - bridge EID, dtnd REST URL, serial socket path
// uses "serial_client.go" to talk to the sidecar over the socket

package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/TorukMacto/md7-bridge/internal/fragment"
	"github.com/TorukMacto/md7-bridge/internal/meshtastic"
)

type BundlePayload struct {
	From    string `json:"from"`
	To      string `json:"to"`
	Payload string `json:"payload"`
}

var dtn7REST = "http://localhost:8080/rest"
var bridgeEID = "dtn://node1/bridge1"

// Reassembler handles incoming fragments - 10 min timeout at the receiver side
var reassembler = fragment.NewReassembler(10 * time.Minute)

// keep the fragments of whatever we last sent around for a bit, so if the other side comes back asking for a resend we don't have to re-fragment the whole bundle, just hand back the pieces it's missing.
var (
	sentCacheMu sync.Mutex
	sentCache   = make(map[[8]byte]struct {
		fragments []*fragment.Fragment
		storedAt  time.Time
	})
)

const sentCacheTTL = 5 * time.Minute // no reason to keep these around forever

func cacheSentFragments(frags []*fragment.Fragment) {
	if len(frags) == 0 {
		return
	}
	sentCacheMu.Lock()
	defer sentCacheMu.Unlock()

	now := time.Now()
	for id, entry := range sentCache {
		if now.Sub(entry.storedAt) > sentCacheTTL {
			delete(sentCache, id)
		}
	}
	sentCache[frags[0].BundleID] = struct {
		fragments []*fragment.Fragment
		storedAt  time.Time
	}{fragments: frags, storedAt: now}
}

func getCachedFragment(bundleID [8]byte, index uint8) (*fragment.Fragment, bool) {
	sentCacheMu.Lock()
	defer sentCacheMu.Unlock()

	entry, ok := sentCache[bundleID]
	if !ok {
		return nil, false
	}
	for _, f := range entry.fragments {
		if f.Index == index {
			return f, true
		}
	}
	return nil, false
}

// DTN7 REST API structs - for talking to the local dtnd over HTTP. Separate from
// the binary fragment format that actually goes out over the LoRa radio; this is
// still JSON since it never leaves the machine and size isn't a constraint here.

type RegisterRequest struct {
	EndpointId string `json:"endpoint_id"`
}

type RegisterResponse struct {
	Error string `json:"error"`
	UUID  string `json:"uuid"`
}

type BuildRequest struct {
	UUID string                 `json:"uuid"`
	Args map[string]interface{} `json:"arguments"`
}

type BuildResponse struct {
	Error string `json:"error"`
}

type FetchRequest struct {
	UUID string `json:"uuid"`
}

// DTN7 helper functions

func registerEndpoint() (string, error) {
	reqBody, _ := json.Marshal(RegisterRequest{
		EndpointId: bridgeEID,
	})

	resp, err := http.Post(
		dtn7REST+"/register",
		"application/json",
		bytes.NewReader(reqBody),
	)
	if err != nil {
		return "", fmt.Errorf("register failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var regResp RegisterResponse
	json.Unmarshal(body, &regResp)

	if regResp.Error != "" {
		return "", fmt.Errorf("register error: %s", regResp.Error)
	}

	fmt.Println("Registered with dtnd, UUID:", regResp.UUID)
	return regResp.UUID, nil
}

func sendBundle(uuid, srcEID, dstEID string, payload []byte) error {
	args := map[string]interface{}{
		"source":                 srcEID,
		"destination":            dstEID,
		"payload_block":          string(payload),
		"creation_timestamp_now": true,
		"lifetime":               "24h",
	}

	reqBody, _ := json.Marshal(BuildRequest{
		UUID: uuid,
		Args: args,
	})

	resp, err := http.Post(
		dtn7REST+"/build",
		"application/json",
		bytes.NewReader(reqBody),
	)
	if err != nil {
		return fmt.Errorf("build failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var buildResp BuildResponse
	json.Unmarshal(body, &buildResp)

	if buildResp.Error != "" {
		return fmt.Errorf("build error: %s", buildResp.Error)
	}

	fmt.Printf("  → Bundle sent: %s → %s via DTN7\n", srcEID, dstEID)
	return nil
}

func fetchBundles(uuid string) ([]BundlePayload, error) {
	reqBody, _ := json.Marshal(FetchRequest{UUID: uuid})

	resp, err := http.Post(
		dtn7REST+"/fetch",
		"application/json",
		bytes.NewReader(reqBody),
	)
	if err != nil {
		return nil, fmt.Errorf("fetch failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	var fetchResp struct {
		Error   string `json:"error"`
		Bundles []struct {
			PrimaryBlock struct {
				Source      string `json:"source"`
				Destination string `json:"destination"`
			} `json:"primaryBlock"`
			PayloadBlock struct {
				Data string `json:"data"`
			} `json:"payloadBlock"`
		} `json:"bundles"`
	}

	if err := json.Unmarshal(body, &fetchResp); err != nil {
		return nil, fmt.Errorf("parse error: %w", err)
	}

	if fetchResp.Error != "" {
		return nil, fmt.Errorf("fetch error: %s", fetchResp.Error)
	}

	var payloads []BundlePayload
	for _, bundle := range fetchResp.Bundles {
		decoded, err := base64.StdEncoding.DecodeString(bundle.PayloadBlock.Data)
		if err != nil {
			fmt.Println("  Base64 decode error:", err)
			continue
		}

		var bp BundlePayload
		if err := json.Unmarshal(decoded, &bp); err != nil {
			bp = BundlePayload{
				From:    "unknown",
				To:      "unknown",
				Payload: string(decoded),
			}
		}
		payloads = append(payloads, bp)
	}
	return payloads, nil
}

// startSerialMode runs the bridge using real Meshtastic hardware via Python sidecar, using binary fragment encoding to fit within LoRa+PKC packet limits
func startSerialMode(uuid string, portName string) {
	fmt.Printf("Starting serial mode on %s\n", portName)

	client, err := meshtastic.NewSerialClient(portName)
	if err != nil {
		fmt.Printf("ERROR connecting to Meshtastic sidecar: %v\n", err)
		return
	}
	defer client.Close()

	// When a packet arrives from the Meshtastic mesh via the Python sidecar
	client.SetReceiveHandler(func(from uint32, to uint32, payload []byte) {
		fmt.Printf("\n[MESH→REASSEMBLER] Received %d bytes from %08x\n",
			len(payload), from)

		// could be a retry request rather than an actual fragment, check that first
		if nackReq, err := fragment.DecodeNack(payload); err == nil {
			fmt.Printf("  [NACK] Peer is missing %d fragment(s) of bundle %x: %v\n",
				len(nackReq.Missing), nackReq.BundleID[:4], nackReq.Missing)
			for _, idx := range nackReq.Missing {
				f, ok := getCachedFragment(nackReq.BundleID, idx)
				if !ok {
					fmt.Printf("  [NACK-RESEND] Fragment %d for bundle %x not in cache (expired?) - cannot resend\n",
						idx, nackReq.BundleID[:4])
					continue
				}
				data := f.Encode()
				if err := client.SendPacket(0xFFFFFFFF, data); err != nil {
					// connection's probably down, no point hammering the rest of the
					// list against a dead socket - they'll just ask again later
					fmt.Printf("  [NACK-RESEND] Failed to resend fragment %d: %v - aborting this resend batch\n", idx, err)
					break
				}
				fmt.Printf("  [NACK-RESEND] Resent fragment %s\n", f.String())
				fmt.Printf("METRIC,SEND,%x,%d,%d,%d,%d\n", f.BundleID, f.Index, f.Total, len(f.Data), time.Now().UnixMilli())
				time.Sleep(3 * time.Second) // same delay given as the normal send loop
			}
			return
		}

		// use binary decode
		f, err := fragment.Decode(payload)
		if err == nil {
			fmt.Printf("  Fragment: %s\n", f.String())
			fmt.Printf("METRIC,RECV,%x,%d,%d,%d,%d\n", f.BundleID, f.Index, f.Total, len(f.Data), time.Now().UnixMilli())
			result, complete := reassembler.AddFragment(f)
			if complete {
				fmt.Printf("[REASSEMBLER→DTN7] Bundle complete! Sending to DTN7\n")
				fmt.Printf("METRIC,DONE,%x,%d,%d\n", f.BundleID, len(result), time.Now().UnixMilli())
				localAppEID := strings.Replace(bridgeEID, "bridge", "app", 1)
				err := sendBundle(uuid, bridgeEID, localAppEID, result)
				if err != nil {
					fmt.Printf("  [ERROR] DTN7 delivery failed: %v\n", err)
				} else {
					fmt.Printf("\n------------------------------------------\n")
					fmt.Printf("|  BUNDLE DELIVERED TO DTN SUCCESSFULLY    |\n")
					fmt.Printf("|  Endpoint: %-30s ║\n", localAppEID)
					fmt.Printf("|  Size: %-35d ║\n", len(result))
					fmt.Printf(" -------------------------------------------\n\n")
				}
			}
		} else {
			// Not a valid fragment - forward as raw DTN payload
			fmt.Printf("  [INFO] Non-fragment payload received - forwarding to DTN7\n")
			meshPayload, _ := json.Marshal(map[string]interface{}{
				"from":    fmt.Sprintf("%08x", from),
				"to":      fmt.Sprintf("%08x", to),
				"payload": string(payload),
			})
			var destEID string
			if bridgeEID == "dtn://node1/bridge1" {
				destEID = "dtn://node2/bridge"
			} else {
				destEID = "dtn://node1/bridge1"
			}
			sendBundle(uuid, bridgeEID, destEID, meshPayload)
		}
	})

	// Bridge 1 fetches from DTN7 and pushes to Meshtastic mesh
	if bridgeEID == "dtn://node1/bridge1" {
		go func() {
			fmt.Printf("Fetch loop started - polling DTN7 every 5 seconds\n")
			for {
				time.Sleep(5 * time.Second)
				bundles, err := fetchBundles(uuid)
				if err != nil || len(bundles) == 0 {
					continue
				}
				fmt.Printf("[DTN7→MESH] Fetched %d bundle(s) from DTN7\n", len(bundles))
				for _, bp := range bundles {
					payloadBytes := []byte(bp.Payload)
					fmt.Printf("  Payload: %q (%d bytes)\n", bp.Payload, len(payloadBytes))

					// using binary fragment encoding - each fragment is 13+37=50 bytes max
					fragments := fragment.Fragmentize(payloadBytes)
					cacheSentFragments(fragments) // keep a copy in case the peer NACKs missing fragments
					fmt.Printf("  Sending %d fragment(s) over LoRa\n", len(fragments))
					for _, f := range fragments {
						data := f.Encode() // binary encode - compact, fits in LoRa packet
						fmt.Printf("  [FRAGMENTER] %s → %d bytes binary\n", f.String(), len(data))
						err := client.SendPacket(0xFFFFFFFF, data)
						if err != nil {
							fmt.Printf("  [ERROR] LoRa send failed: %v\n", err)
						} else {
							fmt.Printf("  [MESH] Sent fragment %d/%d (%d bytes)\n",
								f.Index+1, f.Total, len(data))
							fmt.Printf("METRIC,SEND,%x,%d,%d,%d,%d\n", f.BundleID, f.Index, f.Total, len(f.Data), time.Now().UnixMilli())
						}
						time.Sleep(500 * time.Millisecond)
					}
				}
			}
		}()
	}

	// every 5s, check if any bundle has gone quiet without finishing and ask the sender for exactly what's missing, instead of just sitting there till the 10 minute reassembly timeout eventually throws it away.
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		// 20s wait period, gives plenty of room above the sender's fixed 3s/fragment pace.
		// we used to wait on the meshtastic ACK here too but that thing resolved anywhere between instant and a full 30s, so this kept firing mid-burst and asking for fragments that were already on their way.
		for range ticker.C {
			for _, nackReq := range reassembler.CheckStalled(20*time.Second, 6) {
				fmt.Printf("\n[REASSEMBLER→NACK] Requesting resend of %d missing fragment(s) for bundle %x: %v\n",
					len(nackReq.Missing), nackReq.BundleID[:4], nackReq.Missing)
				if err := client.SendPacket(0xFFFFFFFF, nackReq.Encode()); err != nil {
					fmt.Printf("  [ERROR] Failed to send NACK request: %v\n", err)
				} else {
					fmt.Printf("  [NACK] Sent over LoRa - waiting to see if bundle %x completes\n", nackReq.BundleID[:4])
				}
			}
		}
	}()

	// Start listening - blocks until sidecar disconnects
	fmt.Printf("Listening for packets from Meshtastic sidecar...\n")
	client.Start()
}

func main() {
	if len(os.Args) >= 2 {
		bridgeEID = os.Args[1]
	}
	if len(os.Args) >= 3 {
		dtn7REST = os.Args[2]
	}
	if len(os.Args) < 4 {
		fmt.Println("usage: gateway <bridge-eid> <dtn7-rest-url> <serial-port>")
		return
	}
	serialPort := os.Args[3]

	fmt.Println("=== MD7 Bridge Gateway ===")
	fmt.Println()

	fmt.Print("Waiting for dtnd... ")
	httpClient := &http.Client{Timeout: 3 * time.Second}
	for {
		dtndBase := strings.Replace(dtn7REST, "/rest", "", 1)
		resp, err := httpClient.Get(dtndBase)
		if err == nil {
			resp.Body.Close()
			break
		}
		time.Sleep(1 * time.Second)
	}
	fmt.Println("dtnd is ready!")

	uuid, err := registerEndpoint()
	if err != nil {
		fmt.Println("ERROR: Could not register with dtnd:", err)
		return
	}
	fmt.Println()

	if bridgeEID == "dtn://node1/bridge1" {
		fmt.Printf("[%s] Fetch loop polling dtnd1 at %s\n", bridgeEID, dtn7REST)
	} else {
		fmt.Printf("[%s] Fetch loop polling dtnd2 at %s\n", bridgeEID, dtn7REST)
	}
	fmt.Println("Bridge is ready!")
	fmt.Println()

	startSerialMode(uuid, serialPort)
}
