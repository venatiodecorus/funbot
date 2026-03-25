package irc

import (
	"testing"
	"time"
)

func TestFloodControl_Delay(t *testing.T) {
	fc := NewFloodControl(500 * time.Millisecond)
	if fc.Delay() != 500*time.Millisecond {
		t.Errorf("expected 500ms delay, got %v", fc.Delay())
	}
}

func TestFloodControl_Wait(t *testing.T) {
	fc := NewFloodControl(50 * time.Millisecond)

	// First call should not wait
	start := time.Now()
	fc.Wait()
	elapsed := time.Since(start)
	if elapsed > 10*time.Millisecond {
		t.Errorf("first Wait took too long: %v", elapsed)
	}

	// Second call should wait approximately 50ms
	start = time.Now()
	fc.Wait()
	elapsed = time.Since(start)
	if elapsed < 40*time.Millisecond {
		t.Errorf("second Wait was too fast: %v", elapsed)
	}
	if elapsed > 100*time.Millisecond {
		t.Errorf("second Wait was too slow: %v", elapsed)
	}
}
