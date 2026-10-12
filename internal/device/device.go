// Package device names this Mac for the records Chrome Relay keeps in shared 1Password vaults. Several Macs can
// share the vaults, but a Chrome Relay passkey belongs to one of them: its key is in that Mac's Secure Enclave.
package device

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
)

var platformUUID = regexp.MustCompile(`"IOPlatformUUID" = "([0-9A-Fa-f-]+)"`)

// ID is this Mac's, the same across reinstalls and renames: a hash of its hardware UUID (never the UUID itself).
var ID = sync.OnceValue(func() string {
	src := ""
	if out, err := exec.Command("ioreg", "-rd1", "-c", "IOPlatformExpertDevice").Output(); err == nil {
		if m := platformUUID.FindSubmatch(out); m != nil {
			src = string(m[1])
		}
	}
	if src == "" { // not a Mac, or ioreg missing: the host name is the best there is
		src, _ = os.Hostname()
	}
	return Hash(src)
})

// Hash turns a hardware UUID into the ID that goes into the vault.
func Hash(uuid string) string {
	sum := sha256.Sum256([]byte("chrome-relay device\n" + strings.ToUpper(uuid)))
	return hex.EncodeToString(sum[:8])
}

// Name is what the person calls this Mac ("Sam's MacBook Pro"), for titles and prompts.
func Name() string {
	if out, err := exec.Command("scutil", "--get", "ComputerName").Output(); err == nil && strings.TrimSpace(string(out)) != "" {
		return strings.TrimSpace(string(out))
	}
	h, _ := os.Hostname()
	return strings.TrimSuffix(h, ".local")
}
