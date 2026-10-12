package vault

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"slices"
	"strconv"
	"strings"

	op "github.com/1password/onepassword-sdk-go"

	"github.com/aindeev/agent-chrome-relay/internal/phone"
)

// Saving: agents create accounts and turn on 2-step verification, and the secrets go straight into 1Password.
// They name what to save with a reference, as when filling, and never see the value:
//
//	op://<vault>/<title>/password?new&username=<login>[&length=20][&symbols=no]
//	    a new login: Chrome Relay generates the password, saves the item, types it. The same reference again (the
//	    "confirm password" field) types the same password.
//	op://<vault>/<item>/otp?new
//	    2-step verification: Chrome Relay reads the setup key from the page (its QR code or the key in text), saves
//	    it, and types the current code. On a login Chrome Relay saved, into that item; on the person's own login,
//	    into a separate item beside it, since Chrome Relay never changes the person's items.
//
// Each save asks the person (unless they turned approvals off), naming the agent, the site and the account.

// SavedTag marks the logins Chrome Relay saved: like its passkey items, the only items it changes.
const SavedTag = "chrome-relay-saved"

// SaveRef is a parsed ?new reference.
type SaveRef struct {
	Vault, Item, Field string
	Username           string
	Length             int
	Symbols            bool
}

// ParseSave: the reference, if it asks to save (has ?new).
func ParseSave(ref string) (SaveRef, bool, error) {
	m := Ref.FindStringSubmatch(strings.TrimSpace(ref))
	if m == nil {
		return SaveRef{}, false, nil
	}
	q, _ := url.ParseQuery(m[4])
	if !q.Has("new") {
		return SaveRef{}, false, nil
	}
	r := SaveRef{Vault: m[1], Item: unescape(m[2]), Field: strings.ToLower(unescape(m[3])), Username: q.Get("username"), Length: 24, Symbols: true}
	if l := q.Get("length"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 12 || n > 64 {
			return r, true, errors.New("length is 12 to 64 characters")
		}
		r.Length = n
	}
	if s := strings.ToLower(q.Get("symbols")); s == "no" || s == "false" || s == "0" {
		r.Symbols = false
	}
	switch {
	case r.Field == "password" && r.Username == "":
		return r, true, errors.New("a new login needs its username: op://<vault>/<title>/password?new&username=<login>")
	case r.Field != "password" && !otpNames[r.Field]:
		return r, true, fmt.Errorf("?new saves a password or a one-time code (otp), not %q", r.Field)
	}
	return r, true, nil
}

// Generate: a random password, with at least one lower case letter, upper case letter and digit (and symbol).
func Generate(length int, symbols bool) string {
	sets := []string{"abcdefghijkmnopqrstuvwxyz", "ABCDEFGHJKLMNPQRSTUVWXYZ", "23456789"}
	if symbols {
		sets = append(sets, "-_.!@#%^*+=")
	}
	all := strings.Join(sets, "")
	pick := func(s string) byte {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(s))))
		return s[n.Int64()]
	}
	for {
		b := make([]byte, length)
		for i := range b {
			b[i] = pick(all)
		}
		if !slices.ContainsFunc(sets, func(set string) bool { return !strings.ContainsAny(string(b), set) }) {
			return string(b)
		}
	}
}

// approveSave asks the person before anything is written to their vault (unless they turned approvals off).
func (v *Vault) approveSave(who Who, host, what, reason string) error {
	if v.tokenApprove == "off" {
		return nil
	}
	v.mu.Unlock()
	err := v.Person.Approve(phone.Details{Reason: reason, Agent: who.Name, Site: host, What: what})
	v.mu.Lock()
	return err
}

// place for a vault name the agent gave, after open.
func (v *Vault) placeFor(name string) (place, error) {
	s := v.Settings()
	if !slices.ContainsFunc(s.Vaults(), func(n string) bool { return strings.EqualFold(n, name) }) {
		return place{}, fmt.Errorf("only the vaults %s are available to agents, not %q", quoted(s.Vaults()), name)
	}
	p, ok := v.places[strings.ToLower(name)]
	if !ok {
		return place{}, fmt.Errorf("the vault %q is not open", name)
	}
	return p, nil
}

// NewLogin saves a new login with a generated password, for the page's site, and returns the password to type.
func (v *Vault) NewLogin(ctx context.Context, r SaveRef, pageURL string, who Who) (Value, error) {
	u, err := url.Parse(pageURL)
	if err != nil || !SecureOrigin(u) {
		return Value{}, fmt.Errorf("logins are only saved for https pages, not %s", pageURL)
	}
	host := u.Hostname()
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := v.open(ctx, fmt.Sprintf("let %s save a new login for %s in 1Password", who.Name, host)); err != nil {
		return Value{}, err
	}
	p, err := v.placeFor(r.Vault)
	if err != nil {
		return Value{}, err
	}
	list, err := v.itemList(ctx, p)
	if err != nil {
		return Value{}, err
	}
	for _, o := range list { // never a second login for the same account
		if !HostMatches(overviewURLs(o), host) && !related(overviewURLs(o), host) {
			continue
		}
		if it, err := v.item(ctx, p, o.ID); err == nil {
			if n := PickField(it, "username", false); n != nil && strings.EqualFold(n.Value, r.Username) {
				return Value{}, fmt.Errorf("%q in %q is already the login for %s on %s: fill op://%s/%s/password instead (changing a saved password is the person's to do)", it.Title, p.name, r.Username, host, p.name, it.ID)
			}
		}
	}
	reason := fmt.Sprintf("let %s save a new login for %s as %s in your 1Password vault %q (Chrome Relay makes the password)", who.Name, host, r.Username, p.name)
	if err := v.approveSave(who, host, "new login as "+r.Username, reason); err != nil {
		return Value{}, err
	}
	pw := Generate(r.Length, r.Symbols)
	notes := fmt.Sprintf("Saved by Chrome Relay for %s, signing up on %s.", who.Name, host)
	saved, err := p.store.Create(ctx, op.ItemCreateParams{Category: op.ItemCategoryLogin, VaultID: p.id, Title: r.Item, Tags: []string{SavedTag}, Notes: &notes,
		Fields: []op.ItemField{{ID: "username", Title: "username", FieldType: op.ItemFieldTypeText, Value: r.Username},
			{ID: "password", Title: "password", FieldType: op.ItemFieldTypeConcealed, Value: pw}},
		Websites: []op.Website{{URL: "https://" + host, Label: "website", AutofillBehavior: op.AutofillBehaviorAnywhereOnWebsite}}})
	v.forget()
	if err != nil {
		return Value{}, err
	}
	return Value{pw, saved.Title, "password", "secret"}, nil
}

// SaveOTP keeps a 2-step setup key the page showed (an otpauth:// URI or a base32 key) and returns the current
// code to type. The reference names the login it belongs to.
func (v *Vault) SaveOTP(ctx context.Context, r SaveRef, secret, pageURL string, who Who) (Value, error) {
	u, err := url.Parse(pageURL)
	if err != nil || !SecureOrigin(u) {
		return Value{}, fmt.Errorf("one-time codes are only saved for https pages, not %s", pageURL)
	}
	host := u.Hostname()
	if _, _, err := TOTP(secret, v.now()); err != nil || !longEnough(secret) {
		return Value{}, errors.New("the setup key on the page is not a valid TOTP secret")
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := v.open(ctx, fmt.Sprintf("let %s save a one-time code setup for %s in 1Password", who.Name, host)); err != nil {
		return Value{}, err
	}
	p, err := v.placeFor(r.Vault)
	if err != nil {
		return Value{}, err
	}
	it, err := v.item(ctx, p, r.Item)
	if err != nil {
		return Value{}, err
	}
	if !HostMatches(websites(it), host) {
		return Value{}, fmt.Errorf("%q is for %s, not %s", it.Title, strings.Join(websites(it), ", "), host)
	}
	user := ""
	if n := PickField(it, "username", false); n != nil {
		user = n.Value
	}
	as := ""
	if user != "" {
		as = " (" + user + ")"
	}
	reason := fmt.Sprintf("let %s save the 2-step verification for %q%s on %s in your 1Password vault %q", who.Name, it.Title, as, host, p.name)
	if err := v.approveSave(who, host, "2-step code for "+cmpOr(user, it.Title), reason); err != nil {
		return Value{}, err
	}
	field := op.ItemField{ID: "chrome_relay_otp", Title: "one-time password", FieldType: op.ItemFieldTypeTOTP, Value: secret}
	title := it.Title
	if slices.Contains(it.Tags, SavedTag) { // a login Chrome Relay saved: the code goes with it
		it.Fields = append(slices.DeleteFunc(it.Fields, func(f op.ItemField) bool { return f.FieldType == op.ItemFieldTypeTOTP }), field)
		_, err = p.store.Put(ctx, it)
	} else { // the person's own login: an item of its own beside it
		title = it.Title + " one-time code · Chrome Relay"
		notes := fmt.Sprintf("2-step verification for %q, saved by Chrome Relay for %s on %s.", it.Title, who.Name, host)
		_, err = p.store.Create(ctx, op.ItemCreateParams{Category: op.ItemCategoryLogin, VaultID: p.id, Title: title, Tags: []string{SavedTag}, Notes: &notes,
			Fields:   []op.ItemField{{ID: "username", Title: "username", FieldType: op.ItemFieldTypeText, Value: user}, field},
			Websites: it.Websites})
	}
	v.forget()
	if err != nil {
		return Value{}, err
	}
	code, _, _ := TOTP(secret, v.now())
	return Value{code, title, "one-time password", "otp"}, nil
}

// longEnough: a real setup key is at least 80 bits (16 base32 characters); shorter text that happens to be base32
// ("NOTAKEY") is not one.
func longEnough(secret string) bool {
	if strings.HasPrefix(strings.ToLower(secret), "otpauth:") {
		if u, err := url.Parse(secret); err == nil {
			secret = u.Query().Get("secret")
		}
	}
	return len(strings.NewReplacer(" ", "", "-", "", "=", "").Replace(secret)) >= 16
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
