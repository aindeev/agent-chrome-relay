// Package cli is the chrome-relay command: setup, the agent-facing commands (url, identities, vault, status) and
// the person's tools (doctor, update, audit, logs, uninstall). `chrome-relay serve` runs the relay itself.
package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aindeev/agent-chrome-relay/internal/decide"
)

const usage = `Chrome Relay: your coding agents drive background tabs in your everyday Chrome.

  ./build && ./chrome-relay setup  one-time setup, in your checkout: build and sign, then secret, background service,
                                   identities, skill; safe to re-run
  chrome-relay setup --dev         from a second checkout (a git worktree): install it beside your normal one as
                                   ` + "`chrome-relay-dev-mode`" + `, with its own relay (port 9334), data and extension
  chrome-relay url [email]         CDP URL for agent-browser, including this Mac's secret
                                   agent-browser --cdp "$(chrome-relay url you@company.com)" open <url>
  chrome-relay identities          Chrome profiles, their Google accounts, and which account each site uses
  chrome-relay vault               the 1Password vaults agents sign in with: items, websites, what each holds (never values)
  chrome-relay vault setup         let agents sign in with 1Password: asks which accounts, then creates a shared vault and a
                                   service account in each (token straight into the Keychain, never shown)
  chrome-relay vault setup <vault> [--account <email>] one account, no questions (--paste: a token you made yourself)
  chrome-relay vault approve <m>   how often Chrome Relay asks before agents sign in: session, sign-in or off (asks you first)
  chrome-relay vault forget        stop: remove 1Password from the config, and any service account token from the Keychain
  chrome-relay passkeys [on|off]   passkeys in agent tabs (on by default): Chrome Relay's own live in the Secure Enclave
  chrome-relay passkeys reuse <s>  let one approval cover an agent's passkey sign-ins for s seconds (max 300; off = every time)
  chrome-relay phone [pair|test]   let a paired iPhone approve agents' requests with Face ID when you're away
  chrome-relay phone push --key <AuthKey_KEYID.p8> --team-id <TEAMID>  notify it through Apple push with your own
                                   APNs key, kept in the Keychain (` + "`phone push forget`" + `: back to the push relay)
  chrome-relay notify [test]       macOS notifications when an agent needs you (a passkey only you can approve)
  chrome-relay do "<goal>" [--until <check>] [--data key=value]  carry out a goal in your agent tab: fills what
                                   --data names, replays what worked before, asks a fast decider for the rest, and
                                   hands back what it isn't sure of (` + "`chrome-relay do --help`" + `)
  chrome-relay decider [jev|claude ...]  what ` + "`do`" + ` asks: TypeSafe's Jev (` + "`pbpaste | chrome-relay decider jev`" + `),
                                   else Haiku through your Claude Code
  chrome-relay flows [forget <site>]  the flows ` + "`do`" + ` saved, replayed without a model
  chrome-relay status              connected profiles and agents
  chrome-relay doctor              check everything and say how to fix what's wrong
  chrome-relay update [--force]    after ` + "`git pull` and `./build`" + `: restart the relay and reload the extension everywhere (waits for idle agents unless --force)
  chrome-relay audit [N] [filter]  last N audit events (default 40), optionally only lines containing filter
  chrome-relay logs [N]            last N relay log lines
  chrome-relay uninstall           stop and remove the background service, CLI link and skill (keeps ~/.chrome-relay)
  chrome-relay serve               the relay itself (the background service runs it; you don't need to)
`

const devName = "chrome-relay-dev-mode"

// instance is one install: the normal one, or dev mode beside it. Dev mode is a second install of another
// checkout, run as `chrome-relay-dev-mode` (the link `setup --dev` makes) or with --dev; everything it owns is
// separate: port, data folder, service, Keychain item.
type instance struct {
	Dev       bool
	Name      string // chrome-relay or chrome-relay-dev-mode: the command, and how hints name it
	Home      string // data folder: token, config.json, identities.json, relay.log, audit/
	Port      int    // the extension dials 9333, or the port in extension/relay.json (written by setup)
	Label     string // the LaunchAgent
	Plist     string
	SkillLink string // "" in dev mode: dev mode leaves your agents' skill alone
	UserHome  string
	Bin       string // this binary, symlinks resolved
	Tool      string // the checkout: the directory holding the binary
	Ext       string // the checkout's extension
}

// devMode: the program's name or a --dev anywhere; returns the arguments without --dev.
func devMode(args []string) (bool, []string) {
	dev := len(args) > 0 && filepath.Base(args[0]) == devName
	rest := make([]string, 0, len(args))
	for _, a := range args {
		if a == "--dev" {
			dev = true
			continue
		}
		rest = append(rest, a)
	}
	return dev, rest
}

func newInstance(dev bool, getenv func(string) string, userHome, bin string) instance {
	in := instance{Dev: dev, Name: "chrome-relay", Port: 9333, UserHome: userHome, Bin: bin}
	if dev {
		in.Name, in.Port = devName, 9334
	}
	in.Home = filepath.Join(userHome, "."+in.Name)
	if h := getenv("CHROME_RELAY_HOME"); h != "" {
		in.Home = h
	}
	if p, err := strconv.Atoi(getenv("CHROME_RELAY_PORT")); err == nil && p > 0 {
		in.Port = p
	}
	in.Label = "com." + in.Name
	in.Plist = filepath.Join(userHome, "Library", "LaunchAgents", in.Label+".plist")
	if !dev {
		in.SkillLink = filepath.Join(userHome, ".claude", "skills", "chrome-relay")
	}
	in.Tool = filepath.Dir(bin)
	in.Ext = filepath.Join(in.Tool, "extension")
	return in
}

// vaultInstance picks this install's Keychain item for the 1Password token.
func (in instance) vaultInstance() string {
	if in.Dev {
		return "dev-mode"
	}
	return ""
}

// thisInstance: the running binary (the checkout root is where it lives), the person's home, the environment.
func thisInstance(dev bool) (instance, error) {
	exe, err := os.Executable()
	if err != nil {
		return instance{}, err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return instance{}, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return instance{}, err
	}
	return newInstance(dev, os.Getenv, home, exe), nil
}

type cli struct {
	instance
	args    []string // os.Args without --dev
	version string
	out     io.Writer
	errw    io.Writer
	op      func(args ...string) (stdout, stderr []byte, err error) // tests: the 1Password CLI
	opAccts func() []opAccount                                      // tests: its accounts
	models  func(choice string) ([]decide.Model, []string)          // tests: the deciders `do` asks
}

// Main runs every command but serve; returns the exit code.
func Main(args []string, version string) int {
	dev, rest := devMode(args)
	in, err := thisInstance(dev)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	c := &cli{instance: in, args: rest, version: version, out: os.Stdout, errw: os.Stderr}
	if err := c.run(); err != nil {
		var ec exitCode
		if errors.As(err, &ec) {
			if ec.msg != "" {
				fmt.Fprintln(os.Stderr, ec.msg)
			}
			return ec.code
		}
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func (c *cli) arg(i int) string {
	if len(c.args) > i {
		return c.args[i]
	}
	return ""
}

func (c *cli) run() error {
	switch cmd := c.arg(1); cmd {
	case "setup":
		return c.setup()
	case "url":
		return c.url()
	case "status":
		return c.status()
	case "do":
		return c.do()
	case "open", "snapshot", "click", "dblclick", "fill", "type", "select", "check", "uncheck", "hover", "upload", "dialog", "drag", "console", "errors", "cookies", "pdf", "cdp", "is", "press", "scroll", "back", "reload", "get", "eval",
		"screenshot", "wait", "tabs", "tab", "close":
		return c.drive(cmd)
	case "decider":
		return c.decider()
	case "flows":
		return c.flows()
	case "identities":
		return c.identities()
	case "vault":
		return c.vault()
	case "passkeys":
		return c.passkeys()
	case "phone":
		return c.phone()
	case "notify":
		return c.notify()
	case "attend":
		return c.attend()
	case "doctor":
		return c.doctor()
	case "update":
		return c.update()
	case "audit":
		return c.audit()
	case "logs":
		return c.logs()
	case "uninstall":
		return c.uninstall()
	default:
		fmt.Fprint(c.out, usage+"\n") // a blank line after, as Python's print(__doc__)
		return nil
	}
}

func (c *cli) printf(format string, a ...any) { fmt.Fprintf(c.out, format, a...) }
func (c *cli) println(a ...any)               { fmt.Fprintln(c.out, a...) }

// --- talking to the relay: http://127.0.0.1:PORT/...?token=<this Mac's secret> ---

func (c *cli) token() (string, error) {
	b, err := os.ReadFile(filepath.Join(c.Home, "token"))
	if err != nil {
		return "", fmt.Errorf("no secret in %s: run `%s setup`", c.Home, c.Name)
	}
	return strings.TrimSpace(string(b)), nil
}

func (c *cli) getRaw(path string, timeout time.Duration) ([]byte, error) {
	tok, err := c.token()
	if err != nil {
		return nil, err
	}
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	client := &http.Client{Timeout: timeout}
	res, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d%s%stoken=%s", c.Port, path, sep, tok))
	if err != nil {
		// never print the URL: it carries this Mac's secret
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		if strings.Contains(err.Error(), "Timeout") || strings.Contains(err.Error(), "deadline") {
			return nil, fmt.Errorf("the relay did not answer %s within %s", path, timeout)
		}
		return nil, fmt.Errorf("the relay did not answer %s: %s", path, strings.ReplaceAll(err.Error(), tok, "<secret>"))
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP Error %d: %s", res.StatusCode, http.StatusText(res.StatusCode))
	}
	return body, nil
}

func (c *cli) get(path string, timeout time.Duration, v any) error {
	body, err := c.getRaw(path, timeout)
	if err != nil {
		return err
	}
	return json.Unmarshal(body, v)
}

type relayStatus struct {
	Profiles []string          `json:"profiles"`
	Agents   []json.RawMessage `json:"agents"`
}

func (c *cli) relayStatus() (relayStatus, error) {
	var s relayStatus
	err := c.get("/status", 3*time.Second, &s)
	return s, err
}

func (c *cli) up() bool {
	var s relayStatus
	return c.get("/status", time.Second, &s) == nil
}

func (c *cli) launchctl(args ...string) ([]byte, error) {
	return exec.Command("launchctl", args...).CombinedOutput()
}

func (c *cli) target() string { return fmt.Sprintf("gui/%d/%s", os.Getuid(), c.Label) }

func (c *cli) kickstart(restart bool) {
	args := []string{"kickstart"}
	if restart {
		args = append(args, "-k")
	}
	c.launchctl(append(args, c.target())...)
}

func (c *cli) ensureUp() error {
	if c.up() {
		return nil
	}
	c.kickstart(false)
	for range 20 {
		if c.up() {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("Chrome Relay is not running: run `%s doctor`", c.Name)
}

// --- this install's files ---

func (c *cli) configPath() string { return filepath.Join(c.Home, "config.json") }

// config is HOME/config.json, keys in file order; empty when missing or unreadable.
func (c *cli) config() *object {
	if b, err := os.ReadFile(c.configPath()); err == nil {
		if v, err := parseJSON(b); err == nil {
			if o, ok := v.(*object); ok {
				return o
			}
		}
	}
	return newObject()
}

func (c *cli) saveConfig(cfg *object) error { return writeJSON(c.configPath(), cfg) }

func (c *cli) ids() (*object, error) {
	b, err := os.ReadFile(filepath.Join(c.Home, "identities.json"))
	if err != nil {
		return nil, fmt.Errorf("no identities yet (%v): run `%s setup`", err, c.Name)
	}
	v, err := parseJSON(b)
	if err != nil {
		return nil, fmt.Errorf("%s/identities.json: %v", c.Home, err)
	}
	o, ok := v.(*object)
	if !ok {
		return nil, fmt.Errorf("%s/identities.json is not a JSON object", c.Home)
	}
	return o, nil
}

func hasFlag(args []string, flag string) bool { return slices.Contains(args, flag) }

var errUsage = errors.New(usage)
