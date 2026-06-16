// Package stats provides request rate and bandwidth tracking.
package stats

import (
	"sync"
	"time"
)

type entry struct {
	timestamp time.Time
	bytesIn   int64
	bytesOut  int64
}

// Tracker tracks requests and bandwidth with a 60-second sliding window.
type Tracker struct {
	mu      sync.Mutex
	entries []entry
}

var Global = &Tracker{}

// Record records a request with the given response size.
func (t *Tracker) Record(bytesIn, bytesOut int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	t.entries = append(t.entries, entry{timestamp: now, bytesIn: int64(bytesIn), bytesOut: int64(bytesOut)})
	cutoff := now.Add(-60 * time.Second)
	i := 0
	for i < len(t.entries) && t.entries[i].timestamp.Before(cutoff) { i++ }
	if i > 0 { t.entries = t.entries[i:] }
}

// ReqPerSec returns the average requests per second over the last 60 seconds.
func (t *Tracker) ReqPerSec() float64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-60 * time.Second)
	count := 0
	for _, e := range t.entries {
		if e.timestamp.After(cutoff) { count++ }
	}
	if count == 0 { return 0 }
	return float64(count) / 60.0
}

// ByteRate returns incoming and outgoing bytes per second.
func (t *Tracker) ByteRate() (in, out float64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-60 * time.Second)
	var totalIn, totalOut int64
	for _, e := range t.entries {
		if e.timestamp.After(cutoff) { totalIn += e.bytesIn; totalOut += e.bytesOut }
	}
	if totalIn == 0 && totalOut == 0 { return 0, 0 }
	return float64(totalIn) / 60.0, float64(totalOut) / 60.0
}
