package vault

import (
	"context"
	"fmt"
	"os"
	"strings"

	op "github.com/1password/onepassword-sdk-go"

	"github.com/aindeev/agent-chrome-relay/internal/passkeys"
)

// Summary is what agents may know about the vaults: titles, websites, usernames, which kinds of secret exist.
type Summary struct {
	Vault  string        `json:"vault"` // the vaults, comma-separated (older CLIs print this)
	Vaults []string      `json:"vaults"`
	Items  []ItemSummary `json:"items"`
}
type ItemSummary struct {
	Vault    string   `json:"vault"`
	Account  string   `json:"account,omitempty"` // the 1Password account it's in (desktop app)
	Title    string   `json:"title"`
	Websites []string `json:"websites"`
	Username string   `json:"username"`
	Password bool     `json:"password"`
	OTP      bool     `json:"otp"`
	Passkey  string   `json:"passkey"` // its relying party, if Chrome Relay made one
}

// Summarize lists the vault for an agent, never values. Opening the vault takes Touch ID like anything else.
func (v *Vault) Summarize(ctx context.Context, who Who) (Summary, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	names := v.Settings().Vaults()
	if _, err := v.open(ctx, fmt.Sprintf("show %s which logins are in your 1Password vaults %s (titles and websites, no secrets)", who.Name, quoted(names))); err != nil {
		return Summary{}, err
	}
	out := Summary{Vault: strings.Join(names, ", "), Vaults: names, Items: []ItemSummary{}}
	for _, p := range v.allowed() {
		list, err := v.itemList(ctx, p)
		if err != nil {
			return Summary{}, err
		}
		for _, o := range list {
			if s, ok := v.summarize(ctx, p, o); ok {
				out.Items = append(out.Items, s)
			}
		}
	}
	return out, nil
}

func (v *Vault) summarize(ctx context.Context, p place, o op.ItemOverview) (ItemSummary, bool) {
	{
		it, err := v.item(ctx, p, o.ID)
		if err != nil {
			return ItemSummary{}, false
		}
		s := ItemSummary{Vault: p.name, Account: p.account, Title: it.Title, Websites: websites(it)}
		if f := PickField(it, "username", false); f != nil {
			s.Username = f.Value
		}
		if f := PickField(it, "password", false); f != nil && f.FieldType == op.ItemFieldTypeConcealed && f.Value != "" {
			s.Password = true
		}
		s.OTP = PickField(it, "otp", true) != nil
		if pk := PasskeyOf(it); pk != nil {
			s.Passkey = pk.RPID
		}
		return s, true
	}
}

// PasskeyOf: the passkey Chrome Relay keeps in an item's "Passkey (Chrome Relay)" section, if any. No private key:
// that stays in the Secure Enclave of the Mac named in "device"; the vault has the public key and the chip's handle.
func PasskeyOf(it op.Item) *passkeys.Passkey {
	sec := ""
	for _, s := range it.Sections {
		if s.Title == PasskeySection {
			sec = s.ID
		}
	}
	if sec == "" {
		return nil
	}
	get := func(title string) string {
		for _, f := range it.Fields {
			if f.SectionID != nil && *f.SectionID == sec && f.Title == title {
				return f.Value
			}
		}
		return ""
	}
	pk := passkeys.Passkey{RPID: get("rp id"), CredentialID: get("credential id"), UserHandle: get("user handle"), UserName: get("user name"),
		PublicKey: get("public key"), KeyHandle: get("secure enclave key"), Device: get("device"), Item: it.Title, ItemID: it.ID}
	if pk.RPID == "" || pk.CredentialID == "" || pk.KeyHandle == "" {
		return nil
	}
	return &pk
}

// related: an item on the relying party's domain, either way round (item for login.example.com, passkey for example.com).
func related(urls []string, rpID string) bool {
	if HostMatches(urls, rpID) {
		return true
	}
	for _, u := range urls {
		if HostMatches([]string{rpID}, HostOf(u)) {
			return true
		}
	}
	return false
}

// Passkeys for a relying party. Reading them needs no approval of its own: signing with one does (Touch ID).
func (v *Vault) Passkeys(ctx context.Context, rpID string, who Who) ([]passkeys.Passkey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if _, err := v.open(ctx, fmt.Sprintf("let %s use a passkey for %s from your 1Password vaults %s", who.Name, rpID, quoted(v.Settings().Vaults()))); err != nil {
		return nil, err
	}
	var out []passkeys.Passkey
	for _, p := range v.allowed() {
		list, err := v.itemList(ctx, p)
		if err != nil {
			return nil, err
		}
		for _, o := range list {
			if !related(overviewURLs(o), rpID) {
				continue
			}
			if it, err := v.item(ctx, p, o.ID); err == nil {
				if pk := PasskeyOf(it); pk != nil && pk.RPID == rpID {
					out = append(out, *pk)
				}
			}
		}
	}
	return out, nil
}

// SavePasskey stores a new passkey: on the site's existing Login item for that username, or a new Login item.
func (v *Vault) SavePasskey(ctx context.Context, pk passkeys.Passkey, rpName string) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.open_ {
		return "", fmt.Errorf("1Password is not open")
	}
	vaults := v.allowed()
	if len(vaults) == 0 {
		return "", v.notSetUp()
	}
	host, _ := os.Hostname()
	sec := "chrome_relay_passkey"
	field := func(i int, title string, t op.ItemFieldType, value string) op.ItemField {
		return op.ItemField{ID: fmt.Sprintf("chrome_relay_passkey_%d", i), Title: title, SectionID: &sec, FieldType: t, Value: value}
	}
	fields := []op.ItemField{field(0, "rp id", op.ItemFieldTypeText, pk.RPID), field(1, "credential id", op.ItemFieldTypeText, pk.CredentialID),
		field(2, "user handle", op.ItemFieldTypeText, pk.UserHandle), field(3, "user name", op.ItemFieldTypeText, pk.UserName),
		field(4, "public key", op.ItemFieldTypeText, pk.PublicKey), field(5, "secure enclave key", op.ItemFieldTypeConcealed, pk.KeyHandle),
		field(6, "device", op.ItemFieldTypeText, host)}
	section := op.ItemSection{ID: sec, Title: PasskeySection}
	// on the site's existing login for that username, in whichever allowed vault has it
	for _, p := range vaults {
		list, err := v.itemList(ctx, p)
		if err != nil {
			return "", err
		}
		for _, o := range list {
			if o.Category != op.ItemCategoryLogin || !related(overviewURLs(o), pk.RPID) {
				continue
			}
			it, err := v.item(ctx, p, o.ID)
			if err != nil || PasskeyOf(it) != nil {
				continue
			}
			if u := PickField(it, "username", false); u != nil && u.Value == pk.UserName {
				it.Sections = append(it.Sections, section)
				it.Fields = append(it.Fields, fields...)
				saved, err := p.store.Put(ctx, it)
				v.forget()
				return saved.Title, err
			}
		}
	}
	// else a new Login item in the first allowed vault
	title := pk.UserName
	if title == "" {
		title = "passkey"
	}
	if rpName == "" {
		rpName = pk.RPID
	}
	saved, err := vaults[0].store.Create(ctx, op.ItemCreateParams{Category: op.ItemCategoryLogin, VaultID: vaults[0].id, Title: fmt.Sprintf("%s (%s)", rpName, title),
		Sections: []op.ItemSection{section},
		Fields:   append([]op.ItemField{{ID: "username", Title: "username", FieldType: op.ItemFieldTypeText, Value: pk.UserName}}, fields...),
		Websites: []op.Website{{URL: "https://" + pk.RPID, Label: "website", AutofillBehavior: op.AutofillBehaviorAnywhereOnWebsite}}})
	v.forget()
	return saved.Title, err
}

func overviewURLs(o op.ItemOverview) []string {
	urls := make([]string, len(o.Websites))
	for i, w := range o.Websites {
		urls[i] = w.URL
	}
	return urls
}

// SDK is the real 1Password, through the Go SDK: the desktop app as one of your accounts, or a service account.
type SDK struct{ C *op.Client }

func Connect(version string) func(ctx context.Context, account, token string) (Store, error) {
	return func(ctx context.Context, account, token string) (Store, error) {
		auth := op.WithServiceAccountToken(token)
		if account != "" {
			auth = op.WithDesktopAppIntegration(account) // 1Password asks the person to allow Chrome Relay
		}
		c, err := op.NewClient(ctx, auth, op.WithIntegrationInfo("Chrome Relay", version))
		if err != nil {
			return nil, err
		}
		return SDK{c}, nil
	}
}
func (s SDK) Vaults(ctx context.Context) ([]op.VaultOverview, error) { return s.C.Vaults().List(ctx) }
func (s SDK) List(ctx context.Context, vaultID string) ([]op.ItemOverview, error) {
	return s.C.Items().List(ctx, vaultID)
}
func (s SDK) Get(ctx context.Context, vaultID, itemID string) (op.Item, error) {
	return s.C.Items().Get(ctx, vaultID, itemID)
}
func (s SDK) Create(ctx context.Context, p op.ItemCreateParams) (op.Item, error) {
	return s.C.Items().Create(ctx, p)
}
func (s SDK) Put(ctx context.Context, it op.Item) (op.Item, error) { return s.C.Items().Put(ctx, it) }
