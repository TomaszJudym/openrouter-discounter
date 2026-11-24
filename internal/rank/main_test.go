package rank

import (
	"os"
	"testing"
)

// TestMain pins the composite weights to the 75/25 the fixtures were built
// for, so changing weights.go does not break ranking tests.
func TestMain(m *testing.M) {
	scoreWeight, priceWeight = 0.75, 0.25
	os.Exit(m.Run())
}
