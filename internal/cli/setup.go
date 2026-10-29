package cli

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/aindeev/agent-chrome-relay/internal/relay"
	"github.com/aindeev/agent-chrome-relay/internal/vault"
)

// extensionID is Chrome's ID for an unpacked extension: derived from the folder's absolute path.
func extensionID(folder string) string {
	if real, err := filepath.EvalSymlinks(folder); err == nil {
		folder = real
	}
	if abs, err := filepath.Abs(folder); err == nil {
		folder = abs
	}
	h := sha256.Sum256([]byte(folder))
	id := make([]byte, 32)
	for i, c := range hex.EncodeToString(h[:])[:32] {
		v := c - '0'
		if c >= 'a' {
			v = c - 'a' + 10
		}
		id[i] = byte('a' + v)
	}
	return string(id)
}

type chromeProfile struct{ Email, Dir string }

// chromeProfiles: email -> Chrome profile directory, from Chrome's own Local State (in Chrome's order). The
// error says why there is no list: macOS can keep Chrome's data folder from the terminal running setup
// ("operation not permitted"), and a silent empty list then made every `url <email>` "unknown profile".
func chromeProfiles(userHome string) ([]chromeProfile, error) {
	b, err := os.ReadFile(filepath.Join(userHome, "Library", "Application Support", "Google", "Chrome", "Local State"))
	if err != nil {
		return nil, err
	}
	v, err := parseJSON(b)
	if err != nil {
		return nil, err
	}
	root, ok := v.(*object)
	if !ok {
		return nil, fmt.Errorf("Local State is not a JSON object")
	}
	cache := root.obj("profile").obj("info_cache")
	var out []chromeProfile
	for _, dir := range cache.keys {
		if email := cache.obj(dir).str("user_name"); email != "" {
			out = append(out, chromeProfile{email, dir})
		}
	}
	return out, nil
}

// addProfile records email in identities' profiles unless it is there already (keeping its `use` and
// accounts); dir is set when known.
func addProfile(profiles *object, email, dir string) {
	entry, ok := profiles.get(email).(*object)
	if !ok {
		entry = newObject()
		entry.set("chromeProfile", dir)
		entry.set("use", "")
		profiles.set(email, entry)
	}
	if dir != "" {
		entry.set("chromeProfile", dir)
	}
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// plist is the LaunchAgent that keeps `chrome-relay serve` running from this checkout.
func (in instance) plist() string {
	args := "<string>" + xmlEscape(in.Bin) + "</string><string>serve</string>"
	env := "<key>CHROME_RELAY_HOME</key><string>" + xmlEscape(in.Home) + "</string><key>CHROME_RELAY_PORT</key><string>" + fmt.Sprint(in.Port) + "</string>"
	if in.Dev {
		args += "<string>--dev</string>"
		env += "<key>CHROME_RELAY_INSTANCE</key><string>dev-mode</string>"
	}
	out := xmlEscape(filepath.Join(in.Home, "relay.out"))
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>` + xmlEscape(in.Label) + `</string>
  <key>ProgramArguments</key><array>` + args + `</array>
  <key>WorkingDirectory</key><string>` + xmlEscape(in.Tool) + `</string>
  <key>EnvironmentVariables</key><dict>` + env + `</dict>
  <key>RunAtLoad</key><true/><key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>` + out + `</string><key>StandardErrorPath</key><string>` + out + `</string>
</dict></plist>
`
}

// runsThisCheckout: does a LaunchAgent plist run the relay from this checkout (this binary, or the Node relay
// an older Chrome Relay installed)?
func (in instance) runsThisCheckout(plist string) bool {
	return strings.Contains(plist, "<string>"+xmlEscape(in.Bin)+"</string>") ||
		strings.Contains(plist, "<string>"+xmlEscape(filepath.Join(in.Tool, "relay", "relay.mjs"))+"</string>")
}

func (c *cli) setup() error {
	if c.Dev {
		main := filepath.Join(c.UserHome, "Library", "LaunchAgents", "com.chrome-relay.plist")
		if b, err := os.ReadFile(main); err == nil && c.runsThisCheckout(string(b)) {
			return errors.New("this checkout already serves your normal Chrome Relay: dev mode needs a second one (e.g. a git worktree)")
		}
	}
	if err := os.MkdirAll(c.Home, 0o700); err != nil {
		return err
	}
	// which relay this checkout's extension dials
	if err := os.WriteFile(filepath.Join(c.Ext, "relay.json"), fmt.Appendf(nil, `{"port": %d}`, c.Port), 0o644); err != nil {
		return err
	}
	// only this checkout's extension may connect to the relay (it checks the connection's origin)
	cfg := c.config()
	ids := []string{extensionID(c.Ext)}
	if old, ok := cfg.get("extensionIds").([]any); ok {
		for _, x := range old {
			if s, ok := x.(string); ok {
				ids = append(ids, s)
			}
		}
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	cfg.set("extensionIds", ids)
	if err := c.saveConfig(cfg); err != nil {
		return err
	}
	if err := c.installNotifier(); err != nil {
		return err
	}
	tokPath := filepath.Join(c.Home, "token")
	if _, err := os.Stat(tokPath); errors.Is(err, fs.ErrNotExist) {
		secret := make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			return err
		}
		if err := os.WriteFile(tokPath, []byte(hex.EncodeToString(secret)), 0o600); err != nil {
			return err
		}
		c.println("created this Mac's secret:", tokPath)
	}
	if err := os.Chmod(tokPath, 0o600); err != nil {
		return err
	}
	// identities: keep what's there (hand-set rules, learned sites); add any new Chrome profiles
	d, err := c.ids()
	if err != nil {
		if _, statErr := os.Stat(filepath.Join(c.Home, "identities.json")); statErr == nil {
			return err // there but unreadable: never overwrite it
		}
		d = newObject()
		d.set("profiles", newObject())
		sites := newObject()
		sites.set("rules", newObject())
		sites.set("learned", newObject())
		d.set("sites", sites)
	}
	profiles, ok := d.get("profiles").(*object)
	if !ok {
		profiles = newObject()
		d.set("profiles", profiles)
	}
	found, readErr := chromeProfiles(c.UserHome)
	for _, p := range found {
		addProfile(profiles, p.Email, p.Dir)
	}
	// Profiles whose extension is connected now count too: they are the ones agents can use, and they are
	// all there is when Chrome's own list could not be read.
	if c.up() {
		if s, err := c.relayStatus(); err == nil {
			for _, email := range s.Profiles {
				addProfile(profiles, email, "")
			}
		}
	}
	if readErr != nil {
		c.printf("identities: could not read Chrome's profile list (%v); macOS may be keeping Chrome's data from this terminal. "+
			"Profiles whose extension is connected are recorded; run `%s setup` again once the rest are open in Chrome\n", readErr, c.Name)
	}
	if err := writeJSON(filepath.Join(c.Home, "identities.json"), d); err != nil {
		return err
	}
	c.printf("identities: %d Chrome profiles in %s/identities.json (edit `use` and `sites.rules` as you like)\n", len(profiles.keys), c.Home)
	// background service (this binary, `serve`), CLI on PATH, Claude Code skill
	if err := os.MkdirAll(filepath.Dir(c.Plist), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(c.Plist, []byte(c.plist()), 0o644); err != nil {
		return err
	}
	c.launchctl("bootout", c.target())
	if err := c.bootstrap(); err != nil {
		return err
	}
	bindir := filepath.Join(c.UserHome, ".local", "bin")
	if err := os.MkdirAll(bindir, 0o755); err != nil {
		return err
	}
	link := filepath.Join(bindir, c.Name)
	if real, err := filepath.EvalSymlinks(link); err != nil || real != c.Bin {
		if _, err := os.Lstat(link); err == nil {
			if err := os.Remove(link); err != nil {
				return err
			}
		}
		if err := os.Symlink(c.Bin, link); err != nil {
			return err
		}
	}
	if c.SkillLink != "" {
		if err := os.MkdirAll(filepath.Dir(c.SkillLink), 0o755); err != nil {
			return err
		}
		if _, err := os.Lstat(c.SkillLink); err == nil {
			if err := os.Remove(c.SkillLink); err != nil {
				return err
			}
		}
		if err := os.Symlink(filepath.Join(c.Tool, "skill"), c.SkillLink); err != nil {
			return err
		}
	}
	msg := fmt.Sprintf("background service %s started on port %d; `%s` linked into %s", c.Label, c.Port, c.Name, bindir)
	if c.SkillLink != "" {
		msg += "; Claude Code skill linked at " + c.SkillLink
	}
	c.println(msg)
	if c.Dev {
		c.printf("Dev mode: agents use `%s url <email>`; this checkout's extension is a second Chrome Relay (its icon's tooltip says dev mode).\n", c.Name)
	}
	soon := ""
	if !c.Dev && !relay.WebStoreLive {
		soon = " (a Chrome Web Store version is in review: coming soon)"
	}
	install := `  1. open chrome://extensions in a window of that profile
  2. turn on Developer mode (top right)
  3. Load unpacked -> ` + c.Ext + `
  4.`
	if !c.Dev && relay.WebStoreLive {
		install = `  1. install Chrome Relay from the Chrome Web Store: ` + relay.WebStoreURL() + `
     (or this checkout's own copy instead, never both: chrome://extensions -> Developer mode -> Load unpacked
     -> ` + c.Ext + `)
  2.`
	}
	c.printf(`
Last step, by hand, in EACH Chrome profile agents should use (nothing installs it for you)%s:
%s pin it: the puzzle-piece icon in Chrome's toolbar, then the pin next to Chrome Relay (its badge shows when
     an agent needs you)
Then run `+"`%s doctor`, and `%s notify test` to allow Chrome Relay's notifications"+".\n", soon, install, c.Name, c.Name)
	c.setupDecider()
	return nil
}

// setupDecider: `chrome-relay do` decides steps with Jev if the person gives it a key; without one, with Haiku
// through their own Claude Code. Setup only says which, and how to change it: it never asks for a key.
func (c *cli) setupDecider() {
	if k, _ := jevKey(); k != "" {
		c.printf("\n`%s do` (agents carry out a whole goal in one command) decides steps with TypeSafe's Jev.\n", c.Name)
		return
	}
	c.printf("\nOptional: `%s do` lets agents carry out a whole goal in one command. For steps that need a decision it uses\n", c.Name)
	if p := claudePath(); p != "" {
		c.printf("Haiku through your Claude Code (%s), ~1 s a step. Faster (~0.15 s) with a TypeSafe key for Jev:\n", p)
	} else {
		c.printf("TypeSafe's Jev (~0.15 s a step), or Haiku through Claude Code once it's installed. For Jev, a TypeSafe key:\n")
	}
	c.printf("  pbpaste | %s decider jev      (stored in your Keychain; page text of agent tabs then goes to TypeSafe)\n", c.Name)
}

// bootstrap loads the LaunchAgent. Right after a bootout launchd can still be tearing the old one down, so retry
// for a moment before giving up.
func (c *cli) bootstrap() error {
	var out []byte
	var err error
	for range 10 {
		if out, err = c.launchctl("bootstrap", fmt.Sprintf("gui/%d", os.Getuid()), c.Plist); err == nil {
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("launchctl bootstrap %s: %v %s", c.Plist, err, strings.TrimSpace(string(out)))
}

func (c *cli) uninstall() error {
	c.launchctl("bootout", c.target())
	for _, p := range []string{c.Plist, filepath.Join(c.UserHome, ".local", "bin", c.Name), c.SkillLink} {
		if p == "" {
			continue
		}
		if _, err := os.Lstat(p); err == nil {
			if err := os.Remove(p); err != nil {
				return err
			}
		}
	}
	if !c.Dev {
		os.RemoveAll(c.notifierApp())
	}
	c.printf("removed the service, CLI link and skill; %s kept. Remove the extension from chrome://extensions in each profile.\n", c.Home)
	if slices.ContainsFunc(append(serviceKeys(configuredSources(c.config())), ""), func(k string) bool { return vault.HasToken(c.vaultInstance(), k) }) {
		c.printf("The 1Password token is still in your Keychain: `%s vault forget` removes it.\n", c.Name)
	}
	return nil
}
