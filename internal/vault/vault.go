// Package vault gives agents logins from 1Password without ever showing them a secret: the relay reads the
// vaults the person allowed and types values straight into pages, only on each item's own website.
//
// The way in is 1Password service accounts, one per 1Password account, each able to open one shared vault; their
// tokens are stored locked to this Mac's Secure Enclave in Keychain items only Chrome Relay reads (opened without
// a prompt), so they work in the background and while the person is away. Chrome Relay's own approval, naming
// the agent, item and site, approves sign-ins (per sign-in, once per agent session, or off): Touch ID here, or
// Face ID on a paired iPhone, whichever answers first.
package vault

import (
	"cmp"
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	op "github.com/1password/onepassword-sdk-go"

	"github.com/aindeev/agent-chrome-relay/internal/phone"
)

// PasskeySection is the section of a Login item where Chrome Relay keeps a passkey it made.
const PasskeySection = "Passkey (Chrome Relay)"

// Ref: the whole typed text is a secret reference, op://<vault>/<item>/[<section>/]<field>[?attribute=otp].
// Vault and item names may contain spaces.
var Ref = regexp.MustCompile(`(?i)^op://([^/\s][^/]*)/([^/\s][^/]*)/(?:[^/]+/)?([^/?]+)(?:\?attribute=(otp|totp))?$`)

// Store is 1Password, as the relay uses it (the SDK client, or a test double).
type Store interface {
	Vaults(ctx context.Context) ([]op.VaultOverview, error)
	List(ctx context.Context, vaultID string) ([]op.ItemOverview, error)
	Get(ctx context.Context, vaultID, itemID string) (op.Item, error)
	Create(ctx context.Context, p op.ItemCreateParams) (op.Item, error)
	Put(ctx context.Context, item op.Item) (op.Item, error)
}

// Source: a service account and the vault agents may use through it.
type Source struct {
	Vaults []string // its one vault
	Key    string   // its token's Keychain key (its 1Password account's UUID; "" = the older, single one)
	Label  string   // its 1Password account, for people ("you@example.com")
}

// Settings: how agents reach 1Password ("onepassword" in config.json). Several accounts at once are fine; the
// vault names agents may use must be unique across them (references name only the vault). How often Chrome
// Relay asks is kept with each token, not here: config.json is a file agents could edit.
type Settings struct {
	Sources []Source
}

// Vaults agents may use, in configured order.
func (s Settings) Vaults() []string {
	var out []string
	for _, src := range s.Sources {
		out = append(out, src.Vaults...)
	}
	return out
}
func (s Settings) key() string { return fmt.Sprintf("%q", s.Sources) }

// Person is who approves: Unlock opens the stored service account token (silently, but for one Touch ID for a
// token an older Chrome Relay saved), Approve asks Touch ID here or Face ID on the paired iPhone.
// A reason completes the sentence "chrome-relay is trying to ...".
type Person interface {
	Unlock(key, reason string) (Token, error)
	Approve(d phone.Details) error
}

// Vault is 1Password as agents see it, connected on first use.
type Vault struct {
	Settings func() Settings
	Connect  func(ctx context.Context, token string) (Store, error) // an SDK client for a service account token
	Person   Person
	Now      func() time.Time
	Stamp    func() string            // identifies the stored service account token: a new or removed one reconnects
	CLI      string                   // the command people run, for messages ("chrome-relay" or "chrome-relay-dev-mode")
	Device   func() (id, name string) // this Mac, for its passkeys: several Macs may share the vaults (default: package device)

	mu           sync.Mutex
	open_        bool
	places       map[string]place // allowed vault name (lower case) -> where it is
	opened       string           // settings and token stamp it was opened with
	approvals    map[string]time.Time
	tokenApprove string            // how often to ask, as stored with the service account token
	lists        map[string]listed // vault id -> its items, briefly
	items        map[string]cached // item id -> item, briefly
}

// place: an allowed vault, in the account that has it.
type place struct {
	store    Store
	id, name string
	account  string
}

type listed struct {
	items []op.ItemOverview
	at    time.Time
}
type cached struct {
	item op.Item
	at   time.Time
}

// Value is what a reference resolved to, for one page.
type Value struct {
	Value, Item, Field string
	Kind               string // "secret" (concealed: password fields only), "otp" or "text"
}

func (v *Vault) now() time.Time {
	if v.Now != nil {
		return v.Now()
	}
	return time.Now()
}

func (v *Vault) notSetUp() error {
	cli := v.CLI
	if cli == "" {
		cli = "chrome-relay"
	}
	return fmt.Errorf("1Password is not set up for agents: ask the person to run `%s vault setup`", cli)
}

// Configured reports whether any vault is set up.
func (v *Vault) Configured() bool { return len(v.Settings().Vaults()) > 0 }

func quoted(names []string) string {
	q := make([]string, len(names))
	for i, n := range names {
		q[i] = strconv.Quote(n)
	}
	return strings.Join(q, ", ")
}

// open connects on first use, and again when the settings or the stored token change. reason is for the one
// Touch ID a service account token saved by an older Chrome Relay needs, before it is saved without.
func (v *Vault) open(ctx context.Context, reason string) (err error) {
	s := v.Settings()
	stamp := s.key()
	if v.Stamp != nil {
		stamp += "|" + v.Stamp()
	}
	if v.open_ && stamp == v.opened {
		return nil
	}
	v.open_, v.places, v.approvals, v.lists, v.items = false, nil, nil, nil, nil // new settings or token, or none any more
	if len(s.Vaults()) == 0 {
		return v.notSetUp()
	}
	seen := map[string]bool{}
	for _, n := range s.Vaults() {
		if seen[strings.ToLower(n)] {
			return fmt.Errorf("two allowed vaults are named %q: references name only the vault, so allow just one", n)
		}
		seen[strings.ToLower(n)] = true
	}
	places := map[string]place{}
	v.tokenApprove = ""
	for _, src := range s.Sources {
		t, err := v.Person.Unlock(src.Key, reason)
		if err != nil {
			return err
		}
		if strictness[t.Approve] >= strictness[v.tokenApprove] { // several service accounts: the strictest setting
			v.tokenApprove = t.Approve
		}
		v.mu.Unlock() // 1Password's servers take a moment
		store, err := v.Connect(ctx, t.Value)
		var vaults []op.VaultOverview
		if err == nil {
			vaults, err = store.Vaults(ctx)
		}
		v.mu.Lock()
		where := "the service account"
		if src.Label != "" {
			where += " in " + src.Label
		}
		if err != nil {
			return fmt.Errorf("could not reach %s: %w", where, err)
		}
		for _, want := range src.Vaults {
			i := slices.IndexFunc(vaults, func(x op.VaultOverview) bool { return strings.EqualFold(x.Title, want) })
			if i < 0 {
				return fmt.Errorf("%s has no vault %q that Chrome Relay can open", where, want)
			}
			places[strings.ToLower(want)] = place{store, vaults[i].ID, vaults[i].Title, src.Label}
		}
	}
	v.open_, v.places, v.opened = true, places, stamp
	return nil
}

var strictness = map[string]int{"": -1, "off": 0, "session": 1, "sign-in": 2}

// signInWindow: how long one approval lets an agent use an item (any of its fields, on any of its websites).
const signInWindow = 2 * time.Minute

// approve asks the person, per the settings: once per sign-in (that agent and item: its username, password and
// one-time code, on any of the item's websites, for two minutes), once per agent session, or not at all.
func (v *Vault) approve(who Who, it op.Item, host, what, reason string) error {
	if v.approvals == nil {
		v.approvals = map[string]time.Time{}
	}
	mode := cmp.Or(v.tokenApprove, "sign-in") // kept with the tokens, not in config.json (which agents could edit)
	if mode == "off" {
		return nil
	}
	key, ttl := who.Session+"|"+it.ID, signInWindow
	if mode == "session" {
		key, ttl, what = who.Session, 12*time.Hour, "sign-ins this session, starting with "+what
		reason = fmt.Sprintf("let %s sign in with logins from your 1Password vaults %s for the rest of its session, starting with: %s",
			who.Name, quoted(v.Settings().Vaults()), strings.TrimPrefix(reason, "let "+who.Name+" "))
	} else { // one approval for the whole sign-in: the person sees the site, the account and the item, once
		as := ""
		if n := PickField(it, "username", false); n != nil && n.Value != "" {
			as = " as " + n.Value
		}
		what = "sign-in" + as
		reason = fmt.Sprintf("let %s sign in to %s%s with %q from 1Password (username, password and one-time code, for %d minutes)",
			who.Name, host, as, it.Title, int(signInWindow/time.Minute))
	}
	if !v.approvals[key].After(v.now()) {
		v.mu.Unlock()
		err := v.Person.Approve(phone.Details{Reason: reason, Agent: who.Name, Site: host, What: what})
		v.mu.Lock()
		if err != nil {
			return err
		}
		v.approvals[key] = v.now().Add(ttl) // from the approval: using the item doesn't extend it
	} else if mode == "session" {
		v.approvals[key] = v.now().Add(ttl)
	}
	return nil
}

// Short caches: a sign-in fills two or three fields in a row, and 1Password has rate limits.
func (v *Vault) itemList(ctx context.Context, p place) ([]op.ItemOverview, error) {
	key := p.account + "/" + p.id // caches are per account: ids from two accounts never mix
	if l, ok := v.lists[key]; ok && v.now().Sub(l.at) < 30*time.Second {
		return l.items, nil
	}
	items, err := p.store.List(ctx, p.id)
	if err != nil {
		return nil, err
	}
	if v.lists == nil {
		v.lists = map[string]listed{}
	}
	v.lists[key] = listed{items, v.now()}
	return items, nil
}

func (v *Vault) item(ctx context.Context, p place, nameOrID string) (op.Item, error) {
	list, err := v.itemList(ctx, p)
	if err != nil {
		return op.Item{}, err
	}
	id := ""
	for _, o := range list {
		if o.ID == nameOrID {
			id = o.ID
		}
	}
	if id == "" {
		var same []op.ItemOverview
		for _, o := range list {
			if strings.EqualFold(o.Title, nameOrID) {
				same = append(same, o)
			}
		}
		switch len(same) {
		case 0:
			return op.Item{}, fmt.Errorf("there is no item %q in the vault %q", nameOrID, p.name)
		case 1:
			id = same[0].ID
		default: // never guess which account: the wrong one's password would be typed
			var which []string
			for _, o := range same {
				who := "(no username)"
				if it, err := v.item(ctx, p, o.ID); err == nil {
					if f := PickField(it, "username", false); f != nil && f.Value != "" {
						who = f.Value
					}
				}
				which = append(which, fmt.Sprintf("%s -> op://%s/%s/<field>", who, p.name, o.ID))
			}
			return op.Item{}, fmt.Errorf("%d items are titled %q in the vault %q. Use the one for the account you are signing in as, by its id in place of the title: %s", len(same), nameOrID, p.name, strings.Join(which, "; "))
		}
	}
	if c, ok := v.items[p.account+"/"+id]; ok && v.now().Sub(c.at) < 30*time.Second {
		return c.item, nil
	}
	it, err := p.store.Get(ctx, p.id, id)
	if err != nil {
		return op.Item{}, err
	}
	if v.items == nil {
		v.items = map[string]cached{}
	}
	v.items[p.account+"/"+id] = cached{it, v.now()}
	return it, nil
}

func (v *Vault) forget() { v.lists, v.items = nil, nil }

// allowed vaults, in configured order (after open).
func (v *Vault) allowed() []place {
	var out []place
	for _, n := range v.Settings().Vaults() {
		if p, ok := v.places[strings.ToLower(n)]; ok {
			out = append(out, p)
		}
	}
	return out
}

// Who describes the agent asking, for prompts: "Claude Code (my-repo)". Session keys approvals.
type Who struct{ Name, Session string }

// Resolve: the value a reference stands for, if this page may have it, after the person's approval.
// Errors are messages for the agent.
func (v *Vault) Resolve(ctx context.Context, ref, pageURL string, who Who) (Value, error) {
	m := Ref.FindStringSubmatch(strings.TrimSpace(ref))
	if m == nil {
		return Value{}, errors.New("not a 1Password secret reference")
	}
	vaultName, itemName, fieldName, attribute := m[1], unescape(m[2]), unescape(m[3]), m[4]
	s := v.Settings()
	if len(s.Vaults()) == 0 {
		return Value{}, v.notSetUp()
	}
	if !slices.ContainsFunc(s.Vaults(), func(n string) bool { return strings.EqualFold(n, vaultName) }) {
		return Value{}, fmt.Errorf("only the vaults %s are available to agents, not %q", quoted(s.Vaults()), vaultName)
	}
	u, err := url.Parse(pageURL)
	if err != nil || !SecureOrigin(u) {
		return Value{}, fmt.Errorf("secrets are only filled into https pages, not %s", pageURL)
	}
	what := fieldName
	if attribute != "" || otpNames[strings.ToLower(fieldName)] {
		what = "one-time code"
	}
	reason := fmt.Sprintf("let %s fill the %s of %q on %s", who.Name, what, itemName, u.Hostname())

	v.mu.Lock()
	defer v.mu.Unlock()
	if err := v.open(ctx, reason); err != nil {
		return Value{}, err
	}
	it, err := v.item(ctx, v.places[strings.ToLower(vaultName)], itemName)
	if err != nil {
		return Value{}, err
	}
	if len(it.Websites) == 0 {
		return Value{}, fmt.Errorf("%q has no website, so it can't be filled anywhere: add one to the item in 1Password", it.Title)
	}
	if !HostMatches(websites(it), u.Hostname()) {
		return Value{}, fmt.Errorf("%q is for %s, not %s", it.Title, strings.Join(websites(it), ", "), u.Hostname())
	}
	f := PickField(it, fieldName, attribute != "")
	if f == nil {
		return Value{}, fmt.Errorf("%q has no field %q", it.Title, fieldName)
	}
	if !strings.EqualFold(itemName, it.Title) { // referred to by id: the person is asked by title and account, never an id
		label := fmt.Sprintf("%q", it.Title)
		if n := PickField(it, "username", false); n != nil && n.Value != "" {
			label += " (" + n.Value + ")"
		}
		reason = fmt.Sprintf("let %s fill the %s of %s on %s", who.Name, what, label, u.Hostname())
	}
	if err := v.approve(who, it, u.Hostname(), what, reason); err != nil {
		return Value{}, err
	}
	switch f.FieldType {
	case op.ItemFieldTypeTOTP:
		code, remaining, err := TOTP(f.Value, v.now())
		if err != nil {
			if f.Details != nil && f.Details.OTP() != nil && f.Details.OTP().Code != nil {
				return Value{*f.Details.OTP().Code, it.Title, fieldTitle(f), "otp"}, nil
			}
			return Value{}, err
		}
		if remaining < 3 { // don't type a code that expires mid-submit
			v.mu.Unlock()
			time.Sleep(time.Duration(remaining)*time.Second + 200*time.Millisecond)
			v.mu.Lock()
			code, _, _ = TOTP(f.Value, v.now())
		}
		return Value{code, it.Title, fieldTitle(f), "otp"}, nil
	case op.ItemFieldTypeConcealed:
		if f.Value == "" {
			return Value{}, fmt.Errorf("%q has an empty %q", it.Title, fieldTitle(f))
		}
		return Value{f.Value, it.Title, fieldTitle(f), "secret"}, nil
	}
	if f.Value == "" {
		return Value{}, fmt.Errorf("%q has an empty %q", it.Title, fieldTitle(f))
	}
	return Value{f.Value, it.Title, fieldTitle(f), "text"}, nil
}

func unescape(s string) string {
	if u, err := url.PathUnescape(s); err == nil {
		return u
	}
	return s
}

func fieldTitle(f *op.ItemField) string {
	if f.Title != "" {
		return f.Title
	}
	return f.ID
}

func websites(it op.Item) []string {
	out := make([]string, len(it.Websites))
	for i, w := range it.Websites {
		out[i] = w.URL
	}
	return out
}

var otpNames = map[string]bool{"otp": true, "totp": true, "one-time password": true, "one-time-password": true, "onetimepassword": true, "code": true}

// PickField: the field a reference names. username / password are the item's built-in fields (ids "username",
// "password", whatever their labels), otp is its one-time password, anything else matches an id or label.
func PickField(it op.Item, name string, otp bool) *op.ItemField {
	n := strings.ToLower(name)
	find := func(ok func(f op.ItemField) bool) *op.ItemField {
		for i := range it.Fields {
			if ok(it.Fields[i]) {
				return &it.Fields[i]
			}
		}
		return nil
	}
	if otp || otpNames[n] {
		if f := find(func(f op.ItemField) bool {
			return f.FieldType == op.ItemFieldTypeTOTP && (f.ID == name || strings.EqualFold(f.Title, name))
		}); f != nil {
			return f
		}
		return find(func(f op.ItemField) bool { return f.FieldType == op.ItemFieldTypeTOTP })
	}
	if n == "username" || n == "password" {
		if f := find(func(f op.ItemField) bool { return f.ID == n && f.SectionID == nil }); f != nil {
			return f
		}
	}
	if f := find(func(f op.ItemField) bool { return f.ID == name }); f != nil {
		return f
	}
	return find(func(f op.ItemField) bool { return strings.EqualFold(f.Title, name) })
}

// HostMatches: is host one of the item's websites, or a subdomain of one? Like 1Password's own filling.
func HostMatches(urls []string, host string) bool {
	host = strings.ToLower(host)
	for _, raw := range urls {
		h := HostOf(raw)
		h = strings.TrimPrefix(h, "www.")
		if h != "" && (host == h || strings.HasSuffix(host, "."+h)) {
			return true
		}
	}
	return false
}

// HostOf a website as 1Password stores it ("https://github.com/login" or just "github.com").
func HostOf(raw string) string {
	if !regexp.MustCompile(`(?i)^[a-z][a-z0-9+.-]*://`).MatchString(raw) {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// SecureOrigin: pages where secrets may be typed and passkeys answered: https, or this Mac.
func SecureOrigin(u *url.URL) bool {
	switch u.Hostname() {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return u.Scheme == "https"
}

// TOTP: RFC 6238 code from an otpauth:// URI or a bare base32 secret, and the seconds it stays valid.
func TOTP(value string, now time.Time) (string, int, error) {
	secret, digits, period, algo := value, 6, 30, "sha1"
	if strings.HasPrefix(strings.ToLower(value), "otpauth:") {
		u, err := url.Parse(value)
		if err != nil {
			return "", 0, errors.New("the one-time password field holds no valid TOTP secret")
		}
		q := u.Query()
		secret = q.Get("secret")
		if d, _ := strconv.Atoi(q.Get("digits")); d > 0 {
			digits = d
		}
		if p, _ := strconv.Atoi(q.Get("period")); p > 0 {
			period = p
		}
		if a := q.Get("algorithm"); a != "" {
			algo = strings.ToLower(a)
		}
	}
	clean := strings.ToUpper(strings.NewReplacer(" ", "", "-", "", "=", "").Replace(secret))
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(clean)
	if err != nil || len(key) == 0 {
		return "", 0, errors.New("the one-time password field holds no valid TOTP secret")
	}
	h := map[string]func() hash.Hash{"sha1": sha1.New, "sha256": sha256.New, "sha512": sha512.New}[algo]
	if h == nil {
		return "", 0, fmt.Errorf("unsupported TOTP algorithm %q", algo)
	}
	secs := now.Unix()
	msg := make([]byte, 8)
	binary.BigEndian.PutUint64(msg, uint64(secs/int64(period)))
	mac := hmac.New(h, key)
	mac.Write(msg)
	sum := mac.Sum(nil)
	o := sum[len(sum)-1] & 15
	n := (binary.BigEndian.Uint32(sum[o:]) & 0x7fffffff) % uint32(pow10(digits))
	return fmt.Sprintf("%0*d", digits, n), period - int(secs%int64(period)), nil
}

func pow10(n int) int {
	p := 1
	for range n {
		p *= 10
	}
	return p
}
