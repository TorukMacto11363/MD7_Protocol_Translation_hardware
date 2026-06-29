package fragment

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
)

// MAX_FRAGMENT_SIZE is the max data bytes per fragment.
// Binary overhead is 12 bytes (8 BundleID + 1 Index + 1 Total + 2 PayloadSize).
// Total packet = 12 + MAX_FRAGMENT_SIZE bytes.
// Must fit within LoRa+PKC limit of ~50 bytes. So 50 - 12 = 38 bytes max data.
const MAX_FRAGMENT_SIZE = 38

// Fragment represents one binary-encoded piece of a larger bundle.
type Fragment struct {
	BundleID    [8]byte // 8 bytes — unique ID shared by all fragments of one bundle
	Index       uint8   // 1 byte — position in sequence (0-based)
	Total       uint8   // 1 byte — total number of fragments
	PayloadSize uint16  // 2 bytes — original full payload size
	Data        []byte  // variable — this fragment's data chunk
}

// String returns a human readable description
func (f *Fragment) String() string {
	return fmt.Sprintf("Fragment[%x index=%d/%d size=%d]",
		f.BundleID[:4], f.Index, f.Total-1, len(f.Data))
}

// Encode serialises fragment to binary bytes
// Format: [8 BundleID][1 Index][1 Total][2 PayloadSize][N Data]
func (f *Fragment) Encode() []byte {
	buf := make([]byte, 12+len(f.Data))
	copy(buf[0:8], f.BundleID[:])
	buf[8] = f.Index
	buf[9] = f.Total
	binary.BigEndian.PutUint16(buf[10:12], f.PayloadSize)
	copy(buf[12:], f.Data)
	return buf
}

// Decode deserialises fragment from binary bytes
func Decode(data []byte) (*Fragment, error) {
	if len(data) < 12 {
		return nil, fmt.Errorf("fragment too short: %d bytes", len(data))
	}
	f := &Fragment{}
	copy(f.BundleID[:], data[0:8])
	f.Index = data[8]
	f.Total = data[9]
	f.PayloadSize = binary.BigEndian.Uint16(data[10:12])
	f.Data = make([]byte, len(data)-12)
	copy(f.Data, data[12:])
	return f, nil
}

// Fragmentize splits a large payload into binary-encoded fragments
func Fragmentize(payload []byte) []*Fragment {
	var bundleID [8]byte
	rand.Read(bundleID[:])

	total := (len(payload) + MAX_FRAGMENT_SIZE - 1) / MAX_FRAGMENT_SIZE
	if total > 255 {
		total = 255 // uint8 max
	}

	var fragments []*Fragment
	for i := 0; i < total; i++ {
		start := i * MAX_FRAGMENT_SIZE
		end := start + MAX_FRAGMENT_SIZE
		if end > len(payload) {
			end = len(payload)
		}

		chunk := make([]byte, end-start)
		copy(chunk, payload[start:end])

		fragments = append(fragments, &Fragment{
			BundleID:    bundleID,
			Index:       uint8(i),
			Total:       uint8(total),
			PayloadSize: uint16(len(payload)),
			Data:        chunk,
		})
	}
	return fragments
}