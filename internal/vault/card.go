package vault

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	op "github.com/1password/onepassword-sdk-go"

	"github.com/aindeev/agent-chrome-relay/internal/phone"
)

// Cards. A Credit Card item has no website, so where its values go is the relay's to check (a checkout page, or a
// payment provider's frame in it, into the matching card field). What the vault does: name its fields the way
// checkout forms do, and always ask the person, naming the shop and the card, whatever they chose for sign-ins.

// CardFields: the names an agent uses (op://<vault>/<card>/number), as Value.Field.
var CardFields = []string{"number", "expiry", "expiry-month", "expiry-year", "cvc", "name"}

var cardAliases = map[string]string{
	"number": "number", "ccnum": "number", "card number": "number", "cardnumber": "number",
	"expiry": "expiry", "expiry date": "expiry", "expiration": "expiry", "exp": "expiry",
	"expiry-month": "expiry-month", "month": "expiry-month",
	"expiry-year": "expiry-year", "year": "expiry-year",
	"cvc": "cvc", "cvv": "cvc", "csc": "cvc", "security code": "cvc", "verification number": "cvc",
	"name": "name", "cardholder": "name", "cardholder name": "name", "name on card": "name",
}

// card resolves a field of a Credit Card item, after the person approves paying on host with it.
func (v *Vault) card(it op.Item, fieldName, host string, who Who) (Value, error) {
	want := cardAliases[strings.ToLower(strings.TrimSpace(fieldName))]
	if want == "" {
		return Value{}, fmt.Errorf("a card's fields are %s, not %q", strings.Join(CardFields, ", "), fieldName)
	}
	number := cardField(it, func(f op.ItemField) bool { return f.FieldType == op.ItemFieldTypeCreditCardNumber || f.ID == "ccnum" })
	digits := regexp.MustCompile(`\D`).ReplaceAllString(number, "")
	if len(digits) < 12 {
		return Value{}, fmt.Errorf("%q has no card number", it.Title)
	}
	last4 := digits[len(digits)-4:]
	var value string
	switch want {
	case "number":
		value = digits
	case "cvc":
		value = cardField(it, func(f op.ItemField) bool { return f.ID == "cvv" || strings.EqualFold(f.Title, "verification number") })
	case "name":
		value = cardField(it, func(f op.ItemField) bool {
			return f.ID == "cardholder" || strings.EqualFold(f.Title, "cardholder name")
		})
	default:
		raw := cardField(it, func(f op.ItemField) bool {
			return f.ID == "expiry" || (f.FieldType == op.ItemFieldTypeMonthYear && strings.Contains(strings.ToLower(f.Title), "expir"))
		})
		month, year, ok := monthYear(raw)
		if !ok {
			return Value{}, fmt.Errorf("%q has no expiry date", it.Title)
		}
		value = map[string]string{"expiry": month + "/" + year[2:], "expiry-month": month, "expiry-year": year}[want]
	}
	if value == "" {
		return Value{}, fmt.Errorf("%q has no %s", it.Title, want)
	}
	if err := v.approveCard(who, it, host, last4); err != nil {
		return Value{}, err
	}
	return Value{Value: value, Item: it.Title, Field: want, Kind: "card", Last4: last4}, nil
}

func cardField(it op.Item, ok func(op.ItemField) bool) string {
	for _, f := range it.Fields {
		if ok(f) {
			return strings.TrimSpace(f.Value)
		}
	}
	return ""
}

// monthYear reads 1Password's month-and-year value ("202812", "12/2028", "2028-12", "12/28") as "12", "2028".
func monthYear(raw string) (month, year string, ok bool) {
	parts := regexp.MustCompile(`\d+`).FindAllString(raw, -1)
	switch {
	case len(parts) == 1 && len(parts[0]) == 6: // YYYYMM (or MMYYYY)
		if p := parts[0]; p[:2] == "19" || p[:2] == "20" {
			year, month = p[:4], p[4:]
		} else {
			month, year = p[:2], p[2:]
		}
	case len(parts) == 2 && len(parts[0]) == 4:
		year, month = parts[0], parts[1]
	case len(parts) == 2:
		month, year = parts[0], parts[1]
		if len(year) == 2 {
			year = "20" + year
		}
	default:
		return "", "", false
	}
	if len(month) == 1 {
		month = "0" + month
	}
	if len(month) != 2 || len(year) != 4 || month < "01" || month > "12" {
		return "", "", false
	}
	return month, year, true
}

// approveCard: every payment is the person's to approve, on Touch ID (or their iPhone), naming the agent, the shop
// and the card; one approval covers that card's fields on that site for a couple of minutes, whatever they chose
// for sign-ins (even "off").
func (v *Vault) approveCard(who Who, it op.Item, host, last4 string) error {
	if v.approvals == nil {
		v.approvals = map[string]time.Time{}
	}
	key := who.Session + "|card|" + it.ID + "|" + host
	if v.approvals[key].After(v.now()) {
		return nil
	}
	reason := fmt.Sprintf("let %s pay on %s with %q (•••• %s) from 1Password (card number, expiry and security code, for %d minutes)",
		who.Name, host, it.Title, last4, int(signInWindow/time.Minute))
	v.mu.Unlock()
	err := v.Person.Approve(phone.Details{Reason: reason, Agent: who.Name, Site: host, What: "payment with •••• " + last4})
	v.mu.Lock()
	if err != nil {
		return err
	}
	if v.approvals == nil { // reopened meanwhile
		v.approvals = map[string]time.Time{}
	}
	v.approvals[key] = v.now().Add(signInWindow)
	return nil
}

// IsCard: does the reference name a Credit Card item? Asks no one: it only looks the item up, so the relay can
// refuse a login headed for a frame before anyone is asked to approve it.
func (v *Vault) IsCard(ctx context.Context, ref, pageURL string, who Who) bool {
	m := Ref.FindStringSubmatch(strings.TrimSpace(ref))
	if m == nil {
		return false
	}
	vaultName, itemName, fieldName := m[1], unescape(m[2]), unescape(m[3])
	if !slices.ContainsFunc(v.Settings().Vaults(), func(n string) bool { return strings.EqualFold(n, vaultName) }) {
		return false
	}
	what := fieldName
	if otpNames[strings.ToLower(fieldName)] {
		what = "one-time code"
	}
	host := pageURL
	if u, err := url.Parse(pageURL); err == nil {
		host = u.Hostname()
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	// if 1Password isn't open yet, opening it asks with the reason the fill itself would give
	if err := v.open(ctx, fmt.Sprintf("let %s fill the %s of %q on %s", who.Name, what, itemName, host)); err != nil {
		return false
	}
	it, err := v.item(ctx, v.places[strings.ToLower(vaultName)], itemName)
	return err == nil && it.Category == op.ItemCategoryCreditCard
}

// CardFieldOf: the card field a reference names ("number", "cvc", ...), as for a card; "" if it names none.
func CardFieldOf(ref string) string {
	m := Ref.FindStringSubmatch(strings.TrimSpace(ref))
	if m == nil {
		return ""
	}
	return cardAliases[strings.ToLower(strings.TrimSpace(unescape(m[3])))]
}
