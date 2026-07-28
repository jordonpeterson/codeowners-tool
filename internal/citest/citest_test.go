package citest

import "testing"

// Temporary test used to verify that the CI pipeline blocks merges when tests
// fail. This is flipped to pass in a follow-up commit.
func TestCIGate(t *testing.T) {
	t.Fatal("intentional failure: verifying that CI blocks merge on failing tests")
}
