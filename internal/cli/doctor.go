package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/aindeev/agent-chrome-relay/internal/macos"
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
		check(len(s.Profiles) > 0, "extension connected in: "+strings.Join(s.Profiles, ", "), fmt.Sprintf("extension not connected in any profile: load it unpacked from %s (see `%s setup` output)", c.Ext, c.Name))
		disk := manifestVersion(c.Ext)
		vers := newObject()
		if body, err := c.getRaw("/versions", 5*time.Second); err == nil {
			if v, err := parseJSON(body); err == nil {
				if o, ok := v.(*object); ok {
					vers = o
				}
			}
		}
		for _, e := range vers.keys {
			v := pyStr(vers.get(e))
			check(v == disk, fmt.Sprintf("%s runs the current extension %s", e, disk), fmt.Sprintf("%s runs %s, files are %s: run `%s update`", e, v, disk, c.Name))
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
	sources := configuredSources(c.config())
	switch {
	case !macos.Linked && len(sources) > 0:
		check(false, "", fmt.Sprintf("1Password is set up, but this chrome-relay was built without Xcode's command line tools: `xcode-select --install`, then %s; or `%s vault forget`", rebuild, c.Name))
	case !macos.Linked:
		c.println("  --   core build: no 1Password or Secure Enclave passkeys (optional: `xcode-select --install`, then ./build again)")
	case len(sources) == 0:
		c.printf("  --   1Password: not set up (optional: `%s vault setup` lets agents sign in with your 1Password)\n", c.Name)
	}
	desktop := false
	for _, s := range sources {
		if s.Account == "" {
			check(vault.HasToken(c.vaultInstance()), fmt.Sprintf("1Password service account for %s: token in the Keychain (locked to this Mac)", quoteList(s.Vaults)),
				fmt.Sprintf("no 1Password service account token in the Keychain: run `%s vault setup %s`", c.Name, s.Vaults[0]))
			continue
		}
		desktop = true
		check(true, fmt.Sprintf("1Password account %s: vaults %s", s.label(), strings.Join(s.Vaults, ", ")), "")
	}
	if desktop { // the relay goes through the 1Password app, and a library it ships
		app := onePasswordApp(c.UserHome)
		check(app != "", "1Password app at "+app, "1Password app not found in /Applications: install it from 1password.com/downloads")
		if app != "" {
			_, err := os.Stat(filepath.Join(app, "Contents", "Frameworks", "libop_sdk_ipc_client.dylib"))
			check(err == nil, "1Password app lets other apps connect (Settings > Developer > \"Integrate with other apps\" must be on)",
				"this 1Password app can't connect other apps: update 1Password, and turn on Settings > Developer > \"Integrate with other apps\"")
		}
	}
	if len(sources) > 0 && c.up() {
		c.println("  ..   asking the relay for the vaults (approve in 1Password or with Touch ID if asked)")
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
	if ok {
		c.println("all good")
	} else {
		c.println("fix the items above")
	}
	return nil
}

// onePasswordApp: where the 1Password app is installed, or "".
func onePasswordApp(userHome string) string {
	for _, p := range []string{"/Applications/1Password.app", filepath.Join(userHome, "Applications", "1Password.app")} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
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
