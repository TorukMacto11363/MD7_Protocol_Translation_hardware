package fragment

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

type ReassemblyBuffer struct {
	BundleID     [8]byte
	Fragments    []*Fragment
	Total        int
	PayloadSize  int
	FirstSeen    time.Time
	LastSeen     time.Time
	LastNackSent time.Time
	NackAttempts int
}

func (rb *ReassemblyBuffer) isComplete() bool {
	return len(rb.Fragments) == rb.Total
}

// missingIndices returns which fragment indices (0..Total-1) haven't arrived yet.
func (rb *ReassemblyBuffer) missingIndices() []uint8 {
	have := make(map[uint8]bool, len(rb.Fragments))
	for _, f := range rb.Fragments {
		have[f.Index] = true
	}
	var missing []uint8
	for i := 0; i < rb.Total; i++ {
		if !have[uint8(i)] {
			missing = append(missing, uint8(i))
		}
	}
	return missing
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
	buf.LastSeen = time.Now()

	// skip duplicates - a retry resend (or the mesh just rebroadcasting on its own) can hand us a fragment we already have. if we count it twice, len(Fragments) overshoots Total and isComplete() never matches again, bundle just hangs.
	for _, existing := range buf.Fragments {
		if existing.Index == f.Index {
			fmt.Printf("[REASSEMBLER] Duplicate fragment %d/%d for bundle %x - ignored\n",
				f.Index+1, f.Total, f.BundleID[:4])
			return nil, false
		}
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

// CheckStalled looks for bundles that stopped receiving fragments (nothing new in quietPeriod) and returns a nack request for each, asking for exactly what's missing.
// Only asks once per quietPeriod per bundle so we're not spamming the sender, and gives up entirely after maxAttempts.
func (r *Reassembler) CheckStalled(quietPeriod time.Duration, maxAttempts int) []*NackRequest {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	var requests []*NackRequest
	for id, buf := range r.buffers {
		if now.Sub(buf.LastSeen) < quietPeriod {
			continue // still actively receiving fragments, don't interrupt
		}
		if now.Sub(buf.LastNackSent) < quietPeriod {
			continue // already asked recently, give the sender time to respond
		}

		missing := buf.missingIndices()
		if len(missing) == 0 {
			continue // shouldn't happen, complete buffers are deleted
		}

		if buf.NackAttempts >= maxAttempts {
			fmt.Printf("[REASSEMBLER] Giving up on bundle %x after %d retry attempts - still missing %v\n",
				id[:4], buf.NackAttempts, missing)
			delete(r.buffers, id)
			continue
		}

		buf.LastNackSent = now
		buf.NackAttempts++
		requests = append(requests, &NackRequest{BundleID: id, Missing: missing})
	}
	return requests
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
