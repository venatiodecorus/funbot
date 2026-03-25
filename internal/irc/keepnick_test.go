package irc

import (
	"testing"
)

func TestKeepNick_DesiredNick(t *testing.T) {
	kn := &KeepNick{
		desiredNick: "coolnick",
		interval:    DefaultKeepNickInterval,
	}

	if kn.DesiredNick() != "coolnick" {
		t.Errorf("expected 'coolnick', got %q", kn.DesiredNick())
	}
}

func TestKeepNick_InactiveByDefault(t *testing.T) {
	kn := &KeepNick{
		desiredNick: "coolnick",
	}

	if kn.IsActive() {
		t.Error("expected not active initially")
	}
}

func TestKeepNick_StopWhenNotStarted(t *testing.T) {
	kn := &KeepNick{
		desiredNick: "coolnick",
	}

	// Should not panic
	kn.Stop()

	if kn.IsActive() {
		t.Error("should not be active after stop")
	}
}

func TestKeepNick_AcquiredWhenNickMatches(t *testing.T) {
	// Create a minimal KeepNick with a mock-like client
	// We can't easily create a real Client without girc, but we can
	// test the acquired flag directly
	kn := &KeepNick{
		desiredNick: "coolnick",
		acquired:    true,
	}

	if !kn.IsAcquired() {
		t.Error("expected acquired when flag is set")
	}
}

func TestKeepNick_DefaultInterval(t *testing.T) {
	if DefaultKeepNickInterval.Seconds() != 30 {
		t.Errorf("expected 30s default interval, got %v", DefaultKeepNickInterval)
	}
}
