package relay

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/aindeev/agent-chrome-relay/internal/vault"
)

// Paying with a card from 1Password. A Credit Card item has no website, so the relay decides where its values may
// go: into a card field of the checkout page itself (https), or of a payment provider's frame in it (Stripe's,
// Braintree's, ...), never into another site's frame; the number into a number field, the code into a code field.
// The person approves every payment (vault/card.go), and nothing typed comes back to the agent.

// paymentHosts: payment providers whose frames take card details on shops' checkout pages (and their subdomains).
var paymentHosts = []string{
	"js.stripe.com", "braintreegateway.com", "braintree-api.com", "adyen.com", "checkout.com", "squarecdn.com",
	"squareup.com", "paypal.com", "recurly.com", "chargebee.com", "paddle.com", "shopifyinc.com", "spreedly.com",
	"authorize.net", "worldpay.com", "cybersource.com", "mollie.com", "razorpay.com",
}

func (s *Server) paymentHost(host string) bool {
	host = strings.ToLower(host)
	for _, h := range append(paymentHosts, s.opt.PaymentHosts...) {
		if host == h || strings.HasSuffix(host, "."+h) {
			return true
		}
	}
	return false
}

// focus: where the focused field is: the page itself (sid ""), or another site's frame (its session and host).
type focus struct {
	el        *field
	sid, host string
}

// focusOf finds the focused field: in the page, or, when the page's focus is in another site's frame, in that frame.
// child: the session an agent's own command came through, if it was a frame's. Must hold s.mu.
func (a *Agent) focusOf(p *profile, t int, child string) *focus {
	if child != "" { // an agent's own command through a frame's session: that frame must have the page's focus
		if el := a.focusedIn(p, t, child); el != nil && el.Focus && !el.Frame {
			return &focus{el, child, a.frameHost(p, t, child)}
		}
		return nil
	}
	el := a.focusedIn(p, t, "")
	if el == nil || !el.Frame {
		if el == nil {
			return nil
		}
		return &focus{el: el}
	}
	for sid, fr := range a.frames {
		if fr.tab != t || fr.parent != "" { // only frames in the page itself: one nested in another site's frame pays that site
			continue
		}
		if fe := a.focusedIn(p, t, sid); fe != nil && fe.Focus && !fe.Frame {
			return &focus{fe, sid, a.frameHost(p, t, sid)}
		}
	}
	return &focus{el: el} // a frame of the page's own site, or one Chrome Relay has no session for
}

func (a *Agent) focusedIn(p *profile, t int, sid string) *field {
	r := a.cdpIn(p, t, sid, "Runtime.evaluate", map[string]any{"expression": strings.TrimSuffix(focusedExpr, "\n"), "returnByValue": true})
	var res struct {
		Result struct {
			Value *field `json:"value"`
		} `json:"result"`
	}
	json.Unmarshal(r.Result, &res)
	return res.Result.Value
}

// frameHost: where a frame's document comes from, as Chrome reports it (not as the frame says).
func (a *Agent) frameHost(p *profile, t int, sid string) string {
	var info struct {
		TargetInfo struct {
			URL string `json:"url"`
		} `json:"targetInfo"`
	}
	if r := a.cdpIn(p, t, sid, "Target.getTargetInfo", map[string]any{}); r.Error != "" || json.Unmarshal(r.Result, &info) != nil {
		return ""
	}
	return hostOf(info.TargetInfo.URL)
}

var (
	cardCVC    = regexp.MustCompile(`cvc|cvv|csc|security.?code|card.?code|verification`)
	cardNumber = regexp.MustCompile(`card.?num|cardnumber|cc-?num|card.?no\b|number`)
	cardMonth  = regexp.MustCompile(`month|\bmm\b`)
	cardYear   = regexp.MustCompile(`year|\byy(yy)?\b`)
	cardExpiry = regexp.MustCompile(`exp|mm\s*/\s*yy`)
	cardName   = regexp.MustCompile(`name.?on.?card|cardholder|card.?holder|cc-?name`)
)

// cardKind: which of a card's fields the focused field takes (vault.CardFields), or "".
func cardKind(f *field) string {
	ac := ""
	for _, tok := range strings.Fields(f.AC) { // "billing cc-number", "section-pay cc-exp"
		if strings.HasPrefix(tok, "cc-") {
			ac = tok
		}
	}
	switch ac {
	case "cc-number":
		return "number"
	case "cc-exp":
		return "expiry"
	case "cc-exp-month":
		return "expiry-month"
	case "cc-exp-year":
		return "expiry-year"
	case "cc-csc":
		return "cvc"
	case "cc-name", "cc-given-name", "cc-family-name":
		return "name"
	}
	h := f.Hint
	switch {
	case cardCVC.MatchString(h):
		return "cvc"
	case cardExpiry.MatchString(h) && cardMonth.MatchString(h) && !cardYear.MatchString(h):
		return "expiry-month"
	case cardExpiry.MatchString(h) && cardYear.MatchString(h) && !cardMonth.MatchString(h):
		return "expiry-year"
	case cardExpiry.MatchString(h):
		return "expiry"
	case cardName.MatchString(h):
		return "name"
	case cardNumber.MatchString(h) && strings.Contains(h, "card"):
		return "number"
	}
	return ""
}

// cardTarget: may the card's field `want` go where the focus is? The checks that need no 1Password, made before
// the person is asked and again right before typing.
func (a *Agent) cardTarget(f *focus, want string) error {
	switch {
	case f == nil || (f.el.Frame && f.sid == ""):
		return errors.New("no card field has focus (fill the card field by its ref; Chrome Relay can't see into that frame)")
	case f.sid != "" && (a.frames[f.sid].parent != "" || !a.s.paymentHost(f.host)):
		return fmt.Errorf("card details only go into the checkout page itself or a payment provider's frame in it (Stripe, Braintree, Adyen, ...), not a frame from %s", cmpOr(f.host, "another site"))
	}
	kind := cardKind(f.el)
	switch {
	case kind == "":
		return fmt.Errorf("the focused field doesn't look like a card field, so the card's %s doesn't go in it", cmpOr(want, "details"))
	case want != "" && kind != want:
		return fmt.Errorf("the focused field is for the card's %s, not its %s: fill it with op://.../%s", kind, want, kind)
	}
	return nil
}

// fillCard types one of a card's fields into the focused field, if that's where it may go. before: where the focus
// was when the agent asked, before the person approved. Must hold s.mu.
func (a *Agent) fillCard(t int, child string, m message, ref string, v vault.Value, pageURL string, before *focus) {
	p := a.p()
	refuse := func(err error) {
		a.s.audit(a, "1password refused", "tab", t, "ref", ref, "url", pageURL, "why", err.Error())
		a.s.log("1password: refused", ref, "in tab", t, "-", err.Error())
		a.err(m, "1Password: "+err.Error())
	}
	now := a.focusOf(p, t, child) // checked again, right before typing
	switch {
	case !a.tabs[t]:
		refuse(errors.New("the tab was closed"))
		return
	case a.urls[t] != pageURL:
		refuse(errors.New("the page changed while 1Password answered; fill the field again"))
		return
	case before == nil || now == nil || now.sid != before.sid:
		refuse(errors.New("the focus moved while 1Password answered; fill the field again"))
		return
	}
	if err := a.cardTarget(now, v.Field); err != nil {
		refuse(err)
		return
	}
	value := v.Value
	if v.Field == "expiry-year" && now.el.Max == 2 && len(value) == 4 {
		value = value[2:]
	}
	a.hideCard(v.Field, value) // before it's typed: nothing the page echoes meanwhile shows it
	// typed into that document only: a frame's text through the frame's own session, the page's by the page's own
	// editing command (Input.insertText through the tab would go wherever the focus is by then)
	var typed bool
	if now.sid != "" {
		r := a.cdpIn(p, t, now.sid, "Input.insertText", map[string]any{"text": value})
		typed = r.Error == ""
	} else {
		b, _ := json.Marshal(value)
		a.evalDriverIn(t, "", `(() => { let el = document.activeElement; while (el && el.shadowRoot && el.shadowRoot.activeElement) el = el.shadowRoot.activeElement;
  if (!el || /^(IFRAME|FRAME)$/.test(el.tagName)) return false; return document.execCommand("insertText", false, `+string(b)+`); })()`, false, &typed)
	}
	if !typed {
		refuse(errors.New("the card field didn't take the text; click it and fill it again"))
		return
	}
	// from now on the field reads back as filled, never what's in it, whatever it's called; no pictures of the page
	a.evalDriverIn(t, now.sid, `(() => { let el = document.activeElement; while (el && el.shadowRoot && el.shadowRoot.activeElement) el = el.shadowRoot.activeElement;
  const D = window.__crDrive ||= { ids: new WeakMap(), nodes: new Map(), next: 1 }; (D.secret ||= new WeakSet()).add(el); return true; })()`, false, nil)
	a.cardFilled[t] = true
	a.s.audit(a, "1password card fill", "tab", t, "item", v.Item, "field", v.Field, "card", "•••• "+v.Last4, "url", pageURL, "frame", now.host)
	a.s.log("1password: filled the card's", v.Field, "of", v.Item, "in tab", t)
	a.ok(m, nil)
}

// hideCard: the secret parts of a card as pages show them back. The number: whole, in groups of four, Amex's
// 4-6-5 and Diners' 4-6-4, with spaces or dashes. The code: only as a whole value ("123"), so the page's other
// numbers still read. The expiry and the name are left as they are: pages show them anyway, and masking "28" or
// "2028" would garble every timestamp and id the agent reads.
func (a *Agent) hideCard(field, value string) {
	switch field {
	case "cvc":
		b, _ := json.Marshal(value)
		a.hiddenWhole = append(a.hiddenWhole, string(b))
	case "number":
		d := regexp.MustCompile(`\D`).ReplaceAllString(value, "")
		if len(d) < 12 {
			return
		}
		a.hide(d)
		var fours []string
		for i := 0; i < len(d); i += 4 {
			fours = append(fours, d[i:min(i+4, len(d))])
		}
		layouts := [][]string{fours}
		if len(d) >= 14 {
			layouts = append(layouts, []string{d[:4], d[4:10], d[10:]})
		}
		for _, l := range layouts {
			a.hide(strings.Join(l, " "))
			a.hide(strings.Join(l, "-"))
		}
	}
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
