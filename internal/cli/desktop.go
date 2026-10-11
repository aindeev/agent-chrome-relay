package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	op "github.com/1password/onepassword-sdk-go"

	"github.com/aindeev/agent-chrome-relay/internal/vault"
)

// Your 1Password app as the way in: the relay connects as you, one connection per 1Password account, and
// 1Password shows its own approval. Nothing is stored but the account and vault names, in config.json:
//
//	"onepassword": {"accounts": [{"account": "<uuid>", "label": "you@x.com", "vaults": ["Private"]}],
//	                "vault": "<a service account's vault>", "approve": "session" | "sign-in" | "off"}

// source is one way into 1Password and the vaults agents may use through it.
type source struct {
	Account string // a 1Password account (UUID or sidebar name); "" = the service account
	Label   string // what to call it: its email, or the name typed
	Vaults  []string
}

func (s source) label() string {
	switch {
	case s.Account == "":
		return "service account"
	case s.Label != "":
		return s.Label
	}
	return s.Account
}

var approveModes = []string{"session", "sign-in", "off"}

// configuredSources reads "onepassword" from config.json: the app's accounts, then the service account.
func configuredSources(cfg *object) []source {
	op1 := cfg.obj("onepassword")
	var out []source
	for _, a := range asList(op1.get("accounts")) {
		o, ok := a.(*object)
		if !ok || o.str("account") == "" {
			continue
		}
		s := source{Account: o.str("account"), Label: o.str("label")}
		for _, v := range asList(o.get("vaults")) {
			if name, ok := v.(string); ok && name != "" {
				s.Vaults = append(s.Vaults, name)
			}
		}
		out = append(out, s)
	}
	if v := op1.str("vault"); v != "" {
		out = append(out, source{Vaults: []string{v}})
	}
	return out
}

// sourceOf: where a vault agents may use lives.
func sourceOf(sources []source, vaultName string) (source, bool) {
	for _, s := range sources {
		if containsFold(s.Vaults, vaultName) {
			return s, true
		}
	}
	return source{}, false
}

func containsFold(list []string, s string) bool {
	return slices.ContainsFunc(list, func(x string) bool { return strings.EqualFold(x, s) })
}

func quoteList(names []string) string {
	q := make([]string, len(names))
	for i, n := range names {
		q[i] = strconv.Quote(n)
	}
	return strings.Join(q, ", ")
}

// checkDuplicates: references name only the vault (op://<vault>/<item>/<field>), so the vaults agents may use
// must have different names, whichever account they are in.
func checkDuplicates(sources []source, serviceVault string) error {
	if serviceVault != "" {
		sources = append(slices.Clip(sources), source{Vaults: []string{serviceVault}})
	}
	seen := map[string]source{}
	for _, s := range sources {
		for _, v := range s.Vaults {
			k := strings.ToLower(v)
			if first, dup := seen[k]; dup {
				where := first.label()
				if first.label() != s.label() {
					where += " and " + s.label()
				} else {
					where = "twice in " + where
				}
				return fmt.Errorf("two vaults agents would use are called %q (%s): agents name a vault only by its name, "+
					"in op://<vault>/<item>/<field>, so they must differ. Leave one out, or rename one in 1Password.", v, where)
			}
			seen[k] = s
		}
	}
	return nil
}

// applyDesktop writes the app's accounts into config.json, keeping the rest of it (a service account's vault
// unless serviceVault is "", and "approve" unless a new one is given).
func applyDesktop(cfg *object, sources []source, serviceVault, approve string) {
	op1, ok := cfg.get("onepassword").(*object)
	if !ok {
		op1 = newObject()
	}
	accounts := []any{}
	for _, s := range sources {
		o := newObject()
		o.set("account", s.Account)
		o.set("label", s.label())
		vaults := make([]any, len(s.Vaults)) // as parsed JSON has it, so the config reads back the same
		for i, v := range s.Vaults {
			vaults[i] = v
		}
		o.set("vaults", vaults)
		accounts = append(accounts, o)
	}
	op1.set("accounts", accounts)
	if serviceVault == "" {
		op1.del("vault")
	} else {
		op1.set("vault", serviceVault)
	}
	op1.del("op") // an older Chrome Relay's 1Password CLI: not used any more
	if approve != "" {
		op1.set("approve", approve)
	}
	cfg.set("onepassword", op1)
}

// okLine: `"Private", "Shared" (you@x.com) and "Employee" (you@work.com)`.
func okLine(sources []source, serviceVault string) string {
	if serviceVault != "" {
		sources = append(slices.Clip(sources), source{Vaults: []string{serviceVault}})
	}
	var parts []string
	for _, s := range sources {
		parts = append(parts, fmt.Sprintf("%s (%s)", quoteList(s.Vaults), s.label()))
	}
	if len(parts) > 1 {
		parts = append(parts[:len(parts)-2], parts[len(parts)-2]+" and "+parts[len(parts)-1])
	}
	return "ok: agents can use " + strings.Join(parts, ", ") + ". Nothing to restart."
}

// --- the 1Password accounts on this Mac ---

type opAccount struct {
	UUID  string `json:"account_uuid"`
	Email string `json:"email"`
	URL   string `json:"url"`
}

func (a opAccount) label() string {
	if a.Email != "" {
		return a.Email
	}
	return a.URL
}

func parseOpAccounts(b []byte) ([]opAccount, error) {
	var all []opAccount
	if err := json.Unmarshal(b, &all); err != nil {
		return nil, err
	}
	var out []opAccount
	for _, a := range all {
		if a.UUID != "" {
			out = append(out, a)
		}
	}
	return out, nil
}

func findOp() string {
	if p, err := exec.LookPath("op"); err == nil {
		return p
	}
	for _, p := range []string{"/opt/homebrew/bin/op", "/usr/local/bin/op"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// opAccounts: the SDK can't list accounts, but the 1Password CLI can, without asking anything. nil without it.
func opAccounts() []opAccount {
	path := findOp()
	if path == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "account", "list", "--format", "json")
	for _, kv := range os.Environ() { // a service account token or account in the environment changes what op lists
		if !strings.HasPrefix(kv, "OP_") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	accts, _ := parseOpAccounts(out)
	return accts
}

// desktopVaults connects to one account through the 1Password app (1Password asks the person) and lists its
// vaults' names. Never items.
func desktopVaults(ctx context.Context, account, version string) ([]string, error) {
	client, err := op.NewClient(ctx, op.WithDesktopAppIntegration(account), op.WithIntegrationInfo("Chrome Relay", version))
	if err != nil {
		return nil, err
	}
	vs, err := client.Vaults().List(ctx)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(vs))
	for i, v := range vs {
		names[i] = v.Title
	}
	return names, nil
}

// desktopEnv is what setup needs from this Mac (a test double in tests).
type desktopEnv struct {
	accounts func() []opAccount
	vaults   func(ctx context.Context, account string) ([]string, error)
}

func (c *cli) desktopEnv() desktopEnv {
	return desktopEnv{
		accounts: opAccounts,
		vaults: func(ctx context.Context, account string) ([]string, error) {
			return desktopVaults(ctx, account, c.version)
		},
	}
}

func (c *cli) connectVaults(w io.Writer, env desktopEnv, s source) ([]string, error) {
	fmt.Fprintf(w, "1Password will ask you to allow Chrome Relay for %s…\n", s.label())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	names, err := env.vaults(ctx, s.Account)
	if err != nil {
		hint := "Is the 1Password app open and unlocked, with Settings > Developer > \"Integrate with other apps\" on?"
		if strings.Contains(err.Error(), "Denied authorization") {
			hint = "1Password was not allowed to connect Chrome Relay: run this again and choose Allow in 1Password's prompt."
		}
		return nil, fmt.Errorf("1Password (%s): %v\n%s", s.label(), err, hint)
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("1Password (%s): no vaults", s.label())
	}
	return names, nil
}

// --- `vault setup` ---

type setupArgs struct {
	service string // `vault setup <vault>`: a service account's vault
	desktop bool
	sources []source // --account X --vaults "A,B", repeated
	approve string
}

func parseSetupArgs(args []string) (setupArgs, error) {
	var a setupArgs
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") {
			if a.service != "" {
				return a, fmt.Errorf("one vault name only, got %q and %q", a.service, arg)
			}
			a.service = arg
			continue
		}
		name, val, hasVal := strings.Cut(arg, "=")
		value := func() (string, error) {
			if hasVal {
				return val, nil
			}
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s needs a value", name)
			}
			i++
			return args[i], nil
		}
		switch name {
		case "--desktop":
			a.desktop = true
		case "--account":
			v, err := value()
			if err != nil {
				return a, err
			}
			a.sources = append(a.sources, source{Account: strings.TrimSpace(v)})
		case "--vaults":
			v, err := value()
			if err != nil {
				return a, err
			}
			if len(a.sources) == 0 {
				return a, errors.New("--vaults goes after the --account it belongs to")
			}
			last := &a.sources[len(a.sources)-1]
			last.Vaults = append(last.Vaults, splitList(v)...)
		case "--approve":
			v, err := value()
			if err != nil {
				return a, err
			}
			if !slices.Contains(approveModes, v) {
				return a, fmt.Errorf("--approve is session, sign-in or off, not %q", v)
			}
			a.approve = v
		default:
			return a, fmt.Errorf("unknown option %s", arg)
		}
	}
	for _, s := range a.sources {
		if len(s.Vaults) == 0 {
			return a, fmt.Errorf("--account %s needs --vaults \"A,B\"", s.Account)
		}
	}
	if a.service != "" && (a.desktop || len(a.sources) > 0 || a.approve != "") {
		return a, errors.New("`vault setup <vault>` is for a service account; --desktop, --account and --approve are for your 1Password app: one at a time")
	}
	return a, nil
}

func splitList(s string) []string {
	var out []string
	for _, x := range strings.Split(s, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

// desktopSetupFlags: `vault setup --account X --vaults "A,B" ...`, no questions. Connects to each account the
// same way the relay will, to check the vaults are there.
func (c *cli) desktopSetupFlags(a setupArgs, env desktopEnv) error {
	known := env.accounts()
	sources := slices.Clone(a.sources)
	for i, s := range sources {
		for _, k := range known { // an email or URL from `op account list` works too; the UUID is what's kept
			if strings.EqualFold(s.Account, k.UUID) || strings.EqualFold(s.Account, k.Email) || strings.EqualFold(s.Account, k.URL) {
				sources[i].Account, sources[i].Label = k.UUID, k.label()
			}
		}
		names, err := c.connectVaults(c.errw, env, sources[i])
		if err != nil {
			return err
		}
		for j, want := range s.Vaults {
			k := slices.IndexFunc(names, func(n string) bool { return strings.EqualFold(n, want) })
			if k < 0 {
				return fmt.Errorf("%s has no vault %q (it has: %s)", sources[i].label(), want, quoteList(names))
			}
			sources[i].Vaults[j] = names[k]
		}
	}
	return c.saveDesktop(sources, false, a.approve)
}

// desktopSetup asks: which way in (when ask), which accounts, which vaults in each, how often to ask for Touch ID.
func (c *cli) desktopSetup(p *prompter, env desktopEnv, ask bool) error {
	if ask {
		fmt.Fprint(p.out, "How should agents reach 1Password?\n"+
			"  1) Your 1Password app (your accounts; you approve in 1Password) [default]\n"+
			"  2) A service account for one shared vault\n")
		n, err := p.pick("Choose [1]:", 2, "1", false)
		if err != nil {
			return err
		}
		if n[0] == 2 {
			name, err := p.ask("Which vault may the service account open?")
			if err != nil {
				return err
			}
			if name == "" {
				return errors.New("no vault name given")
			}
			return c.vaultSetup(name)
		}
	}
	var chosen []source
	if accts := env.accounts(); len(accts) > 0 {
		fmt.Fprintln(p.out, "Your 1Password accounts:")
		for i, a := range accts {
			fmt.Fprintf(p.out, "  %d) %s  %s\n", i+1, a.Email, a.URL)
		}
		q, def := "Which should agents use? (numbers, comma-separated):", ""
		if len(accts) == 1 {
			q, def = "Which should agents use? [1]:", "1"
		}
		picked, err := p.pick(q, len(accts), def, true)
		if err != nil {
			return err
		}
		for _, i := range picked {
			chosen = append(chosen, source{Account: accts[i-1].UUID, Label: accts[i-1].label()})
		}
	} else {
		names, err := p.ask("Your 1Password account names, as shown at the top of 1Password's sidebar (comma-separated):")
		if err != nil {
			return err
		}
		for _, n := range splitList(names) {
			chosen = append(chosen, source{Account: n, Label: n})
		}
		if len(chosen) == 0 {
			return errors.New("no account given")
		}
	}
	for i := range chosen {
		names, err := c.connectVaults(p.out, env, chosen[i])
		if err != nil {
			return err
		}
		fmt.Fprintf(p.out, "Vaults in %s:\n", chosen[i].label())
		for j, n := range names {
			fmt.Fprintf(p.out, "  %d) %s\n", j+1, n)
		}
		picked, err := p.pick("Let agents use (numbers, comma-separated):", len(names), "", true)
		if err != nil {
			return err
		}
		for _, j := range picked {
			chosen[i].Vaults = append(chosen[i].Vaults, names[j-1])
		}
	}
	dropService := false
	if service := c.config().obj("onepassword").str("vault"); service != "" {
		a, err := p.ask(fmt.Sprintf("Keep the service account's vault %q too? [Y/n]:", service))
		if err != nil {
			return err
		}
		dropService = strings.HasPrefix(strings.ToLower(a), "n")
	}
	fmt.Fprint(p.out, "How often should Chrome Relay ask for your Touch ID?\n"+
		"  1) once per agent session [default]\n"+
		"  2) every sign-in\n"+
		"  3) never (1Password's own prompt only)\n")
	n, err := p.pick("Choose [1]:", 3, "1", false)
	if err != nil {
		return err
	}
	return c.saveDesktop(chosen, dropService, approveModes[n[0]-1])
}

func (c *cli) saveDesktop(sources []source, dropService bool, approve string) error {
	cfg := c.config()
	service := cfg.obj("onepassword").str("vault")
	if dropService {
		service = ""
	}
	if err := checkDuplicates(sources, service); err != nil {
		return err
	}
	if dropService {
		if err := vault.ForgetToken(c.vaultInstance()); err != nil {
			return fmt.Errorf("could not remove the service account token from the Keychain: %v", err)
		}
	}
	applyDesktop(cfg, sources, service, approve)
	if err := c.saveConfig(cfg); err != nil {
		return err
	}
	c.println(okLine(sources, service))
	return nil
}

// prompter asks questions on the terminal (or reads answers piped in, one per line).
type prompter struct {
	in  *bufio.Reader
	out io.Writer
}

func newPrompter(r io.Reader, w io.Writer) *prompter { return &prompter{bufio.NewReader(r), w} }

func (p *prompter) ask(q string) (string, error) {
	fmt.Fprint(p.out, q+" ")
	line, err := p.in.ReadString('\n')
	if err != nil && (err != io.EOF || line == "") {
		fmt.Fprintln(p.out)
		return "", errors.New("cancelled: no answer")
	}
	return strings.TrimSpace(line), nil
}

// pick asks until the answer is valid: numbers from 1 to n (several only when multi); "" means def.
func (p *prompter) pick(q string, n int, def string, multi bool) ([]int, error) {
	for {
		a, err := p.ask(q)
		if err != nil {
			return nil, err
		}
		if a == "" {
			a = def
		}
		picked, err := parseChoice(a, n, multi)
		if err == nil {
			return picked, nil
		}
		fmt.Fprintln(p.out, err)
	}
}

func parseChoice(a string, n int, multi bool) ([]int, error) {
	var out []int
	for _, f := range strings.FieldsFunc(a, func(r rune) bool { return r == ',' || r == ' ' }) {
		i, err := strconv.Atoi(f)
		if err != nil || i < 1 || i > n {
			return nil, fmt.Errorf("%q is not one of 1 to %d", f, n)
		}
		if !slices.Contains(out, i) {
			out = append(out, i)
		}
	}
	switch {
	case len(out) == 0:
		return nil, errors.New("pick at least one number")
	case len(out) > 1 && !multi:
		return nil, errors.New("pick one number")
	}
	return out, nil
}
