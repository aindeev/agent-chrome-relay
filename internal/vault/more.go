package vault

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"

	op "github.com/1password/onepassword-sdk-go"

	"github.com/aindeev/agent-chrome-relay/internal/device"
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
	Account  string   `json:"account,omitempty"` // the 1Password account it's in
	ID       string   `json:"id"`                // for references when two items share a title
	Title    string   `json:"title"`
	Websites []string `json:"websites"`
	Username string   `json:"username"`
	Password bool     `json:"password"`
	OTP      bool     `json:"otp"`
	Passkey  string   `json:"passkey"`        // its relying party, if Chrome Relay made one
	Card     string   `json:"card,omitempty"` // a Credit Card item: the last four digits of its number
}

// Summarize lists the vault for an agent, never values.
func (v *Vault) Summarize(ctx context.Context, who Who) (Summary, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	names := v.Settings().Vaults()
	if err := v.open(ctx, fmt.Sprintf("show %s which logins are in your 1Password vaults %s (titles and websites, no secrets)", who.Name, quoted(names))); err != nil {
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
		s := ItemSummary{Vault: p.name, Account: p.account, ID: it.ID, Title: it.Title, Websites: websites(it)}
		if it.Category == op.ItemCategoryCreditCard {
			d := regexp.MustCompile(`\D`).ReplaceAllString(cardField(it, func(f op.ItemField) bool {
				return f.FieldType == op.ItemFieldTypeCreditCardNumber || f.ID == "ccnum"
			}), "")
			if len(d) >= 12 {
				s.Card = d[len(d)-4:]
			}
			return s, true
		}
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
		PublicKey: get("public key"), KeyHandle: get("secure enclave key"), Device: get("device"), DeviceID: get("device id"), Item: it.Title, ItemID: it.ID}
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

// Passkeys for a relying party. Reading them needs no approval of its own: signing with one does.
func (v *Vault) Passkeys(ctx context.Context, rpID string, who Who) ([]passkeys.Passkey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := v.open(ctx, fmt.Sprintf("let %s use a passkey for %s from your 1Password vaults %s", who.Name, rpID, quoted(v.Settings().Vaults()))); err != nil {
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
				// the same passkey recorded twice (an older Chrome Relay kept it in the person's item, and a copy
				// moved it to an item of its own) is one passkey
				if pk := PasskeyOf(it); pk != nil && pk.RPID == rpID && v.mine(pk) && !slices.ContainsFunc(out, func(x passkeys.Passkey) bool { return x.CredentialID == pk.CredentialID }) {
					out = append(out, *pk)
				}
			}
		}
	}
	return out, nil
}

// device: this Mac's ID and name.
func (v *Vault) device() (string, string) {
	if v.Device != nil {
		return v.Device()
	}
	return device.ID(), device.Name()
}

// mine: a passkey this Mac can sign with. Its key is in one Mac's Secure Enclave; other Macs sharing the vault
// skip it. Records from before device IDs were kept could only have come from one Mac, so they count.
func (v *Vault) mine(pk *passkeys.Passkey) bool {
	id, _ := v.device()
	return pk.DeviceID == "" || pk.DeviceID == id
}

// PasskeyTag marks the items Chrome Relay made for its passkeys: the only items it ever changes.
const PasskeyTag = "chrome-relay-passkey"

// SavePasskey stores a new passkey in an item of its own, never in the person's items, which Chrome Relay doesn't
// change (rewriting an item through the SDK can lose what the SDK doesn't know, like 1Password's own passkeys).
// It goes into the vault that holds the site's login for that username, so a work account's passkey stays in the
// work account; else the first vault. A passkey Chrome Relay already keeps for that site and username (one the
// site refused, or an older one) is replaced, in its own item.
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
	devID, devName := v.device()
	sec := "chrome_relay_passkey"
	field := func(i int, title string, t op.ItemFieldType, value string) op.ItemField {
		return op.ItemField{ID: fmt.Sprintf("chrome_relay_passkey_%d", i), Title: title, SectionID: &sec, FieldType: t, Value: value}
	}
	fields := []op.ItemField{field(0, "rp id", op.ItemFieldTypeText, pk.RPID), field(1, "credential id", op.ItemFieldTypeText, pk.CredentialID),
		field(2, "user handle", op.ItemFieldTypeText, pk.UserHandle), field(3, "user name", op.ItemFieldTypeText, pk.UserName),
		field(4, "public key", op.ItemFieldTypeText, pk.PublicKey), field(5, "secure enclave key", op.ItemFieldTypeConcealed, pk.KeyHandle),
		field(6, "device", op.ItemFieldTypeText, devName), field(7, "device id", op.ItemFieldTypeText, devID)}
	target := -1
	for i, p := range vaults {
		list, err := v.itemList(ctx, p)
		if err != nil {
			return "", err
		}
		for _, o := range list {
			if !related(overviewURLs(o), pk.RPID) {
				continue
			}
			it, err := v.item(ctx, p, o.ID)
			if err != nil {
				continue
			}
			if slices.Contains(it.Tags, PasskeyTag) { // Chrome Relay's own item: the only kind it replaces
				if old := PasskeyOf(it); old != nil && old.RPID == pk.RPID && old.UserName == pk.UserName && old.DeviceID == devID {
					it.Fields = append(slices.DeleteFunc(it.Fields, func(f op.ItemField) bool { return f.SectionID != nil && *f.SectionID == sec }), fields...)
					saved, err := p.store.Put(ctx, it)
					v.forget()
					return saved.Title, err
				}
				continue
			}
			if target < 0 && o.Category == op.ItemCategoryLogin {
				if u := PickField(it, "username", false); u != nil && u.Value == pk.UserName {
					target = i
				}
			}
		}
	}
	if target < 0 {
		target = 0
	}
	who := pk.UserName
	if who == "" {
		who = "passkey"
	}
	if rpName == "" {
		rpName = pk.RPID
	}
	p := vaults[target]
	saved, err := p.store.Create(ctx, op.ItemCreateParams{Category: op.ItemCategoryLogin, VaultID: p.id,
		Title: PasskeyTitle(rpName, who, devName), Tags: []string{PasskeyTag},
		Sections: []op.ItemSection{{ID: sec, Title: PasskeySection}},
		Fields:   append([]op.ItemField{{ID: "username", Title: "username", FieldType: op.ItemFieldTypeText, Value: pk.UserName}}, fields...),
		Websites: []op.Website{{URL: "https://" + pk.RPID, Label: "website", AutofillBehavior: op.AutofillBehaviorAnywhereOnWebsite}}})
	v.forget()
	return saved.Title, err
}

// PasskeyTitle: one item per site, account and Mac ("GitHub passkey (sam) · Chrome Relay · Sam's MacBook Pro").
func PasskeyTitle(site, user, mac string) string {
	return fmt.Sprintf("%s passkey (%s) · Chrome Relay · %s", site, user, mac)
}

func overviewURLs(o op.ItemOverview) []string {
	urls := make([]string, len(o.Websites))
	for i, w := range o.Websites {
		urls[i] = w.URL
	}
	return urls
}

// SDK is the real 1Password, through the Go SDK, as a service account.
type SDK struct{ C *op.Client }

func Connect(version string) func(ctx context.Context, token string) (Store, error) {
	return func(ctx context.Context, token string) (Store, error) {
		c, err := op.NewClient(ctx, op.WithServiceAccountToken(token), op.WithIntegrationInfo("Chrome Relay", version))
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
