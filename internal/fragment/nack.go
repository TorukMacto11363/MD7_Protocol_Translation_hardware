package fragment

import "fmt"

// NackRequest - a receiver sends this back over the mesh when it's missing ragments, asking the sender to resend just those instead of the whole bundle.
// format: [1 MsgType][8 BundleID][1 count][count x 1-byte fragment index]
type NackRequest struct {
	BundleID [8]byte
	Missing  []uint8
}

func (n *NackRequest) Encode() []byte {
	buf := make([]byte, 10+len(n.Missing))
	buf[0] = MsgTypeNack
	copy(buf[1:9], n.BundleID[:])
	buf[9] = uint8(len(n.Missing))
	copy(buf[10:], n.Missing)
	return buf
}

// DecodeNack parses a nack request out of raw bytes. Errors out if this is actually fragment data - try Decode in that case.
func DecodeNack(data []byte) (*NackRequest, error) {
	if len(data) < 1 || data[0] != MsgTypeNack {
		return nil, fmt.Errorf("not a nack message")
	}
	if len(data) < 10 {
		return nil, fmt.Errorf("nack message too short: %d bytes", len(data))
	}
	n := &NackRequest{}
	copy(n.BundleID[:], data[1:9])
	count := int(data[9])
	if len(data) < 10+count {
		return nil, fmt.Errorf("nack message truncated: expected %d indices, got %d bytes", count, len(data)-10)
	}
	n.Missing = make([]uint8, count)
	copy(n.Missing, data[10:10+count])
	return n, nil
}
