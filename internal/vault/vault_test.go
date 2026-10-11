package vault

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	op "github.com/1password/onepassword-sdk-go"

	"github.com/aindeev/agent-chrome-relay/internal/passkeys"
)

// fakeStore is 1Password in memory; fakePerson records every Touch ID prompt and can decline.
type fakeStore struct {
	vault string
	items []op.Item
}

func (s *fakeStore) Vaults(context.Context) ([]op.VaultOverview, error) {
	return []op.VaultOverview{{ID: "v1", Title: s.vault}}, nil
}
func (s *fakeStore) List(context.Context, string) ([]op.ItemOverview, error) {
	var out []op.ItemOverview
	for _, it := range s.items {
		out = append(out, op.ItemOverview{ID: it.ID, Title: it.Title, Category: it.Category, VaultID: "v1", Websites: it.Websites})
	}
	return out, nil
}
func (s *fakeStore) Get(_ context.Context, _, id string) (op.Item, error) {
	for _, it := range s.items {
		if it.ID == id {
			return it, nil
		}
	}
	return op.Item{}, errors.New("no such item")
}
func (s *fakeStore) Create(_ context.Context, p op.ItemCreateParams) (op.Item, error) {
	it := op.Item{ID: fmt.Sprintf("new%d", len(s.items)), Title: p.Title, Category: p.Category, Fields: p.Fields, Sections: p.Sections, Websites: p.Websites}
	s.items = append(s.items, it)
	return it, nil
}
func (s *fakeStore) Put(_ context.Context, it op.Item) (op.Item, error) {
	for i := range s.items {
		if s.items[i].ID == it.ID {
			s.items[i] = it
		}
	}
	return it, nil
}

type fakePerson struct {
	prompts []string
	decline bool
}

func (p *fakePerson) Unlock(reason string) (string, error) {
	p.prompts = append(p.prompts, "unlock: "+reason)
	if p.decline {
		return "", errors.New("the person did not approve with Touch ID")
	}
	return "ops_test", nil
}
func (p *fakePerson) Approve(reason string) error {
	p.prompts = append(p.prompts, "approve: "+reason)
	if p.decline {
		return errors.New("the person did not approve with Touch ID")
	}
	return nil
}

func fixture() (*Vault, *fakeStore, *fakePerson, *time.Time) {
	store := &fakeStore{vault: "Shared with Chrome Relay", items: []op.Item{
		{ID: "gh1", Title: "GitHub", Category: op.ItemCategoryLogin, Websites: []op.Website{{URL: "https://github.com"}}, Fields: []op.ItemField{
			{ID: "username", Title: "username", FieldType: op.ItemFieldTypeText, Value: "agent-bot"},
			{ID: "password", Title: "passwd", FieldType: op.ItemFieldTypeConcealed, Value: "hunter2-secret"},
			{ID: "", Title: "password", FieldType: op.ItemFieldTypeText, Value: "old-web-form-value"}, // leftover web form field, as some items have
			{ID: "otp1", Title: "one-time password", FieldType: op.ItemFieldTypeTOTP, Value: "otpauth://totp/GitHub:agent-bot?secret=JBSWY3DPEHPK3PXP&issuer=GitHub"}}},
		{ID: "nw1", Title: "No website", Category: op.ItemCategoryLogin, Fields: []op.ItemField{{ID: "password", FieldType: op.ItemFieldTypeConcealed, Value: "x"}}},
	}}
	person := &fakePerson{}
	now := time.Unix(1_800_000_010, 0)
	v := &Vault{Settings: func() Settings { return Settings{Sources: []Source{{Vaults: []string{"Shared with Chrome Relay"}}}} },
		Person: person, Now: func() time.Time { return now }, Connect: func(context.Context, string, string) (Store, error) { return store, nil }}
	return v, store, person, &now
}

var claude = Who{Name: "Claude Code (my-repo)", Session: "s1"}

func TestTOTPMatchesRFC6238(t *testing.T) {
	uri := "otpauth://totp/x?secret=GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ&digits=8"
	for _, c := range []struct {
		t    int64
		code string
	}{{59, "94287082"}, {1111111109, "07081804"}, {20000000000, "65353130"}} {
		if got, _, _ := TOTP(uri, time.Unix(c.t, 0)); got != c.code {
			t.Errorf("TOTP at %d = %s, want %s", c.t, got, c.code)
		}
	}
	if got, rem, _ := TOTP("gezd gnbv gy3t qojq gezd gnbv gy3t qojq", time.Unix(59, 0)); got != "287082" || rem != 1 {
		t.Errorf("bare secret: %s %d", got, rem)
	}
	if _, _, err := TOTP("not base32!", time.Now()); err == nil {
		t.Error("bad secret accepted")
	}
}

func TestSiteMatching(t *testing.T) {
	urls := []string{"https://github.com/login", "www.example.org"}
	for _, h := range []string{"github.com", "gist.github.com", "example.org", "app.example.org"} {
		if !HostMatches(urls, h) {
			t.Error("should match", h)
		}
	}
	for _, h := range []string{"evilgithub.com", "github.com.evil.io", "com", "example.com", ""} {
		if HostMatches(urls, h) {
			t.Error("should not match", h)
		}
	}
	if m := Ref.FindStringSubmatch("op://Shared with Chrome Relay/Example/password"); m == nil || m[1] != "Shared with Chrome Relay" || m[2] != "Example" {
		t.Error("vault names with spaces", m)
	}
	if Ref.MatchString("hello op://A/B/password") || Ref.MatchString("op://A/B") {
		t.Error("not references")
	}
}

func TestResolveFillsOnlyTheItemsSiteAndAsksOncePerSignIn(t *testing.T) {
	v, _, person, now := fixture()
	ctx := context.Background()
	got, err := v.Resolve(ctx, "op://Shared with Chrome Relay/GitHub/password", "https://github.com/login", claude)
	if err != nil || got.Value != "hunter2-secret" || got.Kind != "secret" {
		t.Fatalf("password: %+v %v (the item's built-in password, not a leftover web-form field)", got, err)
	}
	if len(person.prompts) != 1 || person.prompts[0] != `unlock: let Claude Code (my-repo) fill the password of "GitHub" on github.com` {
		t.Fatalf("first use opens the vault with the request as the reason: %q", person.prompts)
	}
	if got, _ := v.Resolve(ctx, "op://shared with chrome relay/github/username", "https://github.com/login", claude); got.Value != "agent-bot" {
		t.Fatal("username", got)
	}
	otp, err := v.Resolve(ctx, "op://Shared with Chrome Relay/GitHub/otp", "https://github.com/sessions/two-factor", claude)
	if want, _, _ := TOTP("JBSWY3DPEHPK3PXP", *now); err != nil || otp.Value != want || otp.Kind != "otp" {
		t.Fatalf("otp %+v %v", otp, err)
	}
	if len(person.prompts) != 1 {
		t.Fatalf("one sign-in, one Touch ID: %q", person.prompts)
	}
	*now = now.Add(2 * time.Minute)
	v.Resolve(ctx, "op://Shared with Chrome Relay/GitHub/password", "https://github.com/login", claude)
	if len(person.prompts) != 2 || !strings.HasPrefix(person.prompts[1], "approve: let Claude Code (my-repo) fill the password") {
		t.Fatalf("a later sign-in asks again: %q", person.prompts)
	}
	for ref, page := range map[string]string{
		"op://Shared with Chrome Relay/GitHub/password":     "https://evil.example/login", // is for https://github.com, not evil.example
		"op://Private/GitHub/password":                      "https://github.com/login",   // only the shared vault
		"op://Shared with Chrome Relay/No website/password": "https://github.com/",        // has no website
		"op://Shared with Chrome Relay/GitHub/recovery":     "https://github.com/",        // has no field
		"op://Shared with Chrome Relay/GitHub/username":     "http://github.com/",         // only https
	} {
		if _, err := v.Resolve(ctx, ref, page, claude); err == nil {
			t.Errorf("%s on %s should be refused", ref, page)
		}
	}
	person.decline = true
	other := Who{Name: "Codex (x)", Session: "s2"}
	if _, err := v.Resolve(ctx, "op://Shared with Chrome Relay/GitHub/password", "https://github.com/login", other); err == nil {
		t.Error("a declined Touch ID must refuse")
	}
}

func TestSummaryHasNoSecretsAndPasskeysAreStored(t *testing.T) {
	v, store, _, _ := fixture()
	ctx := context.Background()
	sum, err := v.Summarize(ctx, claude)
	if err != nil {
		t.Fatal(err)
	}
	if s := fmt.Sprint(sum); strings.Contains(s, "hunter2") || strings.Contains(s, "JBSWY3DP") {
		t.Fatal("summary carries a secret", s)
	}
	if gh := sum.Items[0]; gh.Title != "GitHub" || gh.Username != "agent-bot" || !gh.Password || !gh.OTP || gh.Passkey != "" {
		t.Fatalf("%+v", gh)
	}
	pk := func(rp, user string) passkeys.Passkey {
		return passkeys.Passkey{RPID: rp, CredentialID: "cred-" + rp, UserHandle: "AQID", UserName: user, PublicKey: "PUB", KeyHandle: "CHIP-" + rp}
	}
	if title, _ := v.SavePasskey(ctx, pk("github.com", "agent-bot"), ""); title != "GitHub" {
		t.Fatal("same site and username: added to that item, got", title)
	}
	if title, _ := v.SavePasskey(ctx, pk("webauthn.example", "bot"), "Demo"); title != "Demo (bot)" {
		t.Fatal("else a new Login item, got", title)
	}
	if store.items[0].Fields[1].Value != "hunter2-secret" || PasskeyOf(store.items[0]).RPID != "github.com" {
		t.Fatal("the item keeps its fields and gains the passkey")
	}
	found, _ := v.Passkeys(ctx, "webauthn.example", claude)
	if len(found) != 1 || found[0].KeyHandle != "CHIP-webauthn.example" || found[0].Device == "" {
		t.Fatalf("%+v", found)
	}
	for _, it := range store.items {
		for _, f := range it.Fields {
			if strings.Contains(strings.ToLower(f.Title), "private") {
				t.Fatal("no private key is ever stored")
			}
		}
	}
	if none, _ := v.Passkeys(ctx, "example.org", claude); len(none) != 0 {
		t.Fatal(none)
	}
}

func TestANewTokenReconnectsAndNoneStopsFilling(t *testing.T) {
	v, _, person, _ := fixture()
	stamp := "token-1"
	v.Stamp = func() string { return stamp }
	ctx := context.Background()
	ref, page := "op://Shared with Chrome Relay/GitHub/username", "https://github.com/login"
	v.Resolve(ctx, ref, page, claude)
	v.Resolve(ctx, ref, page, claude)
	if len(person.prompts) != 1 {
		t.Fatal("same token: opened once", person.prompts)
	}
	stamp = "token-2" // `vault setup` stored a new token
	v.Resolve(ctx, ref, page, claude)
	if len(person.prompts) != 2 || !strings.HasPrefix(person.prompts[1], "unlock:") {
		t.Fatal("a new token is opened again (Touch ID)", person.prompts)
	}
	v.Settings = func() Settings { return Settings{} } // `vault forget`
	if _, err := v.Resolve(ctx, ref, page, claude); err == nil || !strings.Contains(err.Error(), "vault setup") {
		t.Fatal("no vault any more", err)
	}
}

func TestDesktopAppTwoAccountsOneApprovalPerAgentSession(t *testing.T) {
	personal := &fakeStore{vault: "Private", items: []op.Item{{ID: "tw1", Title: "Twilio (Sam - Acme)", Category: op.ItemCategoryLogin,
		Websites: []op.Website{{URL: "https://www.twilio.com"}}, Fields: []op.ItemField{
			{ID: "username", Title: "username", FieldType: op.ItemFieldTypeText, Value: "sam@acme.com"},
			{ID: "password", Title: "password", FieldType: op.ItemFieldTypeConcealed, Value: "twilio-secret"}}}}}
	work := &fakeStore{vault: "Employee", items: []op.Item{{ID: "gh2", Title: "GitHub", Category: op.ItemCategoryLogin,
		Websites: []op.Website{{URL: "https://github.com"}}, Fields: []op.ItemField{{ID: "password", FieldType: op.ItemFieldTypeConcealed, Value: "work-secret"}}}}}
	person := &fakePerson{}
	var connected []string
	v := &Vault{Person: person,
		Settings: func() Settings {
			return Settings{Sources: []Source{{Account: "Home", Vaults: []string{"Private"}}, {Account: "Acme", Vaults: []string{"Employee"}}}}
		},
		Connect: func(_ context.Context, account, token string) (Store, error) {
			connected = append(connected, account+"|"+token)
			if account == "Acme" {
				return work, nil
			}
			return personal, nil
		}}
	ctx := context.Background()
	got, err := v.Resolve(ctx, "op://Private/Twilio (Sam - Acme)/password", "https://www.twilio.com/login", claude)
	if err != nil || got.Value != "twilio-secret" {
		t.Fatal(got, err)
	}
	if fmt.Sprint(connected) != "[Home| Acme|]" {
		t.Fatal("both accounts through the desktop app, no token:", connected)
	}
	if got, err := v.Resolve(ctx, "op://Employee/GitHub/password", "https://github.com/login", claude); err != nil || got.Value != "work-secret" {
		t.Fatal("the other account's vault:", got, err)
	}
	if len(person.prompts) != 1 || !strings.Contains(person.prompts[0], `vaults "Private", "Employee" for the rest of its session`) || strings.HasPrefix(person.prompts[0], "unlock") {
		t.Fatal("one Chrome Relay approval per agent session, naming the vaults; nothing to unlock:", person.prompts)
	}
	other := Who{Name: "Codex (x)", Session: "s9"}
	v.Resolve(ctx, "op://Private/Twilio (Sam - Acme)/username", "https://www.twilio.com/login", other)
	if len(person.prompts) != 2 {
		t.Fatal("another agent asks again", person.prompts)
	}
	if _, err := v.Resolve(ctx, "op://Shared/Anything/password", "https://www.twilio.com/", claude); err == nil || !strings.Contains(err.Error(), `only the vaults "Private", "Employee"`) {
		t.Fatal("vaults not allowed are refused:", err)
	}
	sum, _ := v.Summarize(ctx, claude)
	if len(sum.Items) != 2 || sum.Items[0].Vault != "Private" || sum.Items[0].Account != "Home" || sum.Items[1].Account != "Acme" {
		t.Fatalf("listing says where each login is: %+v", sum.Items)
	}
	clash := &Vault{Person: person, Connect: func(context.Context, string, string) (Store, error) { return personal, nil },
		Settings: func() Settings {
			return Settings{Sources: []Source{{Account: "A", Vaults: []string{"Private"}}, {Account: "B", Vaults: []string{"private"}}}}
		}}
	if _, err := clash.Resolve(ctx, "op://Private/Twilio (Sam - Acme)/password", "https://www.twilio.com/", claude); err == nil || !strings.Contains(err.Error(), "two allowed vaults") {
		t.Fatal("ambiguous vault names are refused:", err)
	}
}
