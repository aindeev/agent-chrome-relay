package cli

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Chrome Relay.app (notifier/main.swift, built by ./build): macOS notifications in Chrome Relay's own name when an
// agent needs the person. One copy for every instance, in ~/Applications, where macOS keeps its permission.

func (in instance) notifierApp() string {
	return filepath.Join(in.UserHome, "Applications", "Chrome Relay.app")
}

// installedNotifier: the app's path if it is there, else "".
func (in instance) installedNotifier() string {
	if _, err := os.Stat(filepath.Join(in.notifierApp(), "Contents", "MacOS", "Chrome Relay")); err != nil {
		return ""
	}
	return in.notifierApp()
}

// installNotifier copies this checkout's build of the app into ~/Applications. A core-only build has none.
func (c *cli) installNotifier() error {
	built := filepath.Join(c.Tool, "Chrome Relay.app")
	if _, err := os.Stat(built); err != nil || c.Dev {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(c.notifierApp()), 0o755); err != nil {
		return err
	}
	if err := os.RemoveAll(c.notifierApp()); err != nil {
		return err
	}
	if out, err := exec.Command("ditto", built, c.notifierApp()).CombinedOutput(); err != nil {
		return fmt.Errorf("installing Chrome Relay.app: %v %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// notifierStatus: "authorized", "denied" or "not asked", as macOS has it for Chrome Relay.app.
func (c *cli) notifierStatus() string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, filepath.Join(c.notifierApp(), "Contents", "MacOS", "Chrome Relay"), "status").Output()
	return strings.TrimSpace(string(out))
}

// chrome-relay notify        whether Chrome Relay's notifications reach you
// chrome-relay notify test   send one (the first time, macOS asks you to allow them)
func (c *cli) notify() error {
	if c.installedNotifier() == "" {
		return fmt.Errorf("Chrome Relay.app is not installed: run `./build` in %s (it needs Xcode's command line tools), then `%s update`", c.Tool, c.Name)
	}
	if c.arg(2) == "test" {
		if out, err := exec.Command("open", "-g", "-n", c.notifierApp(), "--args", "notify", "test", "Chrome Relay", "This is how you'll hear that an agent needs you.").CombinedOutput(); err != nil {
			return fmt.Errorf("%v %s", err, strings.TrimSpace(string(out)))
		}
		c.println("sent. If macOS asks, allow notifications from Chrome Relay. Nothing on screen? Check System Settings > Notifications >")
		c.println("Chrome Relay, and add Chrome Relay to the apps your Focus (Do Not Disturb) lets through.")
		return nil
	}
	switch st := c.notifierStatus(); st {
	case "authorized":
		c.println("on: Chrome Relay notifies you when an agent is stuck on something only you can do (a passkey to approve in 1Password).")
		c.printf("`%s notify test` sends one. A Focus (Do Not Disturb) hides them unless you let Chrome Relay through.\n", c.Name)
	case "denied":
		c.println("off: turn on System Settings > Notifications > Chrome Relay.")
	default:
		c.printf("not allowed yet: run `%s notify test` and allow notifications when macOS asks.\n", c.Name)
	}
	return nil
}

// attend: Chrome Relay.app runs this when the person clicks a notification; the tab that needs them comes forward.
func (c *cli) attend() error {
	if c.arg(3) == "" {
		return errors.New("usage: chrome-relay attend <profile> <tab>")
	}
	var r struct{}
	return c.get("/attend?profile="+url.QueryEscape(c.arg(2))+"&tab="+url.QueryEscape(c.arg(3)), 3*time.Second, &r)
}
