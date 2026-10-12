package vault

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	op "github.com/1password/onepassword-sdk-go"
)

func TestParseSave(t *testing.T) {
	r, save, err := ParseSave("op://Shared with Chrome Relay/Acme/password?new&username=bot@acme.test&length=16&symbols=no")
	if !save || err != nil || r.Vault != "Shared with Chrome Relay" || r.Item != "Acme" || r.Field != "password" || r.Username != "bot@acme.test" || r.Length != 16 || r.Symbols {
		t.Fatalf("%+v %v %v", r, save, err)
	}
	if _, save, _ := ParseSave("op://V/Acme/password"); save {
		t.Fatal("a plain reference fills")
	}
	for ref, want := range map[string]string{
		"op://V/Acme/password?new":                        "needs its username",
		"op://V/Acme/notes?new":                           "not \"notes\"",
		"op://V/Acme/password?new&username=a&length=8":    "12 to 64",
		"op://V/Acme/password?new&username=a&length=many": "12 to 64",
	} {
		if _, save, err := ParseSave(ref); !save || err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", ref, err)
		}
	}
	if r, _, err := ParseSave("op://V/Acme/otp?new"); err != nil || r.Field != "otp" {
		t.Fatal(r, err)
	}
}

func TestGenerate(t *testing.T) {
	for range 50 {
		p := Generate(16, true)
		if len(p) != 16 || !strings.ContainsAny(p, "abcdefghijkmnopqrstuvwxyz") || !strings.ContainsAny(p, "ABCDEFGHJKLMNPQRSTUVWXYZ") ||
			!strings.ContainsAny(p, "23456789") || !strings.ContainsAny(p, "-_.!@#%^*+=") {
			t.Fatal(p)
		}
		if q := Generate(20, false); len(q) != 20 || strings.ContainsAny(q, "-_.!@#%^*+=") {
			t.Fatal(q)
		}
	}
	if Generate(24, true) == Generate(24, true) {
		t.Fatal("random")
	}
}

func TestSavingANewLoginAndItsOneTimeCode(t *testing.T) {
	v, store, person, now := fixture()
	ctx := context.Background()
	r, _, _ := ParseSave("op://Shared with Chrome Relay/Acme/password?new&username=bot@acme.test")
	got, err := v.NewLogin(ctx, r, "https://app.acme.test/signup", claude)
	if err != nil || got.Kind != "secret" || len(got.Value) != 24 || got.Item != "Acme" {
		t.Fatal(got, err)
	}
	if last := person.prompts[len(person.prompts)-1]; last != `approve: let Claude Code (my-repo) save a new login for app.acme.test as bot@acme.test in your 1Password vault "Shared with Chrome Relay" (Chrome Relay makes the password)` {
		t.Fatal("the person approves the save, seeing the site and the account:", last)
	}
	it := store.items[len(store.items)-1]
	if it.Title != "Acme" || !slices.Contains(it.Tags, SavedTag) || PickField(it, "username", false).Value != "bot@acme.test" ||
		PickField(it, "password", false).Value != got.Value || websites(it)[0] != "https://app.acme.test" {
		t.Fatalf("%+v", it)
	}
	if _, err := v.NewLogin(ctx, r, "https://app.acme.test/signup", claude); err == nil || !strings.Contains(err.Error(), "already the login for bot@acme.test") {
		t.Fatal("one login per account and site:", err)
	}
	if _, err := v.NewLogin(ctx, r, "http://app.acme.test/signup", claude); err == nil {
		t.Fatal("https only")
	}

	// 2-step verification on the login Chrome Relay saved: the code goes with it
	o, _, _ := ParseSave("op://Shared with Chrome Relay/Acme/otp?new")
	code, err := v.SaveOTP(ctx, o, "otpauth://totp/Acme:bot?secret=JBSWY3DPEHPK3PXP&issuer=Acme", "https://app.acme.test/settings/2fa", claude)
	if want, _, _ := TOTP("JBSWY3DPEHPK3PXP", *now); err != nil || code.Value != want || code.Kind != "otp" || code.Item != "Acme" {
		t.Fatal(code, err)
	}
	it = store.items[len(store.items)-1]
	if f := PickField(it, "otp", true); f == nil || !strings.Contains(f.Value, "JBSWY3DPEHPK3PXP") {
		t.Fatalf("%+v", it)
	}
	if !strings.Contains(person.prompts[len(person.prompts)-1], `save the 2-step verification for "Acme" (bot@acme.test) on app.acme.test`) {
		t.Fatal(person.prompts)
	}

	// on the person's own login: never changed; the code goes into an item of its own beside it
	before := fmt.Sprint(store.items[0])
	n := len(store.items)
	gh, _, _ := ParseSave("op://Shared with Chrome Relay/GitHub/otp?new")
	if _, err := v.SaveOTP(ctx, gh, "GEZDGNBVGY3TQOJQ", "https://github.com/settings/two_factor_authentication/setup", claude); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(store.items[0]) != before || len(store.items) != n+1 {
		t.Fatal("the person's item is never changed")
	}
	side := store.items[n]
	if side.Title != "GitHub one-time code · Chrome Relay" || PickField(side, "username", false).Value != "agent-bot" || PickField(side, "otp", true) == nil || websites(side)[0] != "https://github.com" {
		t.Fatalf("%+v", side)
	}

	for _, c := range []struct{ secret, page, want string }{
		{"not a key", "https://github.com/x", "not a valid TOTP secret"},
		{"GEZDGNBVGY3TQOJQ", "https://evil.example/x", "is for https://github.com"},
	} {
		if _, err := v.SaveOTP(ctx, gh, c.secret, c.page, claude); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s on %s: %v", c.secret, c.page, err)
		}
	}
	person.decline = true
	r2, _, _ := ParseSave("op://Shared with Chrome Relay/Other/password?new&username=x@acme.test")
	if _, err := v.NewLogin(ctx, r2, "https://app.acme.test/signup", claude); err == nil {
		t.Fatal("nothing is saved when the person declines")
	}
	if slices.ContainsFunc(store.items, func(it op.Item) bool { return it.Title == "Other" }) {
		t.Fatal("declined: no item")
	}
}
