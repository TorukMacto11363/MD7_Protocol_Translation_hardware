package fragment

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

type ReassemblyBuffer struct {
	BundleID    [8]byte
	Fragments   []*Fragment
	Total       int
	PayloadSize int
	FirstSeen   time.Time
}

func (rb *ReassemblyBuffer) isComplete() bool {
	return len(rb.Fragments) == rb.Total
}

func (rb *ReassemblyBuffer) reassemble() ([]byte, error) {
	sort.Slice(rb.Fragments, func(i, j int) bool {
		return rb.Fragments[i].Index < rb.Fragments[j].Index
	})
	for i, f := range rb.Fragments {
		if int(f.Index) != i {
			return nil, fmt.Errorf("missing fragment %d for bundle %x", i, rb.BundleID[:4])
		}
	}
	result := make([]byte, 0, rb.PayloadSize)
	for _, f := range rb.Fragments {
		result = append(result, f.Data...)
	}
	return result, nil
}

type Reassembler struct {
	mu      sync.Mutex
	buffers map[[8]byte]*ReassemblyBuffer
	timeout time.Duration
}

func NewReassembler(timeout time.Duration) *Reassembler {
	r := &Reassembler{
		buffers: make(map[[8]byte]*ReassemblyBuffer),
		timeout: timeout,
	}
	go r.cleanupLoop()
	return r
}

func (r *Reassembler) AddFragment(f *Fragment) ([]byte, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	buf, exists := r.buffers[f.BundleID]
	if !exists {
		buf = &ReassemblyBuffer{
			BundleID:    f.BundleID,
			Total:       int(f.Total),
			PayloadSize: int(f.PayloadSize),
			FirstSeen:   time.Now(),
		}
		r.buffers[f.BundleID] = buf
		fmt.Printf("[REASSEMBLER] New bundle started: %x (expecting %d fragments)\n",
			f.BundleID[:4], f.Total)
	}

	buf.Fragments = append(buf.Fragments, f)
	fmt.Printf("[REASSEMBLER] Got fragment %d/%d for bundle %x\n",
		f.Index+1, f.Total, f.BundleID[:4])

	if buf.isComplete() {
		payload, err := buf.reassemble()
		if err != nil {
			fmt.Printf("[REASSEMBLER] Reassembly failed: %v\n", err)
			delete(r.buffers, f.BundleID)
			return nil, false
		}
		fmt.Printf("[REASSEMBLER] Bundle %x complete! Reassembled %d bytes\n",
			f.BundleID[:4], len(payload))
		delete(r.buffers, f.BundleID)
		return payload, true
	}
	return nil, false
}

func (r *Reassembler) cleanupLoop() {
	for {
		time.Sleep(1 * time.Minute)
		r.mu.Lock()
		now := time.Now()
		for id, buf := range r.buffers {
			if now.Sub(buf.FirstSeen) > r.timeout {
				fmt.Printf("[REASSEMBLER] Discarding incomplete bundle %x (timeout)\n", id[:4])
				delete(r.buffers, id)
			}
		}
		r.mu.Unlock()
	}
}