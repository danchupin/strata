package usagerollup

import (
	"sync"

	"github.com/google/uuid"
)

// Sample is one point in the ring keyed by (bucket, storage class). Captured
// every intermediate sample tick from bucket_stats.
type Sample struct {
	Bytes   int64
	Objects int64
}

// RingKey identifies one in-memory sample stream per (bucket_id, storage_class).
type RingKey struct {
	BucketID uuid.UUID
	Class    string
}

// sampleRing holds at most cap Samples per key. Drain returns + clears the
// slice for a key (used by the daily roll-up tick).
type sampleRing struct {
	mu      sync.RWMutex
	samples map[RingKey][]Sample
	cap     int
}

func newSampleRing(cap int) *sampleRing {
	if cap < 1 {
		cap = 1
	}
	return &sampleRing{samples: make(map[RingKey][]Sample), cap: cap}
}

func (r *sampleRing) add(k RingKey, s Sample) {
	r.mu.Lock()
	defer r.mu.Unlock()
	list := r.samples[k]
	if len(list) >= r.cap {
		list = list[len(list)-r.cap+1:]
	}
	r.samples[k] = append(list, s)
}

func (r *sampleRing) drain(k RingKey) []Sample {
	r.mu.Lock()
	defer r.mu.Unlock()
	list := r.samples[k]
	delete(r.samples, k)
	return list
}

// Trapezoid integrates a sequence of byte snapshots over totalSeconds using
// the trapezoid rule. Snapshots are assumed to be uniformly spaced across the
// interval; the segment between the last snapshot and totalSeconds carries the
// last value forward (rectangle) so a flat sequence integrates back to
// snapshot[0]*totalSeconds. N=1 degrades to snapshot[0]*totalSeconds — the
// pre-trapezoid v1 math, intentional fallback when no intermediate ticks fired.
func Trapezoid(samples []int64, totalSeconds int64) int64 {
	n := len(samples)
	if n == 0 || totalSeconds <= 0 {
		return 0
	}
	if n == 1 {
		return samples[0] * totalSeconds
	}
	deltaT := totalSeconds / int64(n)
	var sum int64
	for i := 0; i < n-1; i++ {
		sum += (samples[i] + samples[i+1]) / 2 * deltaT
	}
	sum += samples[n-1] * deltaT
	return sum
}

// AverageObjects returns the integer mean over samples (sum/N).
func AverageObjects(samples []int64) int64 {
	n := int64(len(samples))
	if n == 0 {
		return 0
	}
	var sum int64
	for _, s := range samples {
		sum += s
	}
	return sum / n
}

// MaxObjects returns the largest sample.
func MaxObjects(samples []int64) int64 {
	if len(samples) == 0 {
		return 0
	}
	m := samples[0]
	for _, s := range samples[1:] {
		if s > m {
			m = s
		}
	}
	return m
}
