// Command chrome-relay is Chrome Relay: the relay (`chrome-relay serve`, run by a LaunchAgent) and the CLI that
// sets it up and that agents use. Build it with ./build at the repo root: it must be signed to work.
package main

import (
	"os"

	"github.com/aindeev/agent-chrome-relay/internal/cli"
	"github.com/aindeev/agent-chrome-relay/internal/relay"
)

// version is set by ./build: -ldflags "-X main.version=<git describe>".
var version = "dev"

func main() {
	cli.Serve = relay.Run
	if command(os.Args) == "serve" {
		os.Exit(cli.ServeCmd(os.Args, version))
	}
	os.Exit(cli.Main(os.Args, version))
}

// command is the first argument that isn't --dev.
func command(args []string) string {
	for _, a := range args[1:] {
		if a != "--dev" {
			return a
		}
	}
	return ""
}
