package discounts

import (
	"os"
	"testing"
	"time"
)

// TestMain pins the Horizon to a day: the window fixtures average over 24 h.
// TestShortHorizon covers the production value.
func TestMain(m *testing.M) {
	Horizon = 24 * time.Hour
	os.Exit(m.Run())
}
