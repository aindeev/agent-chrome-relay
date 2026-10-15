package cli

import (
	"context"
	"errors"
	"sync"

	op "github.com/1password/onepassword-sdk-go"

	"github.com/aindeev/agent-chrome-relay/internal/phone"
	"github.com/aindeev/agent-chrome-relay/internal/vault"
)

// e2eVault: 1Password for the end-to-end tests: one login for the test site, and a person who approves.
type e2eVault struct {
	mu        sync.Mutex
	approvals []string
	site      string // the test site's origin, the login's website
}

func (v *e2eVault) item() op.Item {
	return op.Item{ID: "loc1", Title: "Local site", Category: op.ItemCategoryLogin, Websites: []op.Website{{URL: v.site}},
		Fields: []op.ItemField{{ID: "username", Title: "username", FieldType: op.ItemFieldTypeText, Value: "agent-bot"},
			{ID: "password", Title: "password", FieldType: op.ItemFieldTypeConcealed, Value: "hunter2-secret"}}}
}

// card: a Credit Card item, as 1Password's SDK gives it.
func (v *e2eVault) card() op.Item {
	return op.Item{ID: "card1", Title: "Test card", Category: op.ItemCategoryCreditCard, Fields: []op.ItemField{
		{ID: "cardholder", Title: "cardholder name", FieldType: op.ItemFieldTypeText, Value: "Ada Lovelace"},
		{ID: "ccnum", Title: "number", FieldType: op.ItemFieldTypeCreditCardNumber, Value: "4242 4242 4242 4242"},
		{ID: "cvv", Title: "verification number", FieldType: op.ItemFieldTypeConcealed, Value: "123"},
		{ID: "expiry", Title: "expiry date", FieldType: op.ItemFieldTypeMonthYear, Value: "202812"}}}
}

func (v *e2eVault) Vaults(context.Context) ([]op.VaultOverview, error) {
	return []op.VaultOverview{{ID: "v", Title: "Agents"}}, nil
}
func (v *e2eVault) List(context.Context, string) ([]op.ItemOverview, error) {
	var out []op.ItemOverview
	for _, it := range []op.Item{v.item(), v.card()} {
		out = append(out, op.ItemOverview{ID: it.ID, Title: it.Title, Category: it.Category, Websites: it.Websites})
	}
	return out, nil
}
func (v *e2eVault) Get(_ context.Context, _, id string) (op.Item, error) {
	if id == "loc1" {
		return v.item(), nil
	}
	if id == "card1" {
		return v.card(), nil
	}
	return op.Item{}, errors.New("no item")
}
func (v *e2eVault) Create(context.Context, op.ItemCreateParams) (op.Item, error) {
	return op.Item{}, errors.New("not in this test")
}
func (v *e2eVault) Put(_ context.Context, it op.Item) (op.Item, error) { return it, nil }

func (v *e2eVault) Unlock(key, reason string) (vault.Token, error) {
	return vault.Token{Value: "ops_x", Approve: "sign-in"}, nil
}
func (v *e2eVault) Approve(d phone.Details) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.approvals = append(v.approvals, d.Reason)
	return nil
}
