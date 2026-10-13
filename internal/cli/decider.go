package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aindeev/agent-chrome-relay/internal/decide"
	"github.com/aindeev/agent-chrome-relay/internal/macos"
)

// decider shows or sets what `chrome-relay do` asks when a step needs a decision:
//
//	chrome-relay decider                  which one runs, and why
//	chrome-relay decider jev              save a TypeSafe key (read from stdin) in the Keychain, after checking it works
//	chrome-relay decider jev forget       remove it
//	chrome-relay decider jev|claude on|off  use it or not (Claude: Haiku through your Claude Code)
func (c *cli) decider() error {
	cfg := c.config()
	dc := cfg.obj("decider")
	if dc == nil {
		dc = newObject()
	}
	which, what := c.arg(2), c.arg(3)
	switch {
	case which == "":
	case which == "jev" && what == "":
		key, err := readSecretLine("TypeSafe API key (https://typesafe.ai): ")
		if err != nil {
			return err
		}
		if !macos.Linked {
			return errors.New("this chrome-relay was built without Xcode's command line tools, so it has no Keychain access: set TYPESAFE_API_KEY instead")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		probe := &decide.Request{Body: map[string]any{"state": "A page with a Save button.", "questions": map[string]any{
			"ok": map[string]any{"type": "choice", "criteria": map[string]any{"save": "Click Save", "wait": "Wait"}, "instructions": "Next step?"}}}}
		if _, err := (&decide.Jev{Key: key}).Ask(ctx, probe); err != nil {
			return fmt.Errorf("not saved: %v", err)
		}
		if err := macos.KeychainSet(keychainService, jevAccount, []byte(key)); err != nil {
			return fmt.Errorf("Keychain: %v", err)
		}
		dc.del("jev")
		c.println("Saved: TypeSafe answered, and the key is in your login Keychain (only this signed chrome-relay reads it).")
	case which == "jev" && what == "forget":
		if macos.Linked {
			if err := macos.KeychainDelete(keychainService, jevAccount); err != nil {
				return err
			}
		}
		c.println("Removed the TypeSafe key from the Keychain.")
	case (which == "jev" || which == "claude") && (what == "on" || what == "off"):
		if what == "on" {
			dc.del(which)
		} else {
			dc.set(which, "off")
		}
	default:
		return fmt.Errorf("usage: %s decider [jev [forget|on|off] | claude on|off]", c.Name)
	}
	if which != "" {
		if len(dc.keys) == 0 {
			cfg.del("decider")
		} else {
			cfg.set("decider", dc)
		}
		if err := c.saveConfig(cfg); err != nil {
			return err
		}
	}
	c.printDecider()
	return nil
}

func (c *cli) printDecider() {
	_, names := c.deciders("auto")
	key, from := jevKey()
	cfg := c.config().obj("decider")
	off := func(k string) bool { return cfg != nil && cfg.str(k) == "off" }
	c.printf("`%s do` decides steps with: %s\n", c.Name, strings.Join(names, ", then "))
	switch {
	case off("jev"):
		c.printf("  jev     off (`%s decider jev on`)\n", c.Name)
	case key != "":
		c.printf("  jev     TypeSafe's Jev, key from the %s: ~150 ms a step; page text goes to TypeSafe (US)\n", from)
	default:
		c.printf("  jev     no key: `pbpaste | %s decider jev` (get one at https://typesafe.ai)\n", c.Name)
	}
	p := claudePath()
	switch {
	case off("claude"):
		c.printf("  claude  off (`%s decider claude on`)\n", c.Name)
	case p != "":
		c.printf("  claude  Haiku through your Claude Code (%s): ~1–1.5 s a step, on your Claude plan\n", p)
	default:
		c.printf("  claude  Claude Code not found on PATH\n")
	}
	if len(names) == 1 && names[0] == "none" {
		c.printf("Without a decider, `%s do` still fills what --data names and replays saved flows; any other step comes back to the agent.\n", c.Name)
	}
}

// readSecretLine reads one line from stdin without echoing it back (no prompt when piped).
func readSecretLine(prompt string) (string, error) {
	if fi, err := os.Stdin.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		fmt.Fprint(os.Stderr, prompt)
		defer fmt.Fprintln(os.Stderr)
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		if err != nil {
			return "", errors.New("no key on stdin (e.g. `pbpaste | chrome-relay decider jev`)")
		}
		return "", errors.New("empty key")
	}
	if strings.ContainsAny(line, " \t") {
		return "", errors.New("that doesn't look like a key (it has spaces)")
	}
	return line, nil
}

// flows lists or forgets the flows `chrome-relay do` saved:
//
//	chrome-relay flows                 every saved flow
//	chrome-relay flows forget <site>   one site's (or `all`)
func (c *cli) flows() error {
	f := decide.Flows{Dir: filepath.Join(c.Home, "flows")}
	switch c.arg(2) {
	case "":
	case "forget":
		site := c.arg(3)
		if site == "" {
			return fmt.Errorf("usage: %s flows forget <site>|all", c.Name)
		}
		if site == "all" {
			site = ""
		}
		if err := f.Forget(site); err != nil {
			return err
		}
		c.println("Forgotten.")
		return nil
	default:
		return fmt.Errorf("usage: %s flows [forget <site>|all]", c.Name)
	}
	all := f.List()
	if len(all) == 0 {
		c.printf("No saved flows yet: `%s do` saves one when a run ends with its --until checks met.\n", c.Name)
		return nil
	}
	for _, fl := range all {
		c.printf("%s  %q  %d steps, %d runs, saved %s\n", fl.Site, cut(fl.Goal, 80), len(fl.Steps), fl.Runs, fl.Saved.Format("2006-01-02"))
		for _, s := range fl.Steps {
			c.printf("    %s\n", s)
		}
	}
	return nil
}
