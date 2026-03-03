package main

import "testing"

func TestMaskAPIKey(t *testing.T) {
	if got := maskAPIKey("short"); got != "***" {
		t.Fatalf("maskAPIKey(short) = %q, want ***", got)
	}
	if got := maskAPIKey("1234567890"); got != "1234...7890" {
		t.Fatalf("maskAPIKey(1234567890) = %q, want 1234...7890", got)
	}
}
