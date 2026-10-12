package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/aindeev/agent-chrome-relay/internal/phone"
)

// runOp runs the 1Password CLI: it may ask the person to approve in the 1Password app or sign in (on the
// terminal), so stdin is theirs; stdout and stderr come back. OP_* variables (a token or account in the
// environment) are left out, so it acts as the person.
func (c *cli) runOp(args ...string) (stdout, stderr []byte, err error) {
	if c.op != nil {
		return c.op(args...)
	}
	path := findOp()
	if path == "" {
		return nil, nil, errors.New("no 1Password CLI")
	}
	cmd := exec.Command(path, args...)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "OP_") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	var out, errb bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, &out, &errb
	err = cmd.Run()
	return out.Bytes(), errb.Bytes(), err
}

// opError: op's own message, without its "[ERROR] 2026/10/11 12:00:00" prefix.
func opError(stderr []byte, err error) string {
	s := strings.TrimSpace(string(stderr))
	if i := strings.Index(s, "] "); strings.HasPrefix(s, "[ERROR]") && i > 0 {
		s = strings.TrimSpace(s[i+2:])
		if len(s) > 20 && s[4] == '/' && s[7] == '/' { // the date and time op puts first
			s = strings.TrimSpace(s[20:])
		}
	}
	if s == "" && err != nil {
		s = err.Error()
	}
	return s
}

func isPrivateVault(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	return n == "private" || n == "personal" || n == "employee"
}

func (c *cli) accounts() []opAccount {
	if c.opAccts != nil {
		return c.opAccts()
	}
	return opAccounts()
}

// pickAccount: the account named with --account, the only one, or the person's choice.
func (c *cli) pickAccount(want string, p *prompter) (opAccount, error) {
	accts := c.accounts()
	if len(accts) == 0 {
		return opAccount{}, errors.New("the 1Password CLI lists no accounts: in the 1Password app, turn on Settings > Developer > \"Integrate with 1Password CLI\", then run this again")
	}
	if want != "" {
		for _, a := range accts {
			if strings.EqualFold(want, a.UUID) || strings.EqualFold(want, a.Email) || strings.EqualFold(want, a.URL) {
				return a, nil
			}
		}
		return opAccount{}, fmt.Errorf("no 1Password account %q on this Mac (there are: %s)", want, accountList(accts))
	}
	if len(accts) == 1 {
		return accts[0], nil
	}
	if p == nil {
		return opAccount{}, fmt.Errorf("several 1Password accounts on this Mac: add --account <one of %s>", accountList(accts))
	}
	fmt.Fprintln(p.out, "Which 1Password account should hold the vault and the service account?")
	for i, a := range accts {
		fmt.Fprintf(p.out, "  %d) %s  %s\n", i+1, a.Email, a.URL)
	}
	n, err := p.pick("Choose:", len(accts), "", false)
	if err != nil {
		return opAccount{}, err
	}
	return accts[n[0]-1], nil
}

func accountList(accts []opAccount) string {
	var s []string
	for _, a := range accts {
		s = append(s, a.Email)
	}
	return strings.Join(s, ", ")
}

// createServiceAccount makes, with the person's 1Password CLI: the vault if it doesn't exist, then a service
// account that can read and write that vault and nothing else. Its token goes from op straight to the caller,
// never to the screen. Says what it made.
func (c *cli) createServiceAccount(name, account string, p *prompter) (string, opAccount, error) {
	acct, err := c.pickAccount(account, p)
	if err != nil {
		return "", acct, err
	}
	where := acct.label()
	fmt.Fprintf(c.errw, "Using your 1Password account %s (1Password may ask you to approve).\n", where)
	var v struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	out, errb, err := c.runOp("vault", "get", name, "--account", acct.UUID, "--format", "json")
	switch {
	case err == nil:
		json.Unmarshal(out, &v)
		fmt.Fprintf(c.errw, "Found the vault %q.\n", v.Name)
	case strings.Contains(strings.ToLower(string(errb)), "isn't a vault") || strings.Contains(strings.ToLower(string(errb)), "not found"):
		if p != nil && !p.yes(fmt.Sprintf("There's no vault %q in %s yet. Create it? [Y/n]", name, where), true) {
			return "", acct, fmt.Errorf("no vault %q: create it in 1Password, or run this again and let Chrome Relay create it", name)
		}
		out, errb, err = c.runOp("vault", "create", name, "--account", acct.UUID, "--description",
			"Logins Chrome Relay's agents may use. Made by `chrome-relay vault setup`.", "--format", "json")
		if err != nil {
			return "", acct, fmt.Errorf("1Password could not create the vault %q: %s", name, opError(errb, err))
		}
		json.Unmarshal(out, &v)
		fmt.Fprintf(c.errw, "Created the vault %q in %s. Move or copy into it the logins agents may use.\n", v.Name, where)
	default:
		return "", acct, fmt.Errorf("1Password: %s", opError(errb, err))
	}
	if v.Name == "" {
		v.Name = name
	}
	sa := fmt.Sprintf("Chrome Relay (%s, %s)", phone.MacName(), time.Now().Format("2006-01-02"))
	if c.Dev {
		sa = fmt.Sprintf("Chrome Relay dev mode (%s, %s)", phone.MacName(), time.Now().Format("2006-01-02"))
	}
	out, errb, err = c.runOp("service-account", "create", sa, "--vault", v.Name+":read_items,write_items",
		"--account", acct.UUID, "--raw")
	if err != nil {
		msg := opError(errb, err)
		return "", acct, fmt.Errorf("1Password could not create a service account: %s\n"+
			"(Only an owner or administrator can, on plans with service accounts.) Or make one yourself on 1Password.com, "+
			"Developer > Directory > Service Accounts, with Read and Write on %q, and run `%s vault setup %q --paste`", msg, v.Name, c.Name, v.Name)
	}
	tok := strings.TrimSpace(string(out))
	fmt.Fprintf(c.errw, "Created the service account %q: read and write on %q, nothing else. Its token goes straight into\n"+
		"this Mac's Keychain, never shown. See or revoke it on 1Password.com: Developer > Directory > Service Accounts.\n", sa, v.Name)
	return tok, acct, nil
}

// serviceSetupInteractive: which 1Password accounts (one, several, all), a vault name for each (unique, since
// agents name vaults only by name), then a vault and a service account in each.
func (c *cli) serviceSetupInteractive(p *prompter) error {
	fmt.Fprintln(p.out, "Agents reach 1Password through service accounts: a shared vault per 1Password account, which Chrome Relay\n"+
		"creates with your 1Password CLI. They work in the background and while you're away; after this, no 1Password\n"+
		"prompts. (1Password never lets a service account into a Private vault: move the logins agents may use into\n"+
		"the shared one.)")
	accts := c.accounts()
	if len(accts) == 0 || findOp() == "" {
		name, err := p.ask("Which vault may the service account open? [Shared with Chrome Relay]")
		if err != nil {
			return err
		}
		return c.vaultSetup(setupArgs{service: cmpOr(name, "Shared with Chrome Relay"), paste: true}, p)
	}
	chosen := accts
	if len(accts) > 1 {
		fmt.Fprintln(p.out, "Your 1Password accounts:")
		for i, a := range accts {
			fmt.Fprintf(p.out, "  %d) %s  %s\n", i+1, a.Email, a.URL)
		}
		picked, err := p.pick("Which should agents use? (numbers, comma-separated; Enter = all):", len(accts), allNumbers(len(accts)), true)
		if err != nil {
			return err
		}
		chosen = nil
		for _, i := range picked {
			chosen = append(chosen, accts[i-1])
		}
	}
	names := map[string]bool{}
	for _, a := range chosen {
		def := "Shared with Chrome Relay"
		if len(chosen) > 1 {
			def += " (" + shortName(a) + ")"
		}
		name, err := p.ask(fmt.Sprintf("Vault for agents in %s [%s]:", a.Email, def))
		if err != nil {
			return err
		}
		name = cmpOr(name, def)
		if names[strings.ToLower(name)] {
			return fmt.Errorf("two accounts can't both use a vault called %q: agents name vaults only by name", name)
		}
		names[strings.ToLower(name)] = true
		if err := c.vaultSetup(setupArgs{service: name, account: a.UUID}, p); err != nil {
			return fmt.Errorf("%s: %w", a.Email, err)
		}
	}
	return nil
}

// shortName: "personal" for my.1password.com, else the account's subdomain ("acme" for acme.1password.com).
func shortName(a opAccount) string {
	sub, _, _ := strings.Cut(a.URL, ".")
	if sub == "" || sub == "my" {
		return "personal"
	}
	return sub
}

func allNumbers(n int) string {
	var s []string
	for i := 1; i <= n; i++ {
		s = append(s, fmt.Sprint(i))
	}
	return strings.Join(s, ",")
}

func cmpOr(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return strings.TrimSpace(s)
}
