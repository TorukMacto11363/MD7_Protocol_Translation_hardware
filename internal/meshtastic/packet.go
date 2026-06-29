package meshtastic

import (
	"encoding/json"
	"fmt"
)

// MeshPacket represents a packet on the Meshtastic LoRa mesh network.
// In real hardware these arrive as protobuf-encoded messages over USB serial.
// In simulation mode they are JSON-encoded and sent over TCP.
type MeshPacket struct {
	From     uint32 // sender's hardware node ID, e.g. 0xFA4371A4
	To       uint32 // destination node ID — 0xFFFFFFFF means broadcast to all
	ID       uint32 // unique packet ID used for deduplication in the mesh
	Payload  []byte // the raw data bytes being carried
	HopLimit uint8  // decremented at each relay node, starts at 3 by default
}

// BPv7Bundle is a simplified representation of a DTN Bundle Protocol v7 bundle.
// The real BPv7 format is CBOR-encoded with a primary block and payload block.
// Here we model only the fields needed for the bridge's address translation.
type BPv7Bundle struct {
	SourceEID      string // DTN endpoint of the sender, e.g. "dtn://meshtastic/a3f82b1c"
	DestinationEID string // DTN endpoint of the receiver, e.g. "dtn://meshtastic/b9d12e44"
	BundleID       string // globally unique bundle identifier
	Lifetime       int64  // seconds until the bundle should be discarded if undelivered
	Payload        []byte // the data this bundle is carrying
}

// NodeIDToEID converts a Meshtastic hardware node ID to a DTN7 Endpoint ID.
// This is one half of the protocol address translation that Goal 1 implements.
//
// Meshtastic nodes are identified by a 32-bit integer derived from the device's
// MAC address. DTN7 uses URI-style Endpoint IDs (EIDs). This function creates
// a mapping by zero-padding the hex node ID and inserting it into the
// dtn://meshtastic/ namespace.
//
// Example: 0xA3F82B1C → "dtn://meshtastic/a3f82b1c"
func NodeIDToEID(nodeID uint32) string {
	return fmt.Sprintf("dtn://meshtastic/%08x", nodeID)
}

// EIDToNodeID converts a DTN7 EID back to a Meshtastic node ID.
// This is the reverse of NodeIDToEID.
//
// Example: "dtn://meshtastic/a3f82b1c" → 0xA3F82B1C
func EIDToNodeID(eid string) (uint32, error) {
	var nodeID uint32
	_, err := fmt.Sscanf(eid, "dtn://meshtastic/%x", &nodeID)
	if err != nil {
		return 0, fmt.Errorf("invalid EID format %q: %w", eid, err)
	}
	return nodeID, nil
}

// ToBundle converts a received MeshPacket into a BPv7Bundle.
// This is used when a Meshtastic node sends data that should be
// injected into the DTN7 network — translation direction A.
func (p *MeshPacket) ToBundle() *BPv7Bundle {
	return &BPv7Bundle{
		SourceEID:      NodeIDToEID(p.From),
		DestinationEID: NodeIDToEID(p.To),
		BundleID:       fmt.Sprintf("%s-%d", NodeIDToEID(p.From), p.ID),
		Lifetime:       86400, // 24 hours
		Payload:        p.Payload,
	}
}

// ToMeshPacket converts a BPv7Bundle into a MeshPacket for transmission
// over Meshtastic. Used when a DTN bundle needs to leave the DTN network
// and travel over LoRa — translation direction B.
func (b *BPv7Bundle) ToMeshPacket() (*MeshPacket, error) {
	fromID, err := EIDToNodeID(b.SourceEID)
	if err != nil {
		return nil, fmt.Errorf("bad source EID: %w", err)
	}
	toID, err := EIDToNodeID(b.DestinationEID)
	if err != nil {
		return nil, fmt.Errorf("bad destination EID: %w", err)
	}
	return &MeshPacket{
		From:     fromID,
		To:       toID,
		ID:       0,
		Payload:  b.Payload,
		HopLimit: 3,
	}, nil
}

// Encode serialises a MeshPacket to JSON. Used in simulation mode over TCP.
func (p *MeshPacket) Encode() ([]byte, error) {
	return json.Marshal(p)
}

// DecodeMeshPacket deserialises JSON bytes into a MeshPacket.
func DecodeMeshPacket(data []byte) (*MeshPacket, error) {
	var p MeshPacket
	err := json.Unmarshal(data, &p)
	return &p, err
}

// EncodeBundle serialises a BPv7Bundle to JSON.
func (b *BPv7Bundle) Encode() ([]byte, error) {
	return json.Marshal(b)
}

// DecodeBundle deserialises JSON bytes into a BPv7Bundle.
func DecodeBundle(data []byte) (*BPv7Bundle, error) {
	var b BPv7Bundle
	err := json.Unmarshal(data, &b)
	return &b, err
}