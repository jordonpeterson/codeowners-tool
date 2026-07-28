package citest

import "testing"

// Temporary test used to verify that the CI pipeline blocks merges when tests
// fail and permits them when tests pass.
func TestCIGate(t *testing.T) {
	if 1+1 != 2 {
		t.Fatal("math is broken")
	}
}
