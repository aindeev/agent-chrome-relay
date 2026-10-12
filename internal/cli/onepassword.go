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
)

// 1Password, as agents reach it: service accounts, one per 1Password account, each able to open one shared vault.
// config.json keeps only the names; each token is in its own Keychain item, locked to this Mac:
//
//	"onepassword": {"serviceAccounts": [{"account": "<1Password account UUID>", "label": "you@x.com",
//	                                     "vault": "Shared with Chrome Relay (personal)"}, ...],
//	                "vault": "<the older, single service account's vault>"}

// source is a service account and the vault agents may use through it.
type source struct {
	Label  string // its 1Password account ("you@x.com"); "" for the older, single one
	Vaults []string
	Key    string // its 1Password account's UUID, which keys its token ("" = the older, single one)
}

func (s source) label() string {
	if s.Label != "" {
		return "service account in " + s.Label
	}
	return "service account"
}

var approveModes = []string{"session", "sign-in", "off"}

// configuredSources reads "onepassword" from config.json.
func configuredSources(cfg *object) []source {
	op1 := cfg.obj("onepassword")
	var out []source
	if v := op1.str("vault"); v != "" {
		out = append(out, source{Vaults: []string{v}})
	}
	for _, a := range asList(op1.get("serviceAccounts")) {
		if o, ok := a.(*object); ok && o.str("vault") != "" && o.str("account") != "" {
			out = append(out, source{Key: o.str("account"), Label: o.str("label"), Vaults: []string{o.str("vault")}})
		}
	}
	return out
}

// appAccounts: 1Password app connections an older Chrome Relay set up ("accounts"); no longer used.
func appAccounts(cfg *object) int { return len(asList(cfg.obj("onepassword").get("accounts"))) }

// serviceKeys: every service account token's key ("" for the older, single one).
func serviceKeys(sources []source) []string {
	var keys []string
	for _, s := range sources {
		keys = append(keys, s.Key)
	}
	return keys
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
	if os.Getenv("CHROME_RELAY_NO_OP_CLI") == "1" { // tests: never the person's real 1Password
		return ""
	}
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

// --- `vault setup` ---

type setupArgs struct {
	service string // the vault for one service account
	account string // which 1Password account (--account)
	paste   bool   // paste a token instead of creating the service account (--paste)
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
		switch name {
		case "--account":
			if !hasVal {
				if i+1 >= len(args) {
					return a, errors.New("--account needs a value")
				}
				i++
				val = args[i]
			}
			a.account = strings.TrimSpace(val)
		case "--paste":
			a.paste = true
		default:
			return a, fmt.Errorf("unknown option %s", arg)
		}
	}
	if a.paste && a.service == "" {
		return a, errors.New("--paste goes with a vault name: `vault setup <vault> --paste`")
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

// prompter asks questions on the terminal (or reads answers piped in, one per line).
type prompter struct {
	in  *bufio.Reader
	out io.Writer
}

func newPrompter(r io.Reader, w io.Writer) *prompter { return &prompter{bufio.NewReader(r), w} }

// yes asks a yes/no question; Enter takes def.
func (p *prompter) yes(q string, def bool) bool {
	a, err := p.ask(q)
	if err != nil {
		return false
	}
	if a = strings.ToLower(strings.TrimSpace(a)); a == "" {
		return def
	}
	return strings.HasPrefix(a, "y")
}

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
