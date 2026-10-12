package cli

import (
	"os"
	"testing"
)

// TestMain keeps every test away from the person's real 1Password CLI: tests that need it pass a stand-in.
func TestMain(m *testing.M) {
	os.Setenv("CHROME_RELAY_NO_OP_CLI", "1")
	os.Exit(m.Run())
}
