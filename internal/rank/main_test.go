package rank

import (
	"os"
	"testing"
)

// TestMain pins the composite weights to the 75/25 the fixtures were built
// for, and the persona pair to 20/80, so changing weights.go does not break
// ranking tests.
func TestMain(m *testing.M) {
	scoreWeight, priceWeight = 0.75, 0.25
	personaScoreWeight, personaPriceWeight = 0.20, 0.80
	os.Exit(m.Run())
}
