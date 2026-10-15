// Package relay is Chrome Relay's relay: a DevTools endpoint per agent, served over WebSocket on 127.0.0.1, that
// runs each agent's commands in its own background tabs through the Chrome Relay extension (one per Chrome
// profile). Only callers with this Mac's secret AND launched by Claude Code, Codex, Cursor or Paseo may connect
// as agents; only Chrome itself, with this checkout's extension, may connect as the extension.
package relay

import "github.com/aindeev/agent-chrome-relay/internal/vault"

// Options configure one relay instance (the normal one, or dev mode beside it).
type Options struct {
	Home     string // data folder: token, config.json, identities.json, relay.log, audit/
	Port     int    // 9333, or 9334 for dev mode
	Instance string // "" or "dev-mode": picks this instance's Keychain item for the 1Password token
	Version  string
	Notifier string // Chrome Relay.app, if installed: macOS notifications when an agent needs the person
	// Tests only: who is on the other end of a connection, and whether a process is Chrome (default: lsof and ps).
	Caller   func(remotePort int) Caller
	IsChrome func(pid int) bool
	// Tests only: 1Password and the person who approves, in place of the service accounts and Touch ID.
	VaultStore  vault.Store
	VaultPerson vault.Person
}
