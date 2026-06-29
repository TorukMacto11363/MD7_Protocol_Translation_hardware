package main

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/TorukMacto/md7-bridge/internal/fragment"
	"github.com/TorukMacto/md7-bridge/internal/meshtastic"
)

type FetchResponse struct {
	Error   string            `json:"error"`
	Bundles []json.RawMessage `json:"bundles"`
}

type BundlePayload struct {
	From    string `json:"from"`
	To      string `json:"to"`
	Payload string `json:"payload"`
}

// MeshPacket must match fake_meshtastic and packet.go
type MeshPacket struct {
	From     uint32
	To       uint32
	ID       uint32
	Payload  []byte
	HopLimit uint8
}

var dtn7REST = "http://localhost:8080/rest"

var meshPort = ":9000"
var bridgeEID = "dtn://test/bridge1"

// Reassembler handles incoming fragments — 10 min timeout for hardware
var reassembler = fragment.NewReassembler(10 * time.Minute)
var serialPort = ""

func nodeIDToEID(nodeID uint32) string {
	return fmt.Sprintf("dtn://meshtastic/%08x", nodeID)
}

// --- DTN7 REST API structs ---

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

// --- DTN7 helper functions ---

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

// sendFragmented is used in SIMULATION mode only
// In hardware mode, startSerialMode handles fragmentation
func sendFragmented(conn net.Conn, payload []byte) error {
	fragments := fragment.Fragmentize(payload)

	fmt.Printf("  [FRAGMENTER] Splitting %d bytes into %d fragment(s)\n",
		len(payload), len(fragments))

	for _, f := range fragments {
		// SIMULATION uses JSON encoding over TCP
		fragData, err := json.Marshal(f)
		if err != nil {
			return fmt.Errorf("fragment encode error: %w", err)
		}

		if len(fragData) > 230 {
			fmt.Printf("  [WARNING] Fragment JSON is %d bytes — exceeds LoRa limit\n",
				len(fragData))
		}

		length := uint32(len(fragData))
		binary.Write(conn, binary.BigEndian, length)
		conn.Write(fragData)

		fmt.Printf("  [FRAGMENTER] Sent %s\n", f.String())
		time.Sleep(100 * time.Millisecond)
	}
	return nil
}

// handleMeshConnection is used in SIMULATION mode only (TCP)
func handleMeshConnection(conn net.Conn, uuid string) {
	defer conn.Close()
	fmt.Printf("[%s] Meshtastic node connected on %s\n", bridgeEID, meshPort)

	if bridgeEID == "dtn://test/bridge1" {
		go startFetchLoop(uuid, &conn)
	}

	for {
		var length uint32
		err := binary.Read(conn, binary.BigEndian, &length)
		if err != nil {
			fmt.Printf("[%s] Meshtastic node disconnected\n", bridgeEID)
			return
		}

		data := make([]byte, length)
		_, err = io.ReadFull(conn, data)
		if err != nil {
			fmt.Printf("[%s] Read error: %v\n", bridgeEID, err)
			return
		}

		var packet MeshPacket
		err = json.Unmarshal(data, &packet)

		// Simulation uses JSON fragments with string BundleID
		var simFrag struct {
			BundleID    string `json:"BundleID"`
			Index       int    `json:"Index"`
			Total       int    `json:"Total"`
			PayloadSize int    `json:"PayloadSize"`
			Data        []byte `json:"Data"`
		}
		fragErr := json.Unmarshal(data, &simFrag)

		if fragErr == nil && simFrag.BundleID != "" {
			var bundleID [8]byte
			copy(bundleID[:], simFrag.BundleID)
			f := &fragment.Fragment{
				BundleID:    bundleID,
				Index:       uint8(simFrag.Index),
				Total:       uint8(simFrag.Total),
				PayloadSize: uint16(simFrag.PayloadSize),
				Data:        simFrag.Data,
			}
			fmt.Printf("\n[MESH→REASSEMBLER] %s received fragment\n", bridgeEID)
			fmt.Printf("  Fragment: %s\n", f.String())

			payload, complete := reassembler.AddFragment(f)
			if complete {
				fmt.Printf("\n[REASSEMBLER→DTN7] Bundle complete, sending to DTN7\n")
				localAppEID := strings.Replace(bridgeEID, "bridge", "app", 1)
				err := sendBundle(uuid, bridgeEID, localAppEID, payload)
				if err != nil {
					fmt.Printf("  [ERROR] Delivery to dtnd failed: %v\n", err)
				} else {
					fmt.Printf("\n╔══════════════════════════════════════════╗\n")
					fmt.Printf("║  BUNDLE DELIVERED TO DTN2 SUCCESSFULLY   ║\n")
					fmt.Printf("║  Endpoint: %-30s ║\n", localAppEID)
					fmt.Printf("║  Size: %-35d ║\n", len(payload))
					fmt.Printf("╚══════════════════════════════════════════╝\n\n")
				}
			}
		} else {
			var packet MeshPacket
			json.Unmarshal(data, &packet)

			fmt.Printf("\n[MESH→DTN7] %s received MeshPacket on port %s\n",
				bridgeEID, meshPort)
			fmt.Printf("  From:    %08x\n", packet.From)
			fmt.Printf("  To:      %08x\n", packet.To)
			fmt.Printf("  Payload: %s\n", string(packet.Payload))

			meshPayload, _ := json.Marshal(map[string]interface{}{
				"from":    fmt.Sprintf("%08x", packet.From),
				"to":      fmt.Sprintf("%08x", packet.To),
				"payload": string(packet.Payload),
			})

			var destBridgeEID string
			if bridgeEID == "dtn://test/bridge1" {
				destBridgeEID = "dtn://node2/bridge"
			} else {
				destBridgeEID = "dtn://test/bridge1"
			}

			err := sendBundle(uuid, bridgeEID, destBridgeEID, meshPayload)
			if err != nil {
				fmt.Printf("  [ERROR] DTN7 send failed: %v\n", err)
			} else {
				fmt.Printf("  → Bundle sent to DTN7 ✅\n")
			}
		}
	}
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

// startFetchLoop is used in SIMULATION mode only
func startFetchLoop(uuid string, meshConn *net.Conn) {
	fmt.Printf("[%s] DTN7 fetch loop started — polling %s every 5 seconds\n", bridgeEID, dtn7REST)
	for {
		time.Sleep(5 * time.Second)

		bundles, err := fetchBundles(uuid)
		if err != nil {
			fmt.Printf("[%s] Fetch error: %v\n", bridgeEID, err)
			continue
		}

		if len(bundles) == 0 {
			continue
		}

		fmt.Printf("\n[DTN7→MESH] %s fetched %d bundle(s) from DTN7\n", bridgeEID, len(bundles))

		for _, bp := range bundles {
			fmt.Printf("  Payload: %.50s%s\n", bp.Payload, func() string {
				if len(bp.Payload) > 50 {
					return "..."
				}
				return ""
			}())
			fmt.Printf("  Payload size: %d bytes\n", len(bp.Payload))

			if meshConn != nil && *meshConn != nil {
				payloadBytes := []byte(bp.Payload)
				fragments := fragment.Fragmentize(payloadBytes)
				fmt.Printf("  → Splitting into %d fragment(s) for Meshtastic\n", len(fragments))
				err := sendFragmented(*meshConn, payloadBytes)
				if err != nil {
					fmt.Printf("  [ERROR] Fragment send failed: %v\n", err)
				} else {
					fmt.Printf("  → Sent %d fragment(s) to Meshtastic node ✅\n", len(fragments))
				}
			} else {
				fmt.Printf("  [WAITING] No Meshtastic node connected yet — bundle queued\n")
			}
		}
	}
}

// startSerialMode runs the bridge using real Meshtastic hardware via Python sidecar
// HARDWARE MODE: uses binary fragment encoding (not JSON) to fit within LoRa+PKC packet limits
func startSerialMode(uuid string, portName string) {
	fmt.Printf("[HARDWARE] Starting serial mode on %s\n", portName)

	client, err := meshtastic.NewSerialClient(portName)
	if err != nil {
		fmt.Printf("[HARDWARE] ERROR connecting to Meshtastic sidecar: %v\n", err)
		return
	}
	defer client.Close()

	// When a packet arrives from the Meshtastic mesh via the Python sidecar
	client.SetReceiveHandler(func(from uint32, to uint32, payload []byte) {
		fmt.Printf("\n[MESH→REASSEMBLER] Received %d bytes from %08x\n",
			len(payload), from)

		// HARDWARE: use binary decode (not JSON)
		f, err := fragment.Decode(payload)
		if err == nil {
			fmt.Printf("  Fragment: %s\n", f.String())
			result, complete := reassembler.AddFragment(f)
			if complete {
				fmt.Printf("[REASSEMBLER→DTN7] Bundle complete! Sending to DTN7\n")
				localAppEID := strings.Replace(bridgeEID, "bridge", "app", 1)
				err := sendBundle(uuid, bridgeEID, localAppEID, result)
				if err != nil {
					fmt.Printf("  [ERROR] DTN7 delivery failed: %v\n", err)
				} else {
					fmt.Printf("\n╔══════════════════════════════════════════╗\n")
					fmt.Printf("║  BUNDLE DELIVERED TO DTN SUCCESSFULLY    ║\n")
					fmt.Printf("║  Endpoint: %-30s ║\n", localAppEID)
					fmt.Printf("║  Size: %-35d ║\n", len(result))
					fmt.Printf("╚══════════════════════════════════════════╝\n\n")
				}
			}
		} else {
			// Not a valid fragment — forward as raw DTN payload
			fmt.Printf("  [INFO] Non-fragment payload received — forwarding to DTN7\n")
			meshPayload, _ := json.Marshal(map[string]interface{}{
				"from":    fmt.Sprintf("%08x", from),
				"to":      fmt.Sprintf("%08x", to),
				"payload": string(payload),
			})
			var destEID string
			if bridgeEID == "dtn://test/bridge1" {
				destEID = "dtn://node2/bridge"
			} else {
				destEID = "dtn://test/bridge1"
			}
			sendBundle(uuid, bridgeEID, destEID, meshPayload)
		}
	})

	// Only Bridge 1 fetches from DTN7 and pushes to Meshtastic mesh
	if bridgeEID == "dtn://test/bridge1" {
		go func() {
			fmt.Printf("[HARDWARE] Fetch loop started — polling DTN7 every 5 seconds\n")
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

					// HARDWARE: use binary fragment encoding — each fragment is 12+38=50 bytes max
					fragments := fragment.Fragmentize(payloadBytes)
					fmt.Printf("  Sending %d fragment(s) over LoRa\n", len(fragments))
					for _, f := range fragments {
						data := f.Encode() // binary encode — compact, fits in LoRa packet
						fmt.Printf("  [FRAGMENTER] %s → %d bytes binary\n", f.String(), len(data))
						err := client.SendPacket(0xFFFFFFFF, data)
						if err != nil {
							fmt.Printf("  [ERROR] LoRa send failed: %v\n", err)
						} else {
							fmt.Printf("  [MESH] Sent fragment %d/%d (%d bytes) ✅\n",
								f.Index+1, f.Total, len(data))
						}
						time.Sleep(500 * time.Millisecond)
					}
				}
			}
		}()
	}

	// Start listening — blocks until sidecar disconnects
	fmt.Printf("[HARDWARE] Listening for packets from Meshtastic sidecar...\n")
	client.Start()
}

func main() {
	if len(os.Args) >= 3 {
		meshPort = os.Args[1]
		bridgeEID = os.Args[2]
	}
	if len(os.Args) >= 4 {
		dtn7REST = os.Args[3]
	}
	if len(os.Args) >= 5 {
		serialPort = os.Args[4]
		fmt.Printf("Hardware mode — serial port: %s\n", serialPort)
	} else {
		fmt.Println("Simulation mode — using TCP")
	}
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

	if bridgeEID == "dtn://test/bridge1" {
		fmt.Printf("[%s] Listening for MeshtasticNode1 on port %s\n", bridgeEID, meshPort)
		fmt.Printf("[%s] Fetch loop polling dtnd1 at %s\n", bridgeEID, dtn7REST)
	} else {
		fmt.Printf("[%s] Listening for MeshtasticNode2 on port %s\n", bridgeEID, meshPort)
		fmt.Printf("[%s] Fetch loop polling dtnd2 at %s\n", bridgeEID, dtn7REST)
	}
	listener, err := net.Listen("tcp", meshPort)
	if err != nil {
		fmt.Println("ERROR: Could not start listener:", err)
		return
	}
	defer listener.Close()
	fmt.Println("Bridge is ready! Both directions active.")
	fmt.Println()

	if serialPort != "" {
		listener.Close()
		startSerialMode(uuid, serialPort)
	} else {
		for {
			conn, err := listener.Accept()
			if err != nil {
				fmt.Println("Accept error:", err)
				continue
			}
			go handleMeshConnection(conn, uuid)
		}
	}
}