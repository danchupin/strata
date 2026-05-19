package usagerollup

import (
	"testing"

	"github.com/google/uuid"
)

func TestTrapezoidFlatAllDayMatchesSingleSampleMath(t *testing.T) {
	samples := make([]int64, 24)
	for i := range samples {
		samples[i] = 1024
	}
	got := Trapezoid(samples, 86400)
	want := int64(1024) * 86400
	if got != want {
		t.Fatalf("flat: got %d want %d", got, want)
	}
}

func TestTrapezoidStepUpAtNoon(t *testing.T) {
	samples := make([]int64, 24)
	for i := range 12 {
		samples[i] = 100
	}
	for i := 12; i < 24; i++ {
		samples[i] = 200
	}
	got := Trapezoid(samples, 86400)
	// Hand math: 11 traps of 100 (3600s each) + 1 mid-trap (avg 150) +
	// 11 traps of 200 + last-rect 200 * 3600.
	// = 11*100*3600 + 150*3600 + 11*200*3600 + 200*3600
	// = (1100+150+2200+200) * 3600 = 3650 * 3600 = 13_140_000
	want := int64(13_140_000)
	if got != want {
		t.Fatalf("step-up: got %d want %d", got, want)
	}
	// Within ~2% of ideal (12 h × 100 + 12 h × 200 = 12_960_000).
	ideal := int64(12_960_000)
	diff := got - ideal
	if diff < 0 {
		diff = -diff
	}
	if diff*50 > ideal {
		t.Errorf("step-up off by more than 2%%: got %d ideal %d", got, ideal)
	}
}

func TestTrapezoidStepDownAtNoon(t *testing.T) {
	samples := make([]int64, 24)
	for i := range 12 {
		samples[i] = 200
	}
	for i := 12; i < 24; i++ {
		samples[i] = 100
	}
	got := Trapezoid(samples, 86400)
	// Mirror of step-up: 11*200*3600 + 150*3600 + 11*100*3600 + 100*3600
	// = (2200+150+1100+100) * 3600 = 3550 * 3600 = 12_780_000
	want := int64(12_780_000)
	if got != want {
		t.Fatalf("step-down: got %d want %d", got, want)
	}
}

func TestTrapezoidSawtooth(t *testing.T) {
	samples := make([]int64, 24)
	for i := range samples {
		if i%2 == 0 {
			samples[i] = 0
		} else {
			samples[i] = 100
		}
	}
	got := Trapezoid(samples, 86400)
	// Each adjacent pair averages 50; 23 traps of 50*3600 = 4_140_000.
	// Last-rect samples[23]=100 * 3600 = 360_000. Total = 4_500_000.
	want := int64(4_500_000)
	if got != want {
		t.Fatalf("sawtooth: got %d want %d", got, want)
	}
}

func TestTrapezoidSingleSampleEqualsLegacyMath(t *testing.T) {
	got := Trapezoid([]int64{1024}, 86400)
	want := int64(1024) * 86400
	if got != want {
		t.Fatalf("single-sample: got %d want %d", got, want)
	}
}

func TestTrapezoidEmptyReturnsZero(t *testing.T) {
	if got := Trapezoid(nil, 86400); got != 0 {
		t.Fatalf("nil samples: got %d want 0", got)
	}
	if got := Trapezoid([]int64{}, 86400); got != 0 {
		t.Fatalf("empty samples: got %d want 0", got)
	}
}

func TestTrapezoidZeroDuration(t *testing.T) {
	if got := Trapezoid([]int64{1024, 2048}, 0); got != 0 {
		t.Fatalf("zero duration: got %d want 0", got)
	}
}

func TestAverageAndMaxObjects(t *testing.T) {
	samples := []int64{10, 20, 30, 40, 50}
	if got := AverageObjects(samples); got != 30 {
		t.Errorf("avg: got %d want 30", got)
	}
	if got := MaxObjects(samples); got != 50 {
		t.Errorf("max: got %d want 50", got)
	}
	if got := AverageObjects(nil); got != 0 {
		t.Errorf("avg(nil): got %d want 0", got)
	}
	if got := MaxObjects(nil); got != 0 {
		t.Errorf("max(nil): got %d want 0", got)
	}
}

func TestSampleRingDrainsAndCapsToWindow(t *testing.T) {
	r := newSampleRing(3)
	k := RingKey{BucketID: uuid.New(), Class: "STANDARD"}
	r.add(k, Sample{Bytes: 1, Objects: 1})
	r.add(k, Sample{Bytes: 2, Objects: 2})
	r.add(k, Sample{Bytes: 3, Objects: 3})
	r.add(k, Sample{Bytes: 4, Objects: 4})
	got := r.drain(k)
	if len(got) != 3 {
		t.Fatalf("ring len: got %d want 3", len(got))
	}
	// Oldest dropped: should be [2,3,4].
	want := []int64{2, 3, 4}
	for i, s := range got {
		if s.Bytes != want[i] {
			t.Errorf("ring[%d].Bytes = %d want %d", i, s.Bytes, want[i])
		}
	}
	if drained := r.drain(k); len(drained) != 0 {
		t.Fatalf("second drain should be empty: got %d", len(drained))
	}
}
