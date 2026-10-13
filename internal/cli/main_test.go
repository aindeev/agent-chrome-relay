package cli

import (
	"os"
	"testing"
)

// TestMain keeps every test away from the person's real 1Password CLI (tests that need it pass a stand-in) and
// from the real push relay.
func TestMain(m *testing.M) {
	os.Setenv("CHROME_RELAY_NO_OP_CLI", "1")
	os.Setenv("CHROME_RELAY_PUSH_RELAY", "off")
	os.Exit(m.Run())
}
