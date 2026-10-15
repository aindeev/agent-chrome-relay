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
	"slices"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/aindeev/agent-chrome-relay/internal/macos"
	"github.com/aindeev/agent-chrome-relay/internal/phone"
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
			return fmt.Errorf("%v\n%s", err, c.setupUsage())
		}
		var p *prompter
		if term.IsTerminal(int(os.Stdin.Fd())) {
			p = newPrompter(os.Stdin, c.errw)
		}
		switch {
		case a.service != "":
			return c.vaultSetup(a, p)
		case p != nil:
			return c.serviceSetupInteractive(p)
		}
		return errors.New(c.setupUsage())
	case sub == "approve":
		return c.vaultApprove(c.arg(3))
	case sub == "forget":
		cfg := c.config()
		var err error
		for _, k := range append(serviceKeys(configuredSources(cfg)), "") {
			if e := vault.ForgetToken(c.vaultInstance(), k); e != nil {
				err = e
			}
		}
		c.forgetLegacyToken()
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

// vaultSetup: a 1Password service account for one shared vault, its token locked to this Mac. With the 1Password
// CLI it does everything itself (the vault if it's missing, the service account, the token straight into the
// Keychain, never shown); --paste, or no CLI, takes a token the person made.
func (c *cli) vaultSetup(a setupArgs, p *prompter) error {
	name := a.service
	if err := checkDuplicates(configuredSources(c.config()), name); err != nil {
		return err
	}
	if isPrivateVault(name) {
		return fmt.Errorf("1Password never lets a service account into a Private vault: choose a shared one, e.g. `%s vault setup \"Shared with Chrome Relay\"` (created if it doesn't exist), and move the logins agents may use there", c.Name)
	}
	var tok string
	var acct opAccount // where the service account lives; empty for a pasted token without --account
	if a.paste && a.account != "" {
		var err error
		if acct, err = c.pickAccount(a.account, nil); err != nil {
			return err
		}
	}
	if a.paste || findOp() == "" {
		if !a.paste {
			fmt.Fprintf(c.errw, "No 1Password CLI (`brew install 1password-cli`), so Chrome Relay can't create the service account itself.\n")
		}
		fmt.Fprintf(c.errw, `Agents will sign in with the logins in the 1Password vault "%s", and nothing else. On 1Password.com
(Developer > Directory > Service Accounts), create a service account with access to that vault only: "Read Items"
to sign in, plus "Write Items" so agents can save new logins, 2-step codes and passkeys there. Paste its token here, or pipe it in:
op read 'op://<vault>/<item>/credential' | %s vault setup <vault> --paste
`, name, c.Name)
		// piped in (op read 'op://<vault>/<item>/credential' | ...) or typed at a hidden prompt; never an argument
		raw, err := readSecret("Service account token (ops_..., not shown): ")
		if err != nil {
			return err
		}
		tok = strings.TrimSpace(raw)
	} else {
		var err error
		if tok, acct, err = c.createServiceAccount(name, a.account, p); err != nil {
			return err
		}
	}
	if !opsToken.MatchString(tok) {
		return errors.New("that is not a service account token (they start with ops_)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	exact, err := vault.Check(ctx, tok, name, c.version)
	if err != nil {
		return fmt.Errorf("1Password: %v", err)
	}
	// this account's service account replaces its previous one, and the older single one if it had this vault
	cfg := c.config()
	op1, ok := cfg.get("onepassword").(*object) // keeps your 1Password app's accounts, if any
	if !ok {
		op1 = newObject()
	}
	legacy := strings.EqualFold(op1.str("vault"), exact) || acct.UUID == ""
	var others []source
	for _, s := range configuredSources(cfg) {
		if (s.Key == "" && legacy) || (s.Key != "" && s.Key == acct.UUID) {
			continue
		}
		others = append(others, s)
	}
	if err := checkDuplicates(others, exact); err != nil {
		return err
	}
	approve := "sign-in"
	if acct.UUID != "" { // keep the person's approval setting, if a token already had one
		for _, k := range serviceKeys(configuredSources(cfg)) {
			if t, err := (vault.MacPerson{Instance: c.vaultInstance(), CLI: c.Name}).Unlock(k, ""); err == nil && t.Approve != "" {
				approve = t.Approve
				break
			}
		}
	}
	// locked to this Mac's Secure Enclave, in a Keychain item only this program reads
	if err := vault.SaveToken(c.vaultInstance(), acct.UUID, vault.Token{Value: tok, Approve: approve}); err != nil {
		return fmt.Errorf("could not keep the token in the Keychain: %v", err)
	}
	c.forgetLegacyToken()
	if acct.UUID == "" {
		op1.set("vault", exact)
	} else {
		if legacy && op1.has("vault") {
			op1.del("vault")
			vault.ForgetToken(c.vaultInstance(), "")
		}
		list := []any{}
		for _, x := range asList(op1.get("serviceAccounts")) {
			if o, ok := x.(*object); ok && o.str("account") != acct.UUID {
				list = append(list, o)
			}
		}
		o := newObject()
		o.set("account", acct.UUID)
		o.set("label", acct.label())
		o.set("vault", exact)
		op1.set("serviceAccounts", append(list, o))
	}
	op1.del("op")            // an older Chrome Relay's 1Password CLI: not used any more
	if op1.has("accounts") { // the 1Password app connection, which couldn't work in the background
		op1.del("accounts")
		op1.del("approve")
		fmt.Fprintln(c.errw, "Removed the 1Password app connections from an earlier setup: Chrome Relay uses service accounts only now.")
	}
	cfg.set("onepassword", op1)
	if err := c.saveConfig(cfg); err != nil {
		return err
	}
	where := ""
	if acct.UUID != "" {
		where = " in " + acct.label()
	}
	c.printf("ok: agents can now use the vault \"%s\"%s (token in your login Keychain, locked to this Mac's Secure Enclave). Nothing to restart.\n", exact, where)
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
	ID       string   `json:"id"`
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
  chrome-relay fill @eN "op://<vault>/<item>/username"      (also: password, otp, or any field's name)

`, quoteList(vaults))
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
					where = "service account in " + i.Account
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
			if i.ID != "" && slices.ContainsFunc(items, func(o vaultItem) bool { return o.ID != i.ID && strings.EqualFold(o.Title, i.Title) }) {
				fmt.Fprintf(&b, "      shares its title: refer to it as op://%s/%s/<field>\n", v, i.ID)
			}
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

// vaultApprove sets how often Chrome Relay asks before agents sign in with 1Password: every sign-in, once per
// agent session, or never. Changing it takes the person's approval (Touch ID, or Face ID on a paired iPhone), and
// with a service account the setting is kept with its token, where no file an agent could edit reaches it.
func (c *cli) vaultApprove(mode string) error {
	if !slices.Contains(approveModes, mode) {
		return fmt.Errorf("usage: %s vault approve session|sign-in|off", c.Name)
	}
	if !macos.Linked {
		return errCoreOnly
	}
	what := map[string]string{"session": "once per agent session", "sign-in": "every sign-in", "off": "never"}[mode]
	person := vault.MacPerson{Instance: c.vaultInstance(), CLI: c.Name}
	if err := person.Approve(phone.Details{Reason: fmt.Sprintf("change how often it asks before agents sign in with your 1Password to: %s (decline unless you just ran `%s vault approve %s`)", what, c.Name, mode),
		Agent: c.Name + " in your Terminal", Site: phone.MacName(), What: "approvals: " + what}); err != nil {
		return err
	}
	for _, k := range serviceKeys(configuredSources(c.config())) { // kept with each token
		t, err := person.Unlock(k, "change how often Chrome Relay asks before agents sign in with your 1Password")
		if err != nil {
			return err
		}
		t.Approve = mode
		if err := vault.SaveToken(c.vaultInstance(), k, t); err != nil {
			return err
		}
	}
	cfg := c.config()
	op1, ok := cfg.get("onepassword").(*object)
	if !ok {
		return fmt.Errorf("1Password isn't set up: `%s vault setup`", c.Name)
	}
	op1.set("approve", mode)
	cfg.set("onepassword", op1)
	if err := c.saveConfig(cfg); err != nil {
		return err
	}
	c.printf("Chrome Relay now asks %s before agents sign in with 1Password.\n", what)
	return nil
}

func (c *cli) setupUsage() string {
	return fmt.Sprintf(`usage: %[1]s vault setup                      asks which 1Password accounts, then makes a vault and a service account in each
       %[1]s vault setup <vault> [--account <email or uuid>]
                                             one account: creates the vault if missing and its service account, with
                                             your 1Password CLI; the token goes straight into the Keychain
       %[1]s vault setup <vault> --paste [--account ...]
                                             a service account you made on 1Password.com: paste or pipe its token`, c.Name)
}
