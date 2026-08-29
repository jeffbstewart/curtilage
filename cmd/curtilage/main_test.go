package main

import "testing"

func TestVersionString(t *testing.T) {
	if got := versionString(); got != "curtilage dev" {
		t.Fatalf("versionString() = %q", got)
	}
}
