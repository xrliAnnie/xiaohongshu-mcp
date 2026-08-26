package main

import "testing"

func TestRunWithPanicSafeCleanupContainsCleanupPanic(t *testing.T) {
	cleanupCalls := 0
	workCalls := 0

	runWithPanicSafeCleanup(func() {
		cleanupCalls++
		panic("cleanup failed")
	}, func() {
		workCalls++
	})

	if workCalls != 1 {
		t.Fatalf("work calls = %d, want 1", workCalls)
	}
	if cleanupCalls != 1 {
		t.Fatalf("cleanup calls = %d, want 1", cleanupCalls)
	}
}

func TestRunWithPanicSafeCleanupCleansAfterWorkPanic(t *testing.T) {
	cleanupCalls := 0

	runWithPanicSafeCleanup(func() {
		cleanupCalls++
	}, func() {
		panic("wait failed")
	})

	if cleanupCalls != 1 {
		t.Fatalf("cleanup calls = %d, want 1", cleanupCalls)
	}
}
