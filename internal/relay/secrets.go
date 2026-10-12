package relay

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/aindeev/agent-chrome-relay/internal/passkeys"
	"github.com/aindeev/agent-chrome-relay/internal/phone"
	"github.com/aindeev/agent-chrome-relay/internal/vault"
)

func vaultRef(text string) bool { return vault.Ref.MatchString(text) }

func (a *Agent) who() vault.Who { return vault.Who{Name: describe(a.app, a.cwd), Session: a.sid} }

// fillSecret: `agent-browser fill @e3 "op://Agents/GitHub/password"` types the real value from the shared vault.
// The agent never gets it back: the vault (and the person's Touch ID) decides, the page's site must be the
// item's, secrets go only into password fields of the page itself, and anything the page echoes is masked.
func (a *Agent) fillSecret(t int, child string, m message, ref string) {
	p, u := a.p(), a.urls[t]
	refuse := func(err error) {
		a.s.audit(a, "1password refused", "tab", t, "ref", ref, "url", u, "why", err.Error())
		a.s.log("1password: refused", ref, "in tab", t, "-", err.Error())
		a.err(m, "1Password: "+err.Error())
	}
	if child != "" {
		refuse(errors.New("secrets are only typed into the page itself, not into a frame inside it"))
		return
	}
	// 1Password and the person's Touch ID take a moment: don't hold the relay meanwhile
	a.s.mu.Unlock()
	v, err := a.s.vault.Resolve(context.Background(), ref, a.targetURLOf(t), a.who())
	a.s.mu.Lock()
	if err != nil {
		refuse(err)
		return
	}
	// what has focus is checked last, right before typing
	el := a.focusedField(p, t)
	switch {
	case !a.tabs[t]:
		refuse(errors.New("the tab was closed"))
	case el != nil && el.Frame:
		refuse(errors.New("the focused field is inside a frame; secrets are only typed into the page itself"))
	case el == nil:
		refuse(errors.New("no text field has focus: use `agent-browser fill @ref <reference>` on the field"))
	case a.urls[t] != u:
		refuse(errors.New("the page changed while 1Password answered; fill the field again"))
	case v.Kind == "secret" && el.Type != "password":
		what := el.Type
		if what == "" {
			what = strings.ToLower(el.Tag)
		}
		refuse(fmt.Errorf("%q of %q only goes into a password field, and the focused field is %s", v.Field, v.Item, what))
	default:
		if sent := a.cdp(p, t, "Input.insertText", map[string]any{"text": v.Value}); sent.Error != "" {
			refuse(errors.New(sent.Error))
			return
		}
		a.hide(v.Value) // secrets, and one-time codes too: nothing the agent reads back shows them
		a.filled[t] = v.Item
		a.s.audit(a, "1password fill", "tab", t, "item", v.Item, "field", v.Field, "url", u)
		a.s.log("1password: filled", v.Field, "of", v.Item, "in tab", t)
		a.ok(m, nil)
	}
}

// field: the focused element, as focused.js describes it (nil: no text field has focus).
type field struct {
	Frame bool   `json:"frame"`
	Tag   string `json:"tag"`
	Type  string `json:"type"`
}

func (a *Agent) focusedField(p *profile, t int) *field {
	r := a.cdp(p, t, "Runtime.evaluate", map[string]any{"expression": strings.TrimSuffix(focusedExpr, "\n"), "returnByValue": true})
	var res struct {
		Result struct {
			Value *field `json:"value"`
		} `json:"result"`
	}
	json.Unmarshal(r.Result, &res)
	return res.Result.Value
}

// saveSecret: a ?new reference (vault/save.go). A new login: Chrome Relay makes the password, saves the item and
// types the password (the same reference again types the same one, for "confirm password"). 2-step verification:
// it reads the setup key from the page, saves it and types the current code. The agent sees neither.
func (a *Agent) saveSecret(t int, child string, m message, ref string, r vault.SaveRef, parseErr error) {
	p, u := a.p(), a.urls[t]
	refuse := func(err error) {
		a.s.audit(a, "1password save refused", "tab", t, "ref", ref, "url", u, "why", err.Error())
		a.s.log("1password: save refused", ref, "in tab", t, "-", err.Error())
		a.err(m, "1Password: "+err.Error())
	}
	el := a.focusedField(p, t)
	switch {
	case parseErr != nil:
		refuse(parseErr)
		return
	case child != "" || (el != nil && el.Frame):
		refuse(errors.New("secrets are only typed into the page itself, not into a frame inside it"))
		return
	case el == nil:
		refuse(errors.New("no text field has focus: use `agent-browser fill @ref <reference>` on the field"))
		return
	case r.Field == "password" && el.Type != "password":
		refuse(errors.New("a new password only goes into a password field"))
		return
	}
	var v vault.Value
	var err error
	key := a.sid + "|" + ref
	if r.Field == "password" {
		if nl, ok := a.newLogins[key]; ok && time.Since(nl.at) < 10*time.Minute {
			v = nl.v // "confirm password": the same one, saved already
		} else {
			a.s.mu.Unlock()
			v, err = a.s.vault.NewLogin(context.Background(), r, a.targetURLOf(t), a.who())
			a.s.mu.Lock()
			if err == nil {
				a.newLogins[key] = newLogin{v, time.Now()}
				a.s.audit(a, "1password saved login", "tab", t, "item", v.Item, "username", r.Username, "url", u)
				a.s.log("1password: saved a new login", v.Item, "from tab", t)
			}
		}
	} else {
		secret, ferr := a.pageOTPSecret(p, t)
		if ferr != nil {
			refuse(ferr)
			return
		}
		a.hide(secret)
		a.s.mu.Unlock()
		v, err = a.s.vault.SaveOTP(context.Background(), r, secret, a.targetURLOf(t), a.who())
		a.s.mu.Lock()
		if err == nil {
			a.s.audit(a, "1password saved one-time code", "tab", t, "item", v.Item, "url", u)
			a.s.log("1password: saved a one-time code setup in", v.Item, "from tab", t)
		}
	}
	if err != nil {
		refuse(err)
		return
	}
	switch {
	case !a.tabs[t]:
		refuse(errors.New("the tab was closed (the secret is saved in 1Password)"))
		return
	case a.urls[t] != u:
		refuse(errors.New("the page changed while 1Password answered (the secret is saved in 1Password); fill the field again with the plain reference"))
		return
	}
	if sent := a.cdp(p, t, "Input.insertText", map[string]any{"text": v.Value}); sent.Error != "" {
		refuse(errors.New(sent.Error))
		return
	}
	a.hide(v.Value)
	a.filled[t] = v.Item
	a.ok(m, nil)
}

// pageOTPSecret: the one 2-step setup key the page shows (otpsetup.js), or why there isn't one.
func (a *Agent) pageOTPSecret(p *profile, t int) (string, error) {
	r := a.cdp(p, t, "Runtime.evaluate", map[string]any{"expression": strings.TrimSuffix(otpSetupExpr, "\n"), "returnByValue": true, "awaitPromise": true})
	var res struct {
		Result struct {
			Value struct {
				URIs []string `json:"uris"`
				Keys []string `json:"keys"`
			} `json:"value"`
		} `json:"result"`
	}
	if r.Error != "" {
		return "", fmt.Errorf("could not read the page: %s", r.Error)
	}
	json.Unmarshal(r.Result, &res)
	found := res.Result.Value
	cands := slices.Compact(slices.Sorted(slices.Values(found.URIs)))
	if len(cands) == 0 {
		cands = slices.Compact(slices.Sorted(slices.Values(found.Keys)))
	}
	switch len(cands) {
	case 0:
		return "", errors.New("the page shows no 2-step setup key: if it offers \"can't scan the QR code?\" or \"enter a setup key\", click that to show the key, then fill again")
	case 1:
		return cands[0], nil
	}
	return "", fmt.Errorf("the page shows %d possible setup keys; open the page where only the one being set up is shown", len(cands))
}

// targetURLOf: the full committed URL (the vault checks its scheme and host).
func (a *Agent) targetURLOf(t int) string {
	if u := a.targetURLs[t]; u != "" {
		return u
	}
	return a.urls[t]
}

// onWebAuthn: a page in an agent tab asked for a passkey (passkey.js). Answered from the relay's own record of
// the tab's address, never what the page claims, and only for the page itself (top frame, main world).
func (a *Agent) onWebAuthn(t, id int, c map[string]any, from callFrom) {
	p := a.p()
	kind := "get"
	if c["kind"] == "create" {
		kind = "create"
	}
	mediation, _ := c["mediation"].(string)
	reply := func(r any) {
		b, _ := json.Marshal(r)
		a.cdpSend(p, t, "Runtime.evaluate", map[string]any{"expression": fmt.Sprintf("window.__agentBridgeDone && window.__agentBridgeDone(%d, %s)", id, b)})
	}
	refuse := func(name, why string, kv ...any) {
		a.s.audit(a, "passkey refused", append([]any{"tab", t, "kind", kind, "url", a.urls[t], "why", why}, kv...)...)
		a.s.log("passkey", kind, "refused in tab", t, "-", why)
		reply(map[string]any{"error": name, "message": why})
	}
	ctx, ok := a.contexts[t][from.contextID]
	if from.sessionID != "" || !ok || !ctx.isDefault || ctx.frameID != a.mainFrames[t] {
		a.s.log("passkey request ignored: not from the page itself, tab", t)
		return
	}
	u, err := url.Parse(a.urls[t])
	if err != nil || u.Host == "" {
		refuse("NotAllowedError", "the page's address is unknown")
		return
	}
	raw, _ := json.Marshal(c["publicKey"])
	var create passkeys.CreationOptions
	var get passkeys.RequestOptions
	rpID := ""
	if kind == "create" {
		json.Unmarshal(raw, &create)
		rpID = create.RP.ID
	} else {
		json.Unmarshal(raw, &get)
		rpID = get.RPID
	}
	if rpID == "" {
		rpID = u.Hostname()
	}
	rpID = strings.ToLower(rpID)
	if !vault.SecureOrigin(u) || !passkeys.RPIDAllowed(rpID, u.Hostname()) {
		refuse("SecurityError", fmt.Sprintf("%s may not use passkeys for %s", u.Host, rpID))
		return
	}
	if mediation == "conditional" { // autofill-style requests: only the relay's own passkeys, never a prompt for the person
		if kind == "create" {
			refuse("NotAllowedError", "passkeys are only created when a page asks outright, never as an automatic upgrade")
			return
		}
		if !a.s.vault.Configured() {
			return
		}
	}
	// Not the relay's to answer: 1Password (in the page) asks the person; the agent and the toolbar icon say so.
	handoff := func(why string) {
		a.handoffs[t] = handoff{rpID, kind}
		a.s.extSend(p, map[string]any{"op": "handoff", "tabId": t, "rpId": rpID})
		a.s.alert(p.prof, t, "Your passkey is needed", fmt.Sprintf("An agent is signing in to %s and needs your passkey from 1Password. Click to open its tab and approve.", rpID))
		a.s.audit(a, "passkey handoff", "tab", t, "kind", kind, "rpId", rpID, "why", why, "url", a.urls[t])
		a.s.log("passkey", kind, "for", rpID, "handed to 1Password in tab", t, "-", why)
		reply(map[string]any{"handoff": true})
	}
	if !a.s.vault.Configured() {
		handoff("no shared vault")
		return
	}
	if a.s.chip == nil {
		handoff("this chrome-relay has no Secure Enclave passkeys (built without Xcode's command line tools)")
		return
	}
	if kind == "create" && time.Since(a.lastInput[t]) >= 30*time.Second {
		refuse("NotAllowedError", "a page may create a passkey only right after the agent clicked or typed in it")
		return
	}
	a.s.mu.Unlock()
	keys, err := a.s.vault.Passkeys(context.Background(), rpID, a.who())
	a.s.mu.Lock()
	if err != nil {
		handoff("shared vault unavailable: " + err.Error())
		return
	}
	if kind == "get" {
		// Which account: the ones the site names (allowCredentials); if it names none and the vault has several for
		// this site, the account the agent just filled on this tab; never a guess between accounts.
		allow := passkeys.IDs(get.AllowCredentials)
		var cands []passkeys.Passkey
		for _, k := range keys {
			if len(allow) == 0 || slices.Contains(allow, k.CredentialID) {
				cands = append(cands, k)
			}
		}
		if len(cands) > 1 {
			if mine := slices.DeleteFunc(slices.Clone(cands), func(k passkeys.Passkey) bool { return k.Item != a.filled[t] }); len(mine) > 0 {
				cands = mine
			}
		}
		if len(cands) > 1 {
			if mediation == "conditional" {
				return
			}
			var names []string
			for _, k := range cands {
				names = append(names, fmt.Sprintf("%q (%s)", k.Item, k.UserName))
			}
			refuse("NotAllowedError", fmt.Sprintf("the vault has %d passkeys for %s: %s. Fill the username first (agent-browser fill @ref \"op://<vault>/<item>/username\") so Chrome Relay knows which account", len(cands), rpID, strings.Join(names, ", ")))
			return
		}
		var key *passkeys.Passkey
		if len(cands) == 1 {
			key = &cands[0]
		}
		if key == nil {
			if mediation == "conditional" { // like a person who didn't pick one: stays pending
				a.s.log("passkey autofill: none for", rpID, "in tab", t)
				return
			}
			handoff("not in the shared vault")
			return
		}
		// the prompt reads "chrome-relay is trying to <this>" (the iPhone shows it too): who, which site and account
		as := key.UserName
		if as == "" {
			as = key.Item
		}
		reason := fmt.Sprintf("let %s sign in to %s as %s, from %s", describe(a.app, a.cwd), rpID, as, u.Host)
		reuse := a.s.passkeySettings().reuse
		if reuse > 0 {
			reason += fmt.Sprintf(" (and use its other passkeys for the next %s without asking)", humanDuration(reuse))
		}
		origin := u.Scheme + "://" + u.Host
		chip := scoped{a.s.chip, a.sid, reuse}
		if handle, _ := base64.RawURLEncoding.DecodeString(key.KeyHandle); relayApproved(handle) {
			// Chrome Relay asks: Touch ID here or Face ID on the paired iPhone, whichever answers first; one approval
			// may cover this agent's passkey sign-ins for the reuse window
			if a.passkeysUntil.Before(time.Now()) {
				a.s.audit(a, "passkey approval", "tab", t, "rpId", rpID, "item", key.Item, "url", a.urls[t])
				a.s.log("passkey: asking for approval to sign in to", rpID, "in tab", t)
				a.s.mu.Unlock()
				err := a.s.vault.Person.Approve(phone.Details{Reason: reason, Agent: describe(a.app, a.cwd), Site: rpID, What: "passkey as " + as})
				a.s.mu.Lock()
				if err != nil {
					refuse("NotAllowedError", err.Error(), "item", key.Item)
					return
				}
				a.passkeysUntil = time.Now().Add(reuse)
			}
		} else {
			// a passkey made before: the chip itself asks for Touch ID on this Mac
			a.s.audit(a, "passkey touch id", "tab", t, "rpId", rpID, "item", key.Item, "url", a.urls[t])
			a.s.log("passkey: asking for Touch ID to sign in to", rpID, "in tab", t)
		}
		a.s.mu.Unlock()
		cred, err := passkeys.Authenticate(get, *key, origin, chip, reason)
		a.s.mu.Lock()
		if err != nil {
			name := "NotAllowedError"
			var pe *passkeys.Error
			if errors.As(err, &pe) {
				name = pe.Name
			}
			refuse(name, err.Error(), "item", key.Item)
			return
		}
		reply(map[string]any{"credential": cred})
		a.s.audit(a, "passkey sign-in", "tab", t, "rpId", rpID, "item", key.Item, "url", a.urls[t])
		a.s.log("passkey: signed in to", rpID, "with", key.Item, "in tab", t)
		return
	}
	exclude := passkeys.IDs(create.ExcludeCredentials)
	for _, k := range keys {
		if slices.Contains(exclude, k.CredentialID) {
			refuse("InvalidStateError", fmt.Sprintf("the vault already holds a passkey for this %s account", rpID))
			return
		}
	}
	key, cred, err := passkeys.Register(create, u.Scheme+"://"+u.Host, u.Hostname(), scoped{a.s.chip, a.sid, 0})
	if err != nil {
		var pe *passkeys.Error
		name := "NotAllowedError"
		if errors.As(err, &pe) {
			name = pe.Name
		}
		refuse(name, err.Error())
		return
	}
	// Saved before the site hears of it: a passkey the site knows but the vault lost would lock the account out.
	a.s.mu.Unlock()
	item, err := a.s.vault.SavePasskey(context.Background(), key, create.RP.Name)
	a.s.mu.Lock()
	if err != nil {
		refuse("NotAllowedError", "1Password: could not save the passkey: "+err.Error())
		return
	}
	reply(map[string]any{"credential": cred})
	a.s.audit(a, "passkey created", "tab", t, "rpId", rpID, "item", item, "url", a.urls[t])
	a.s.log("passkey: created for", rpID, "in", item, "tab", t)
}

// humanDuration for prompts: "2 minutes", "90 seconds".
func humanDuration(d time.Duration) string {
	secs := int(d.Seconds())
	switch {
	case secs%60 == 0 && secs == 60:
		return "minute"
	case secs%60 == 0:
		return fmt.Sprintf("%d minutes", secs/60)
	}
	return fmt.Sprintf("%d seconds", secs)
}
