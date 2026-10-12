package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestServiceAccountSetupCreatesTheVaultAndTheAccount(t *testing.T) {
	var calls [][]string
	var out bytes.Buffer
	c := &cli{instance: instance{Name: "chrome-relay"}, out: &out, errw: &out,
		opAccts: func() []opAccount {
			return []opAccount{{UUID: "HOME", Email: "sam@example.com", URL: "my.1password.com"}, {UUID: "WORK", Email: "sam@acme.com", URL: "acme.1password.com"}}
		},
		op: func(args ...string) ([]byte, []byte, error) {
			calls = append(calls, args)
			switch strings.Join(args[:2], " ") {
			case "vault get":
				return nil, []byte(`[ERROR] 2026/10/11 12:00:00 "Agents" isn't a vault in this account. Specify the vault with its ID or name.`), errors.New("exit status 1")
			case "vault create":
				return []byte(`{"id":"v1","name":"Agents"}`), nil, nil
			case "service-account create":
				return []byte("ops_secret-token-value\n"), nil, nil
			}
			return nil, nil, errors.New("unexpected")
		}}
	tok, acct, err := c.createServiceAccount("Agents", "sam@acme.com", nil)
	if err != nil || tok != "ops_secret-token-value" || acct.UUID != "WORK" {
		t.Fatal(tok, acct, err)
	}
	if len(calls) != 3 || calls[0][len(calls[0])-3] != "WORK" {
		t.Fatal("in the account named with --account:", calls)
	}
	sa := strings.Join(calls[2], " ")
	if !strings.Contains(sa, "--vault Agents:read_items,write_items") || !strings.Contains(sa, "--raw") || !strings.Contains(sa, "--account WORK") {
		t.Fatal("read and write on that vault only:", sa)
	}
	said := out.String()
	if strings.Contains(said, "ops_") {
		t.Fatal("the token is never shown:", said)
	}
	if !strings.Contains(said, `Created the vault "Agents" in sam@acme.com`) || !strings.Contains(said, "Created the service account") {
		t.Fatal("says what it made:", said)
	}

	// several accounts, no --account, no terminal to ask: says which to pick
	if _, _, err := c.createServiceAccount("Agents", "", nil); err == nil || !strings.Contains(err.Error(), "--account") {
		t.Fatal(err)
	}
	// no service account can open a Private vault
	if err := c.vaultSetup(setupArgs{service: "Private"}, nil); err == nil || !strings.Contains(err.Error(), "never lets a service account into a Private vault") {
		t.Fatal(err)
	}
}

func TestServiceAccountSetupArgs(t *testing.T) {
	a, err := parseSetupArgs([]string{"Agents", "--account", "sam@acme.com"})
	if err != nil || a.service != "Agents" || a.account != "sam@acme.com" {
		t.Fatal(a, err)
	}
	if a, err := parseSetupArgs([]string{"Agents", "--paste"}); err != nil || !a.paste {
		t.Fatal(a, err)
	}
	if _, err := parseSetupArgs([]string{"Agents", "--account", "x", "--vaults", "A"}); err == nil {
		t.Fatal("--vaults is for the 1Password app")
	}
}
