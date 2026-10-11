package cli

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/aindeev/agent-chrome-relay/internal/macos"
)

// passkeys shows or changes how agent tabs answer passkey requests ("passkeys" in config.json; the relay reads
// it for every new tab and sign-in, so nothing restarts):
//
//	chrome-relay passkeys                     what's set
//	chrome-relay passkeys on | off            answer passkey requests in agent tabs, or leave pages' WebAuthn alone
//	chrome-relay passkeys reuse <seconds|off> let one Touch ID cover an agent's passkey sign-ins for that long (max 300)
func (c *cli) passkeys() error {
	cfg := c.config()
	pk, _ := cfg.get("passkeys").(*object)
	if pk == nil {
		pk = newObject()
	}
	switch c.arg(2) {
	case "":
	case "on", "off":
		pk.set("enabled", c.arg(2) == "on")
	case "reuse":
		secs := 0
		if v := c.arg(3); v != "off" && v != "0" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 300 {
				return fmt.Errorf("usage: %s passkeys reuse <1-300 seconds | off>", c.Name)
			}
			secs = n
		}
		pk.set("touchIdReuseSeconds", json.Number(strconv.Itoa(secs)))
	default:
		return fmt.Errorf("usage: %s passkeys [on | off | reuse <seconds|off>]", c.Name)
	}
	if c.arg(2) != "" {
		cfg.set("passkeys", pk)
		if err := c.saveConfig(cfg); err != nil {
			return err
		}
	}
	on, _ := pk.get("enabled").(bool)
	if pk.get("enabled") == nil {
		on = true
	}
	if !on {
		c.printf("Passkeys in agent tabs: off. Pages' own passkey requests are left alone (Chrome refuses them in background tabs).\n")
		return nil
	}
	reuse := 0
	if n, ok := pk.get("touchIdReuseSeconds").(json.Number); ok {
		v, _ := n.Int64()
		reuse = int(min(v, 300))
	}
	c.printf("Passkeys in agent tabs: on. ")
	if !macos.Linked {
		c.printf("This chrome-relay was built without Xcode's command line tools, so it keeps no passkeys of its own:\n" +
			"every passkey request goes to 1Password's prompt.\n")
		return nil
	}
	c.printf("Passkeys Chrome Relay made live in this Mac's Secure Enclave; ")
	if reuse > 0 {
		c.printf("one Touch ID covers an agent's passkey sign-ins for %d seconds.\n", reuse)
	} else {
		c.printf("each sign-in asks for Touch ID.\n")
	}
	c.printf("Passkeys only 1Password has go to 1Password's own prompt. Change with `%s passkeys on|off|reuse <seconds|off>`.\n", c.Name)
	return nil
}
