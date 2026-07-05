package meshtastic

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"time"
)

// SerialClient is the Go side of the Python sidecar bridge.

type SerialClient struct {
	socketPath string
	conn       net.Conn
	onReceive  func(from uint32, to uint32, payload []byte)
}

// NewSerialClient connects to the Python sidecar at the given Unix socket path.
// It retries for up to 60 seconds to allow the sidecar time to start up.
func NewSerialClient(socketPath string) (*SerialClient, error) {
	fmt.Printf("[SIDECAR] Connecting to Meshtastic sidecar at %s...\n", socketPath)

	var conn net.Conn
	var err error

	// Retry loop — the sidecar may still be initialising the Meshtastic interface
	for i := 0; i < 30; i++ {
		conn, err = net.Dial("unix", socketPath)
		if err == nil {
			break
		}
		fmt.Printf("[SIDECAR] Waiting for sidecar... (%d/30)\n", i+1)
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		return nil, fmt.Errorf("could not connect to sidecar after 60s: %w", err)
	}

	fmt.Printf("[SIDECAR] Connected to Meshtastic sidecar\n")
	return &SerialClient{
		socketPath: socketPath,
		conn:       conn,
	}, nil
}

// SetReceiveHandler registers the function that will be called whenever a packet arrives from the Meshtastic mesh via the sidecar.
// from and to are always 0 for the current testing.
func (sc *SerialClient) SetReceiveHandler(fn func(from uint32, to uint32, payload []byte)) {
	sc.onReceive = fn
}

// Start reads incoming packets from the sidecar in a loop. Each packet is a 2-byte length prefix followed by the payload bytes.
// This call blocks until the sidecar disconnects.
func (sc *SerialClient) Start() {
	fmt.Printf("[SIDECAR] Listening for packets from Meshtastic mesh...\n")

	for {
		// Read the 2-byte length prefix
		var length uint16
		err := binary.Read(sc.conn, binary.BigEndian, &length)
		if err != nil {
			if err == io.EOF {
				fmt.Printf("[SIDECAR] Sidecar disconnected\n")
			} else {
				fmt.Printf("[SIDECAR] Read error: %v\n", err)
			}
			return
		}

		// Read exactly `length` bytes of payload
		data := make([]byte, length)
		_, err = io.ReadFull(sc.conn, data)
		if err != nil {
			fmt.Printf("[SIDECAR] Failed to read payload: %v\n", err)
			return
		}

		fmt.Printf("[SIDECAR] Received %d bytes from mesh\n", len(data))

		if sc.onReceive != nil {
			sc.onReceive(0, 0, data)
		}
	}
}

// SendPacket sends payload bytes to the Meshtastic mesh via the sidecar.
// The `to` parameter is passed for logging; actual routing is broadcast (0xFFFFFFFF) and is set by the sidecar's sendData call.
func (sc *SerialClient) SendPacket(to uint32, payload []byte) error {
	length := uint16(len(payload))

	if err := binary.Write(sc.conn, binary.BigEndian, length); err != nil {
		return fmt.Errorf("failed to write length prefix: %w", err)
	}

	if _, err := sc.conn.Write(payload); err != nil {
		return fmt.Errorf("failed to write payload: %w", err)
	}

	fmt.Printf("[SIDECAR] Sent %d bytes to mesh (node %08x)\n", len(payload), to)
	return nil
}

// Close shuts down the connection to the sidecar.
func (sc *SerialClient) Close() error {
	return sc.conn.Close()
}
