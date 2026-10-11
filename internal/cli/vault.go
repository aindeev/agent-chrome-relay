package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/aindeev/agent-chrome-relay/internal/macos"
	"github.com/aindeev/agent-chrome-relay/internal/vault"
)

// errCoreOnly: 1Password needs the Keychain and Touch ID, which the core-only build (no Xcode tools) leaves out.
var errCoreOnly = errors.New("this chrome-relay was built without Xcode's command line tools, so it has no 1Password: " +
	"`xcode-select --install`, then ./build again")

// Opening the vaults may wait for the person: 1Password's own approval, or Touch ID, in the relay.
const vaultTimeout = 120 * time.Second

var opsToken = regexp.MustCompile(`^ops_[A-Za-z0-9_\-.=+/]+$`)

func (c *cli) vault() error {
	switch sub := c.arg(2); {
	case sub == "setup":
		if !macos.Linked {
			return errCoreOnly
		}
		a, err := parseSetupArgs(c.args[min(3, len(c.args)):])
		if err != nil {
			return fmt.Errorf("%v\nusage: %s vault setup                        your 1Password app (asks which accounts and vaults)\n"+
				"       %s vault setup --account <name or uuid> --vaults \"A,B\" [--account ... --vaults ...] [--approve session|sign-in|off]\n"+
				"       %s vault setup <vault>                a service account for one shared vault", err, c.Name, c.Name, c.Name)
		}
		switch {
		case a.service != "":
			return c.vaultSetup(a.service)
		case len(a.sources) > 0:
			return c.desktopSetupFlags(a, c.desktopEnv())
		}
		ask := !a.desktop && term.IsTerminal(int(os.Stdin.Fd()))
		return c.desktopSetup(newPrompter(os.Stdin, c.errw), c.desktopEnv(), ask)
	case sub == "forget":
		err := vault.ForgetToken(c.vaultInstance())
		c.forgetLegacyToken()
		cfg := c.config()
		cfg.del("onepassword")
		if serr := c.saveConfig(cfg); serr != nil {
			return serr
		}
		if err != nil {
			return fmt.Errorf("removed 1Password from the config, but not the service account token from the Keychain: %v", err)
		}
		c.println("removed 1Password from Chrome Relay (the vaults from the config, any service account token from the Keychain); agents can no longer use it")
		return nil
	case sub != "":
		return errUsage
	case !truthy(c.config().get("onepassword")):
		return fmt.Errorf("no 1Password vaults for agents yet: the person at this Mac can run `%s vault setup`", c.Name)
	}
	return c.vaultList()
}

// truthy is Python's bool() of a JSON value.
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case []any:
		return len(x) > 0
	case *object:
		return len(x.keys) > 0
	case json.Number:
		f, err := x.Float64()
		return err != nil || f != 0
	}
	return true
}

// vaultSetup: a 1Password service account for one shared vault, its token locked to this Mac.
func (c *cli) vaultSetup(name string) error {
	if err := checkDuplicates(configuredSources(c.config()), name); err != nil {
		return err
	}
	fmt.Fprintf(c.errw, `Agents will sign in with the logins in the 1Password vault "%s", and nothing else. In 1Password
(Developer > Service Accounts, or developer.1password.com), create a service account with access to that vault
only: "Read Items" to sign in, plus "Write Items" so agents can save new passkeys there. Paste its token here,
or pipe it in: op read 'op://<vault>/<item>/credential' | %s vault setup <vault>
`, name, c.Name)
	// piped in (op read 'op://<vault>/<item>/credential' | ...) or typed at a hidden prompt; never an argument
	raw, err := readSecret("Service account token (ops_..., not shown): ")
	if err != nil {
		return err
	}
	tok := strings.TrimSpace(raw)
	if !opsToken.MatchString(tok) {
		return errors.New("that is not a service account token (they start with ops_)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	exact, err := vault.Check(ctx, tok, name, c.version)
	if err != nil {
		return fmt.Errorf("1Password: %v", err)
	}
	if err := checkDuplicates(configuredSources(c.config()), exact); err != nil {
		return err
	}
	// locked to this Mac's Secure Enclave, in a Keychain item only this program reads
	if err := vault.SaveToken(c.vaultInstance(), tok); err != nil {
		return fmt.Errorf("could not keep the token in the Keychain: %v", err)
	}
	c.forgetLegacyToken()
	cfg := c.config()
	op1, ok := cfg.get("onepassword").(*object) // keeps your 1Password app's accounts, if any
	if !ok {
		op1 = newObject()
	}
	op1.set("vault", exact)
	op1.del("op") // an older Chrome Relay's 1Password CLI: not used any more
	cfg.set("onepassword", op1)
	if err := c.saveConfig(cfg); err != nil {
		return err
	}
	c.printf("ok: agents can now use the vault \"%s\" (token in your login Keychain, locked to this Mac's Secure Enclave). Nothing to restart.\n", exact)
	if c.up() {
		return c.vaultList()
	}
	return nil
}

// forgetLegacyToken removes the token an older Chrome Relay kept with /usr/bin/security, if it is still there.
func (c *cli) forgetLegacyToken() {
	account := "1password-service-account"
	if c.Dev {
		account += "-dev-mode"
	}
	exec.Command("security", "delete-generic-password", "-s", "chrome-relay", "-a", account).Run()
}

// readSecret reads stdin when it is piped, else asks at a prompt that doesn't echo.
func readSecret(prompt string) (string, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		b, err := io.ReadAll(os.Stdin)
		return string(b), err
	}
	fmt.Fprint(os.Stderr, prompt)
	// Ctrl-C at the prompt: put the terminal's echo back before leaving
	state, err := term.GetState(fd)
	if err != nil {
		return "", err
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	defer signal.Stop(sig)
	go func() {
		if _, ok := <-sig; ok {
			term.Restore(fd, state)
			fmt.Fprintln(os.Stderr)
			os.Exit(130)
		}
	}()
	b, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	return string(b), err
}

type vaultItem struct {
	Vault    string   `json:"vault"`
	Account  string   `json:"account"`
	Title    string   `json:"title"`
	Websites []string `json:"websites"`
	Username string   `json:"username"`
	Password bool     `json:"password"`
	OTP      bool     `json:"otp"`
	Passkey  string   `json:"passkey"`
}

type vaultSummary struct {
	Vault  string      `json:"vault"` // the vaults, comma-separated
	Vaults []string    `json:"vaults"`
	Error  string      `json:"error"`
	Items  []vaultItem `json:"items"`
}

func (c *cli) vaultList() error {
	if err := c.ensureUp(); err != nil {
		return err
	}
	var d vaultSummary
	if err := c.get("/vault", vaultTimeout, &d); err != nil {
		return err
	}
	if d.Error != "" {
		return fmt.Errorf("1Password: %s", d.Error)
	}
	fmt.Fprint(c.out, formatVaultList(d, configuredSources(c.config())))
	return nil
}

// formatVaultList: the vaults agents may use, each with the account it's in, and its items (never values).
func formatVaultList(d vaultSummary, sources []source) string {
	vaults := d.Vaults
	if len(vaults) == 0 && d.Vault != "" { // an older relay: one vault
		vaults = []string{d.Vault}
	}
	for _, i := range d.Items { // items in a vault the relay didn't list: still show them
		if i.Vault != "" && !containsFold(vaults, i.Vault) {
			vaults = append(vaults, i.Vault)
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, `1Password vaults agents may use: %s. Agents fill a field with a reference, never the value:
  agent-browser fill @eN "op://<vault>/<item>/username"     (also: password, otp, or any field's name)

`, quoteList(vaults))
	labels := map[string]string{} // account uuid -> what to call it
	for _, s := range sources {
		labels[s.Account] = s.label()
	}
	for _, v := range vaults {
		where := ""
		if s, ok := sourceOf(sources, v); ok {
			where = s.label()
		}
		var items []vaultItem
		for _, i := range d.Items {
			if strings.EqualFold(i.Vault, v) || (i.Vault == "" && v == vaults[0]) { // an older relay doesn't say which vault
				items = append(items, i)
				if where == "" && i.Account != "" {
					where = labels[i.Account]
					if where == "" {
						where = i.Account
					}
				}
			}
		}
		if where != "" {
			fmt.Fprintf(&b, "  %s (%s)\n", v, where)
		} else {
			fmt.Fprintf(&b, "  %s\n", v)
		}
		for _, i := range items {
			var has []string
			if i.Password {
				has = append(has, "password")
			}
			if i.OTP {
				has = append(has, "one-time code")
			}
			if i.Passkey != "" {
				has = append(has, "passkey for "+i.Passkey)
			}
			sites := strings.Join(i.Websites, ", ")
			if sites == "" {
				sites = "(no website: cannot be filled)"
			}
			row := fmt.Sprintf("    %s %s %s %s", pyFormat(cut(i.Title, 30), 30), pyFormat(cut(sites, 40), 40), pyFormat(cut(i.Username, 28), 28), strings.Join(has, ", "))
			b.WriteString(strings.TrimRight(row, " ") + "\n")
		}
		if len(items) == 0 {
			b.WriteString("    (empty)\n")
		}
	}
	if len(vaults) == 0 {
		b.WriteString("  (empty)\n")
	}
	return b.String()
}
