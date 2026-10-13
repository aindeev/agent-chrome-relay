package cli

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aindeev/agent-chrome-relay/internal/macos"
	"github.com/aindeev/agent-chrome-relay/internal/relay"
	"github.com/aindeev/agent-chrome-relay/internal/vault"
)

func (c *cli) doctor() error {
	ok := true
	check := func(cond bool, good, fix string) {
		ok = ok && cond
		if cond {
			c.println("  ok   " + good)
		} else {
			c.println("  FIX  " + fix)
		}
	}

	tok := filepath.Join(c.Home, "token")
	st, err := os.Stat(tok)
	check(err == nil && st.Mode().Perm() == 0o600, "secret present and private", fmt.Sprintf("secret missing or readable by others: run `%s setup`", c.Name))
	_, err = c.launchctl("print", c.target())
	loaded := err == nil
	plist, _ := os.ReadFile(c.Plist)
	switch {
	case !loaded:
		check(false, "", fmt.Sprintf("service not loaded: run `%s setup`", c.Name))
	case !strings.Contains(string(plist), "<string>"+xmlEscape(c.Bin)+"</string>"):
		check(false, "", fmt.Sprintf("the background service does not run %s (an older Chrome Relay?): run `%s setup`", c.Bin, c.Name))
	default:
		check(true, "background service installed", "")
	}
	allowed := c.config().get("extensionIds")
	id := extensionID(c.Ext)
	check(slices.ContainsFunc(asList(allowed), func(x any) bool { return x == id }), fmt.Sprintf("extension %s allowed to connect", id), fmt.Sprintf("this checkout's extension is not allowed yet: run `%s setup`", c.Name))
	check(c.up(), fmt.Sprintf("relay answering on 127.0.0.1:%d", c.Port), fmt.Sprintf("relay down: `%s update`; details in %s/relay.out", c.Name, c.Home))
	check(exec.Command("pgrep", "-x", "Google Chrome").Run() == nil, "Chrome running", "Chrome is not running: open it")
	if c.up() {
		s, _ := c.relayStatus()
		check(len(s.Profiles) > 0, "extension connected in: "+strings.Join(s.Profiles, ", "), "extension not connected in any profile: "+c.installExtension())
		disk := manifestVersion(c.Ext)
		var vers map[string]struct {
			Version  string
			WebStore bool
		}
		if body, err := c.getRaw("/versions", 5*time.Second); err == nil {
			json.Unmarshal(body, &vers)
		}
		for _, e := range slices.Sorted(maps.Keys(vers)) {
			v := vers[e]
			switch {
			case !v.WebStore:
				check(v.Version == disk, fmt.Sprintf("%s runs the current extension %s", e, disk), fmt.Sprintf("%s runs %s, files are %s: run `%s update`", e, v.Version, disk, c.Name))
			case compareVersions(v.Version, disk) > 0: // the store has a release this checkout doesn't
				check(false, "", fmt.Sprintf("%s runs the Web Store's extension %s, newer than this checkout (%s): `git pull`, `./build`, then `%s update`", e, v.Version, disk, c.Name))
			default: // the store updates it on its own, a few hours after a release
				check(true, fmt.Sprintf("%s runs the Web Store's extension %s", e, v.Version), "")
			}
		}
	}
	// passkeys and the 1Password token live in the Secure Enclave and the Keychain, trusted only by this signed binary
	out, _ := exec.Command("codesign", "-dv", "--verbose=2", c.Bin).CombinedOutput()
	sig := parseCodesign(string(out))
	rebuild := fmt.Sprintf("run `./build` in %s, then `%s update`", c.Tool, c.Name)
	switch {
	case sig.Authority == "" && !sig.Adhoc:
		check(false, "", fmt.Sprintf("%s is not signed: %s", c.Bin, rebuild))
	case !sig.Runtime:
		check(false, "", fmt.Sprintf("%s is signed without the hardened runtime: %s", c.Bin, rebuild))
	case sig.Adhoc:
		check(true, "signed ad-hoc, with the hardened runtime (macOS may ask once after each rebuild before Chrome Relay reads its Keychain item; a code signing identity avoids that: ./build --help)", "")
	default:
		check(true, fmt.Sprintf("signed by %s, with the hardened runtime", sig.Authority), "")
	}
	// how the person hears that an agent needs them
	if c.installedNotifier() == "" {
		c.printf("  --   no Chrome Relay.app, so no notifications when an agent needs you (optional: ./build with Xcode's command line tools, then `%s update`)\n", c.Name)
	} else {
		st := c.notifierStatus()
		check(st == "authorized", "notifications allowed for Chrome Relay (a Focus hides them unless you let Chrome Relay through)",
			fmt.Sprintf("Chrome Relay's notifications are %s: run `%s notify test` and allow them (System Settings > Notifications > Chrome Relay)", cmpOr(st, "unknown"), c.Name))
	}
	sources := configuredSources(c.config())
	switch {
	case !macos.Linked && len(sources) > 0:
		check(false, "", fmt.Sprintf("1Password is set up, but this chrome-relay was built without Xcode's command line tools: `xcode-select --install`, then %s; or `%s vault forget`", rebuild, c.Name))
	case !macos.Linked:
		c.println("  --   core build: no 1Password or Secure Enclave passkeys (optional: `xcode-select --install`, then ./build again)")
	case len(sources) == 0:
		c.printf("  --   1Password: not set up (optional: `%s vault setup` lets agents sign in with your 1Password)\n", c.Name)
	}
	for _, s := range sources {
		check(vault.HasToken(c.vaultInstance(), s.Key), fmt.Sprintf("1Password %s: vault %s, token in the Keychain (locked to this Mac)", s.label(), quoteList(s.Vaults)),
			fmt.Sprintf("no token in the Keychain for the %s: run `%s vault setup %q`", s.label(), c.Name, s.Vaults[0]))
	}
	if n := appAccounts(c.config()); n > 0 {
		check(false, "", fmt.Sprintf("config.json still has %d 1Password app connection(s) from an earlier setup, which Chrome Relay no longer uses (they can't work in the background): run `%s vault setup` for service accounts", n, c.Name))
	}
	if len(sources) > 0 && c.up() {
		c.println("  ..   asking the relay for the vaults")
		var d vaultSummary
		if err := c.get("/vault", vaultTimeout, &d); err != nil {
			d.Error = err.Error()
		}
		vaults := d.Vaults
		if len(vaults) == 0 && d.Vault != "" {
			vaults = []string{d.Vault}
		}
		check(d.Error == "", fmt.Sprintf("1Password vaults %s: %d items", quoteList(vaults), len(d.Items)), "1Password: "+d.Error)
	}
	// what `chrome-relay do` asks when a step needs a decision (optional: without one it fills data and replays flows)
	_, names := c.deciders("auto")
	switch names[0] {
	case "jev":
		c.printf("  ok   `%s do` decides with TypeSafe's Jev%s\n", c.Name, map[bool]string{true: ", then Haiku through Claude Code", false: ""}[len(names) > 1])
	case "claude":
		c.printf("  ok   `%s do` decides with Haiku through your Claude Code (faster with a Jev key: `pbpaste | %s decider jev`)\n", c.Name, c.Name)
	default:
		c.printf("  --   `%s do` has no decider: it fills what agents name and replays flows, and hands other steps back (`%s decider`)\n", c.Name, c.Name)
	}
	if ok {
		c.println("all good")
	} else {
		c.println("fix the items above")
	}
	return nil
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

// installExtension: how to install the extension in a profile, for messages.
func (c *cli) installExtension() string {
	unpacked := "in chrome://extensions turn on Developer mode, then Load unpacked -> " + c.Ext
	if c.Dev {
		return unpacked
	}
	return "install it from " + relay.WebStoreURL() + " (or, for this checkout's own copy: " + unpacked + "; not both)"
}

// compareVersions compares dotted versions (1.10.0 > 1.9.4): -1, 0 or 1.
func compareVersions(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := range max(len(as), len(bs)) {
		var x, y int
		if i < len(as) {
			x, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			y, _ = strconv.Atoi(bs[i])
		}
		if x != y {
			return cmp.Compare(x, y)
		}
	}
	return 0
}

func manifestVersion(ext string) string {
	var m struct {
		Version string `json:"version"`
	}
	b, _ := os.ReadFile(filepath.Join(ext, "manifest.json"))
	json.Unmarshal(b, &m)
	return m.Version
}

type signature struct {
	Authority string // the signing identity (the first Authority= line: the leaf certificate)
	Runtime   bool   // hardened runtime
	Adhoc     bool
}

var codesignFlags = regexp.MustCompile(`(?m)flags=0x[0-9a-fA-F]+\(([^)]*)\)`)

// parseCodesign reads `codesign -dv --verbose=2` output.
func parseCodesign(out string) signature {
	var s signature
	for _, l := range strings.Split(out, "\n") {
		if a, ok := strings.CutPrefix(l, "Authority="); ok && s.Authority == "" {
			s.Authority = strings.TrimSpace(a)
		}
		if strings.TrimSpace(l) == "Signature=adhoc" {
			s.Adhoc = true
		}
	}
	if m := codesignFlags.FindStringSubmatch(out); m != nil {
		for _, f := range strings.Split(m[1], ",") {
			switch f {
			case "runtime":
				s.Runtime = true
			case "adhoc":
				s.Adhoc = true
			}
		}
	}
	return s
}
