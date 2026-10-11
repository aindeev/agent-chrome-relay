package cli

import (
	"errors"
	"fmt"
	"os"

	"github.com/aindeev/agent-chrome-relay/internal/relay"
)

// Serve runs the relay until it fails. cmd/chrome-relay sets it to relay.Run.
var Serve = func(o relay.Options) error { return errors.New("relay not built yet") }

// ServeCmd is `chrome-relay serve [--dev]`, what the LaunchAgent runs. Same data folder and port as the CLI
// (CHROME_RELAY_HOME and CHROME_RELAY_PORT override them, as the LaunchAgent sets).
func ServeCmd(args []string, version string) int {
	dev, _ := devMode(args)
	dev = dev || os.Getenv("CHROME_RELAY_INSTANCE") == "dev-mode"
	in, err := thisInstance(dev)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := Serve(in.serveOptions(version)); err != nil {
		fmt.Fprintln(os.Stderr, "chrome-relay serve:", err)
		return 1
	}
	return 0
}

func (in instance) serveOptions(version string) relay.Options {
	return relay.Options{Home: in.Home, Port: in.Port, Instance: in.vaultInstance(), Version: version}
}
