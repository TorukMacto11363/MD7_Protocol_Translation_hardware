package fragment

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
)

const MAX_FRAGMENT_SIZE = 37 // MAX_FRAGMENT_SIZE is the max data bytes per fragment.

// first byte of every packet, tells the other side what its looking at
const (
	MsgTypeFragment = 0x01
	MsgTypeNack     = 0x02
)

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
func (f *Fragment) Encode() []byte {
	buf := make([]byte, 13+len(f.Data))
	buf[0] = MsgTypeFragment
	copy(buf[1:9], f.BundleID[:])
	buf[9] = f.Index
	buf[10] = f.Total
	binary.BigEndian.PutUint16(buf[11:13], f.PayloadSize)
	copy(buf[13:], f.Data)
	return buf
}

// Decode turns raw bytes back into a Fragment. If error occurs it might just be a nack request instead of real fragment data.
func Decode(data []byte) (*Fragment, error) {
	if len(data) < 1 || data[0] != MsgTypeFragment {
		return nil, fmt.Errorf("not a fragment message")
	}
	data = data[1:]
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
