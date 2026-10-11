// Package relay is Chrome Relay's relay: a DevTools endpoint per agent, served over WebSocket on 127.0.0.1, that
// runs each agent's commands in its own background tabs through the Chrome Relay extension (one per Chrome
// profile). Only callers with this Mac's secret AND launched by Claude Code, Codex, Cursor or Paseo may connect
// as agents; only Chrome itself, with this checkout's extension, may connect as the extension.
package relay

// Options configure one relay instance (the normal one, or dev mode beside it).
type Options struct {
	Home     string // data folder: token, config.json, identities.json, relay.log, audit/
	Port     int    // 9333, or 9334 for dev mode
	Instance string // "" or "dev-mode": picks this instance's Keychain item for the 1Password token
	Version  string
}
