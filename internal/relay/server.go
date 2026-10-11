package relay

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/aindeev/agent-chrome-relay/internal/macos"
	"github.com/aindeev/agent-chrome-relay/internal/passkeys"
	"github.com/aindeev/agent-chrome-relay/internal/vault"
)

// Concurrency model, the same as the Node relay's event loop: all relay state is guarded by Server.mu, and code
// runs to completion between waits. A connection's reader takes the lock BEFORE handing a message to its handler
// goroutine, so handlers start in arrival order; a handler releases the lock only while it waits on Chrome
// (await) and when it finishes. So between two waits, nothing else touches the state.

// Server is one relay instance.
type Server struct {
	opt      Options
	token    string
	instance string // changes on every relay start: the extension closes tabs a previous relay left
	mu       sync.Mutex
	profiles map[string]*profile // email -> the extension connection serving that Chrome profile
	last     map[string]string   // email -> the extension install that last served it
	agents   map[*Agent]bool
	notes    map[int]authNote // tabId -> last Google sign-in hop result, for the agent's error message
	seq      int
	vault    *vault.Vault
	chip     enclave
	caller   func(remotePort int) Caller
	isChrome func(pid int) bool
	logMu    sync.Mutex
	watchers atomic.Int32 // identity-file watchers running (one per connected extension), for tests
	// how often identities.json is checked for changes; how long a command on a tab waiting for the person's
	// 1Password passkey approval waits before saying so
	watchEvery, handoffWait time.Duration
}

type authNote struct {
	TabID                         int
	Site, Account, NeedsUser, URL string
}

// profile is one extension connection.
type profile struct {
	conn    *conn
	next    int
	pending map[int]chan extReply
	seq     int
	client  string
	prof    string
	stop    context.CancelFunc
}

type extReply struct {
	Result json.RawMessage `json:"result"`
	Error  string          `json:"error"`
}

// conn: a WebSocket with one writer at a time.
type conn struct {
	ws *websocket.Conn
	mu sync.Mutex
	up bool
}

func (c *conn) send(b []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.up {
		return fmt.Errorf("closed")
	}
	return c.ws.WriteMessage(websocket.TextMessage, b)
}
func (c *conn) close(code int, reason string) {
	c.mu.Lock()
	c.up = false
	c.ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason), time.Now().Add(time.Second))
	c.mu.Unlock()
	c.ws.Close()
}
func (c *conn) open() bool { c.mu.Lock(); defer c.mu.Unlock(); return c.up }

// Run serves until the process ends.
func Run(o Options) error {
	s, err := New(o)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", o.Port))
	if err != nil {
		return err
	}
	go s.sweep()
	s.log("bridge listening on", o.Port)
	return http.Serve(ln, s)
}

// New prepares a relay from its data folder.
func New(o Options) (*Server, error) {
	tok, err := os.ReadFile(filepath.Join(o.Home, "token"))
	if err != nil {
		return nil, err
	}
	// Chrome Relay's files are private to this user: which accounts and sites are used, and the audit trail.
	os.Chmod(o.Home, 0o700)
	os.MkdirAll(filepath.Join(o.Home, "audit"), 0o700)
	os.Chmod(filepath.Join(o.Home, "audit"), 0o700)
	for _, f := range []string{"config.json", "identities.json", "relay.log", "relay.out"} {
		os.Chmod(filepath.Join(o.Home, f), 0o600)
	}
	if days, _ := filepath.Glob(filepath.Join(o.Home, "audit", "*.jsonl")); days != nil {
		for _, f := range days {
			os.Chmod(f, 0o600)
		}
	}
	b := make([]byte, 8)
	rand.Read(b)
	s := &Server{opt: o, token: strings.TrimSpace(string(tok)), instance: hex.EncodeToString(b), profiles: map[string]*profile{}, last: map[string]string{},
		agents: map[*Agent]bool{}, notes: map[int]authNote{}, isChrome: isChrome,
		watchEvery: 2 * time.Second, handoffWait: 15 * time.Second}
	s.caller = func(port int) Caller { return findCaller(port, os.Getenv("CHROME_RELAY_CHECK_TREE") == "0") }
	cli := "chrome-relay"
	if o.Instance != "" {
		cli = "chrome-relay-" + o.Instance
	}
	s.vault = &vault.Vault{Settings: s.vaultSettings, Connect: vault.Connect(o.Version), Person: vault.MacPerson{Instance: o.Instance, CLI: cli},
		Stamp: func() string { return vault.TokenStamp(o.Instance) }, CLI: cli}
	if macos.Linked { // the core-only build has no Secure Enclave passkeys; 1Password's still work, through the handoff
		s.chip = chip{}
	}
	return s, nil
}

// enclave makes and uses passkeys that never leave the chip: this Mac's Secure Enclave, or a test double.
type enclave interface {
	Create() (handle, publicKey []byte, err error)
	Sign(handle, data []byte, reason, scope string, reuse time.Duration) ([]byte, error)
}

type chip struct{}

func (chip) Create() ([]byte, []byte, error) { return macos.PasskeyCreate() }
func (chip) Sign(handle, data []byte, reason, scope string, reuse time.Duration) ([]byte, error) {
	return macos.PasskeySign(handle, data, reason, scope, reuse)
}

// scoped: the enclave as one agent uses it, so a Touch ID approval covers only that agent's sign-ins.
type scoped struct {
	e     enclave
	scope string
	reuse time.Duration
}

func (c scoped) Create() ([]byte, []byte, error) { return c.e.Create() }
func (c scoped) Sign(handle, data []byte, reason string) ([]byte, error) {
	return c.e.Sign(handle, data, reason, c.scope, c.reuse)
}

var _ passkeys.Chip = scoped{}

// passkeySettings: "passkeys" in config.json. Enabled (default) answers passkey requests in agent tabs (the
// relay's Secure Enclave passkeys, else 1Password's own prompt); off leaves pages' WebAuthn alone. Every
// passkey sign-in asks for Touch ID, unless touchIdReuseSeconds (at most 300) lets one approval cover an
// agent's sign-ins for that long.
type passkeySettings struct {
	enabled bool
	reuse   time.Duration
}

func (s *Server) passkeySettings() passkeySettings {
	out := passkeySettings{enabled: true}
	pk, _ := s.config()["passkeys"].(map[string]any)
	if on, ok := pk["enabled"].(bool); ok {
		out.enabled = on
	}
	if secs, ok := pk["touchIdReuseSeconds"].(float64); ok && secs > 0 {
		out.reuse = min(time.Duration(secs)*time.Second, 5*time.Minute)
	}
	return out
}

func (s *Server) config() map[string]any {
	var c map[string]any
	b, _ := os.ReadFile(filepath.Join(s.opt.Home, "config.json"))
	json.Unmarshal(b, &c)
	return c
}

// vaultSettings: "onepassword" in config.json. {"accounts": [{"account": "my.1password.com", "vaults": ["Private"]}, ...]} for
// your 1Password accounts through the desktop app; {"vault": "Agents"} for a service account; "approve" for how
// often Chrome Relay itself asks (sign-in, session, off).
func (s *Server) vaultSettings() vault.Settings {
	var out vault.Settings
	op, _ := s.config()["onepassword"].(map[string]any)
	if op == nil {
		return out
	}
	strs := func(v any) []string {
		var l []string
		if xs, ok := v.([]any); ok {
			for _, x := range xs {
				if s, ok := x.(string); ok && s != "" {
					l = append(l, s)
				}
			}
		}
		return l
	}
	if accounts, ok := op["accounts"].([]any); ok {
		for _, a := range accounts {
			if m, ok := a.(map[string]any); ok {
				if name, _ := m["account"].(string); name != "" {
					out.Sources = append(out.Sources, vault.Source{Account: name, Vaults: strs(m["vaults"])})
				}
			}
		}
	}
	if name, _ := op["vault"].(string); name != "" {
		out.Sources = append(out.Sources, vault.Source{Vaults: []string{name}})
	}
	out.Approve, _ = op["approve"].(string)
	return out
}

// allowedExtensionIDs: Chrome derives an unpacked extension's ID from its folder; setup records this checkout's.
func (s *Server) allowedExtensionIDs() []string {
	var ids []string
	if l, ok := s.config()["extensionIds"].([]any); ok {
		for _, x := range l {
			if id, ok := x.(string); ok {
				ids = append(ids, id)
			}
		}
	}
	return ids
}

func (s *Server) log(a ...any) {
	parts := make([]string, len(a))
	for i, x := range a {
		parts[i] = fmt.Sprint(x)
	}
	s.logMu.Lock()
	defer s.logMu.Unlock()
	f, err := os.OpenFile(filepath.Join(s.opt.Home, "relay.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err == nil {
		fmt.Fprintf(f, "%s %s\n", time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), strings.Join(parts, " "))
		f.Close()
	}
}

// Audit trail: what every agent did in the browser, one JSON line per event, one file per day. Read it with
// `chrome-relay audit`. Typed text is not stored (only its length); URLs drop query strings.
func (s *Server) audit(a *Agent, event string, kv ...any) {
	now := time.Now().UTC()
	profile := a.email
	if profile == "" {
		profile = "(default)"
	}
	pairs := append([]any{"t", now.Format("2006-01-02T15:04:05.000Z"), "s", a.sid, "via", a.app, "profile", profile, "event", event}, kv...)
	var b strings.Builder
	b.WriteString("{")
	first := true
	for i := 0; i+1 < len(pairs); i += 2 {
		v := pairs[i+1]
		if v == nil || v == "" {
			continue // like JSON.stringify dropping undefined
		}
		if !first {
			b.WriteString(",")
		}
		first = false
		k, _ := json.Marshal(pairs[i])
		val, _ := json.Marshal(v)
		b.Write(k)
		b.WriteString(":")
		b.Write(val)
	}
	b.WriteString("}\n")
	go func(line string, day string) {
		f, err := os.OpenFile(filepath.Join(s.opt.Home, "audit", day+".jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err == nil {
			f.WriteString(line)
			f.Close()
		}
	}(b.String(), now.Format("2006-01-02"))
}

// cleanURL keeps origin and path: no query string or fragment in the audit trail.
func cleanURL(u string) string {
	x, err := url.Parse(u)
	if err != nil || x.Scheme == "" {
		if len(u) > 200 {
			return u[:200]
		}
		return u
	}
	if x.Scheme == "about" || x.Scheme == "data" || x.Scheme == "blob" || x.Opaque != "" {
		return x.Scheme + ":" + x.Opaque
	}
	return x.Scheme + "://" + x.Host + x.EscapedPath()
}

// ---- identities.json: which Google account each site uses
func (s *Server) readIDs() map[string]any {
	b, err := os.ReadFile(filepath.Join(s.opt.Home, "identities.json"))
	if err != nil {
		return nil
	}
	var d map[string]any
	if json.Unmarshal(b, &d) != nil {
		return nil
	}
	return d
}
func (s *Server) siteAccounts() map[string]any {
	out := map[string]any{}
	if d := s.readIDs(); d != nil {
		if sites, ok := d["sites"].(map[string]any); ok {
			for _, k := range []string{"learned", "rules"} {
				if m, ok := sites[k].(map[string]any); ok {
					for site, acc := range m {
						out[site] = acc
					}
				}
			}
		}
	}
	return out
}

// learn remembers which Google account a site used, for next time and for agents (never over an unreadable file).
func (s *Server) learn(site, account string) {
	d := s.readIDs()
	if site == "" || d == nil {
		return
	}
	sites, _ := d["sites"].(map[string]any)
	if sites == nil {
		sites = map[string]any{}
		d["sites"] = sites
	}
	learned, _ := sites["learned"].(map[string]any)
	if learned == nil {
		learned = map[string]any{}
		sites["learned"] = learned
	}
	if rules, _ := sites["rules"].(map[string]any); rules != nil && rules[site] != nil {
		return
	}
	if learned[site] == account {
		return
	}
	learned[site] = account
	b, _ := json.MarshalIndent(d, "", "  ")
	path := filepath.Join(s.opt.Home, "identities.json")
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if os.WriteFile(tmp, b, 0o600) == nil {
		os.Rename(tmp, path) // atomic: readers never see a half-written file
	}
	s.log("learned", site, "->", account)
}

// ---- calls into the extension. Must be called with s.mu held.

// extSend sends now (order matters) and returns where the reply will arrive. A reply that doesn't come in 30s
// (dropped socket, worker restart) arrives as an error.
func (s *Server) extSend(p *profile, msg map[string]any) chan extReply {
	ch := make(chan extReply, 1)
	p.next++
	id := p.next
	msg["id"] = id
	p.pending[id] = ch
	b, _ := json.Marshal(msg)
	if err := p.conn.send(b); err != nil {
		delete(p.pending, id)
		ch <- extReply{Error: "extension connection lost: " + err.Error()}
		return ch
	}
	time.AfterFunc(30*time.Second, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if p.pending[id] == ch {
			delete(p.pending, id)
			ch <- extReply{Error: "Chrome Relay extension did not answer within 30s"}
		}
	})
	return ch
}

// await: wait for a reply with the lock released (the Node relay's await).
func (s *Server) await(ch chan extReply) extReply {
	s.mu.Unlock()
	r := <-ch
	s.mu.Lock()
	return r
}
func (s *Server) extCall(p *profile, msg map[string]any) extReply { return s.await(s.extSend(p, msg)) }

// locked runs f later, under the lock (a promise nobody awaits).
func (s *Server) locked(f func()) { go func() { s.mu.Lock(); defer s.mu.Unlock(); f() }() }

func (s *Server) ext(email string) *profile {
	if email != "" {
		return s.profiles[email]
	}
	keys := make([]string, 0, len(s.profiles))
	for k := range s.profiles {
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return nil
	}
	// "whichever is connected": Node's Map iteration is insertion order; here, the earliest connection
	slices.SortFunc(keys, func(a, b string) int { return s.profiles[a].seq - s.profiles[b].seq })
	return s.profiles[keys[0]]
}

// ---- HTTP: discovery endpoints for agent-browser, status for the CLI, WebSocket upgrades
var upgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }, ReadBufferSize: 1 << 16, WriteBufferSize: 1 << 16}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	prof := q.Get("profile")
	authed := q.Get("token") == s.token
	js := func(v any) { w.Header().Set("content-type", "application/json"); json.NewEncoder(w).Encode(v) }
	if websocket.IsWebSocketUpgrade(r) {
		s.accept(w, r)
		return
	}
	switch p := r.URL.Path; {
	case p == "/json/version":
		tok := ""
		if authed {
			tok = s.token
		}
		ws := fmt.Sprintf("ws://127.0.0.1:%d/devtools/browser/agent?token=%s", s.opt.Port, tok)
		if prof != "" {
			ws += "&profile=" + url.QueryEscape(prof)
		}
		js(map[string]any{"Browser": "Chrome/ChromeRelay", "Protocol-Version": "1.3", "webSocketDebuggerUrl": ws})
	case strings.HasPrefix(p, "/json"):
		js([]any{})
	case p == "/reload-extensions" && authed:
		s.mu.Lock()
		names := []string{}
		for k, pr := range s.profiles {
			s.extSend(pr, map[string]any{"op": "reload"})
			names = append(names, k)
		}
		s.mu.Unlock()
		js(map[string]any{"reloading": names})
	case p == "/versions":
		s.mu.Lock()
		chans := map[string]chan extReply{}
		for k, pr := range s.profiles {
			chans[k] = s.extSend(pr, map[string]any{"op": "version"})
		}
		s.mu.Unlock()
		out := map[string]string{}
		for k, ch := range chans {
			v := "0.1.0"
			select {
			case rep := <-ch:
				var x struct{ Version string }
				if json.Unmarshal(rep.Result, &x) == nil && x.Version != "" {
					v = x.Version
				}
			case <-time.After(2 * time.Second):
			}
			out[k] = v
		}
		js(out)
	case p == "/vault" && authed: // what agents may know: titles, websites, usernames; never values
		who := s.whoAt(r)
		sum, err := s.vault.Summarize(r.Context(), who)
		if err != nil {
			js(map[string]any{"error": err.Error()})
			return
		}
		js(sum)
	case p == "/status":
		s.mu.Lock()
		profs := make([]string, 0, len(s.profiles))
		for k := range s.profiles {
			profs = append(profs, k)
		}
		slices.SortFunc(profs, func(a, b string) int { return s.profiles[a].seq - s.profiles[b].seq })
		ags := []map[string]any{}
		for a := range s.agents {
			p := a.email
			if p == "" {
				p = "(default)"
			}
			ags = append(ags, map[string]any{"profile": p, "via": a.app, "tabs": len(a.tabs), "idleSeconds": int(time.Since(a.last).Seconds() + 0.5)})
		}
		s.mu.Unlock()
		js(map[string]any{"profiles": profs, "agents": ags})
	default:
		w.WriteHeader(404)
	}
}

// whoAt: the agent behind an HTTP request (the CLI it ran), for Touch ID prompts.
func (s *Server) whoAt(r *http.Request) vault.Who {
	_, port, _ := net.SplitHostPort(r.RemoteAddr)
	n, _ := strconv.Atoi(port)
	c := s.caller(n)
	return vault.Who{Name: describe(c.App, c.Cwd), Session: fmt.Sprintf("http-%d", c.PID)}
}

var appNames = map[string]string{"claude": "Claude Code", "codex": "Codex", "cursor": "Cursor", "paseo": "Paseo"}

// describe an agent for a prompt: "Claude Code (my-repo)".
func describe(app, cwd string) string {
	name := appNames[app]
	if name == "" {
		name = app
	}
	if name == "" {
		name = "an agent"
	}
	if cwd != "" {
		name += " (" + filepath.Base(cwd) + ")"
	}
	return name
}

var extOrigin = regexp.MustCompile(`^chrome-extension://([a-p]{32})$`)

// accept a WebSocket: the extension (/extension) or an agent (/devtools/browser/agent).
func (s *Server) accept(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	ws.SetReadLimit(512 << 20)
	c := &conn{ws: ws, up: true}
	q := r.URL.Query()
	prof := q.Get("profile")
	_, portStr, _ := net.SplitHostPort(r.RemoteAddr)
	port, _ := strconv.Atoi(portStr)
	s.mu.Lock()
	s.seq++
	seq := s.seq // arrival order, taken before any wait
	s.mu.Unlock()
	// Hold anything the client sends while we check who it is.
	early := make(chan []byte, 1024)
	gone := make(chan struct{})
	go func() {
		defer close(gone)
		for {
			_, b, err := ws.ReadMessage()
			if err != nil {
				return
			}
			early <- b
		}
	}()
	if r.URL.Path == "/extension" {
		s.acceptExtension(c, r, prof, port, seq, early, gone)
		return
	}
	s.acceptAgent(c, r, prof, port, early, gone)
}

func closed(gone chan struct{}) bool {
	select {
	case <-gone:
		return true
	default:
		return false
	}
}

func (s *Server) acceptExtension(c *conn, r *http.Request, prof string, port, seq int, early chan []byte, gone chan struct{}) {
	origin := r.Header.Get("Origin")
	m := extOrigin.FindStringSubmatch(origin)
	if m == nil || !slices.Contains(s.allowedExtensionIDs(), m[1]) {
		if origin == "" {
			origin = "(none)"
		}
		s.log("refused extension connection from origin", origin)
		c.close(4003, "not the Chrome Relay extension")
		return
	}
	who := s.caller(port)
	if who.PID == 0 || !s.isChrome(who.PID) {
		s.log("refused extension connection not from Chrome")
		c.close(4003, "not Chrome")
		return
	}
	if closed(gone) {
		s.log("extension disconnected during connection check")
		return
	}
	s.mu.Lock()
	// One socket per profile, decided by arrival order (this check may finish after a later connection's):
	//  - same extension install reconnecting: the NEWER connection wins and the older one's waiting calls fail fast;
	//  - a different install claiming the same profile email (two Chrome profiles on one account, or no email):
	//    the one already connected stays, so the two never knock each other off in a loop.
	client := r.URL.Query().Get("client")
	if prev := s.profiles[prof]; prev != nil && prev.conn.open() {
		if prev.client != client {
			s.mu.Unlock()
			s.log("refused a second Chrome profile claiming", prof)
			c.close(4009, "another Chrome profile is already connected as "+prof)
			return
		}
		if prev.seq > seq {
			s.mu.Unlock()
			s.log("dropped a stale extension handshake for", prof)
			c.close(4000, "superseded by a newer connection")
			return
		}
		go prev.conn.close(4000, "replaced by a newer connection")
	}
	if last, ok := s.last[prof]; ok && last != client {
		s.log("a different Chrome profile now serves", prof, "- dropping the previous one's tabs")
		for a := range s.agents {
			if a.email == prof { // only agents on this profile
				for _, t := range a.tabIDs() {
					a.onDetached(t, detach{})
				}
				a.orphans = nil
			}
		}
	}
	s.last[prof] = client
	ctx, stop := context.WithCancel(context.Background())
	p := &profile{conn: c, pending: map[int]chan extReply{}, seq: seq, client: client, prof: prof, stop: stop}
	s.profiles[prof] = p
	s.log("extension connected for", prof)
	config := func() {
		s.extSend(p, map[string]any{"op": "config", "siteAccounts": s.siteAccounts(), "instance": s.instance})
	}
	config()
	s.mu.Unlock()
	s.watchers.Add(1)
	go func() { // ping every 20s; re-send config when identities.json changes (polled every 2s, like fs.watchFile)
		defer s.watchers.Add(-1)
		ids := filepath.Join(s.opt.Home, "identities.json")
		mtime := func() time.Time {
			st, err := os.Stat(ids)
			if err != nil {
				return time.Time{}
			}
			return st.ModTime()
		}
		seen, tick, ping := mtime(), time.NewTicker(s.watchEvery), time.NewTicker(20*time.Second)
		defer tick.Stop()
		defer ping.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ping.C:
				s.mu.Lock()
				if c.open() {
					s.extSend(p, map[string]any{"op": "ping"})
				}
				s.mu.Unlock()
			case <-tick.C:
				if t := mtime(); !t.Equal(seen) {
					seen = t
					s.mu.Lock()
					if c.open() {
						config()
					}
					s.mu.Unlock()
				}
			}
		}
	}()
	for {
		var b []byte
		select {
		case b = <-early:
		case <-gone:
			select { // drain what arrived before the close
			case b = <-early:
			default:
				s.mu.Lock()
				stop()
				if s.profiles[prof] == p {
					delete(s.profiles, prof)
				}
				s.log("extension gone for", prof)
				for id, ch := range p.pending {
					delete(p.pending, id)
					ch <- extReply{Error: "extension disconnected"}
				}
				c.mu.Lock()
				c.up = false
				c.mu.Unlock()
				s.mu.Unlock()
				return
			}
		}
		s.mu.Lock()
		go func() { defer s.mu.Unlock(); s.onExtension(p, b) }()
	}
}

// onExtension: one message from the extension. Called with s.mu held.
func (s *Server) onExtension(p *profile, b []byte) {
	var m struct {
		ID     int             `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  string          `json:"error"`
		Event  *extEvent       `json:"event"`
		Auth   *struct {
			TabID                        int `json:"tabId"`
			Site, Account, Action, Error string
			NeedsUser                    string `json:"needsUser"`
			URL                          string `json:"url"`
		} `json:"auth"`
		Paused *struct {
			TabID  int    `json:"tabId"`
			Reason string `json:"reason"`
		} `json:"paused"`
		Reattached *struct {
			TabID int `json:"tabId"`
		} `json:"reattached"`
		Adopted *struct {
			TabID       int `json:"tabId"`
			OpenerTabID int `json:"openerTabId"`
		} `json:"adopted"`
		Detached *struct {
			TabID       int `json:"tabId"`
			Reason, URL string
		} `json:"detached"`
	}
	if json.Unmarshal(b, &m) != nil {
		return
	}
	switch {
	case m.ID != 0:
		if ch := p.pending[m.ID]; ch != nil {
			delete(p.pending, m.ID)
			ch <- extReply{Result: m.Result, Error: m.Error}
		}
	case m.Event != nil:
		for a := range s.agents {
			if a.onExtEvent(*m.Event) {
				break
			}
		}
	case m.Auth != nil:
		a := m.Auth
		if a.Action != "" {
			for ag := range s.agents {
				if ag.tabs[a.TabID] {
					s.audit(ag, "google sign-in click", "tab", a.TabID, "clicked", a.Action, "site", a.Site, "account", a.Account)
				}
			}
			site := a.Site
			if site == "" {
				site = "?"
			}
			s.log("sign-in hop: tab", a.TabID, "clicked", a.Action, "for", site)
			if strings.HasPrefix(a.Action, "account ") {
				s.learn(a.Site, a.Account)
			}
		}
		if a.Error != "" {
			s.log("sign-in hop: tab", a.TabID, "helper error:", a.Error)
		}
		if a.NeedsUser != "" {
			s.log("sign-in hop: tab", a.TabID, "needs you:", a.NeedsUser, a.Site)
			s.notes[a.TabID] = authNote{a.TabID, a.Site, a.Account, a.NeedsUser, a.URL}
		}
	case m.Paused != nil:
		s.log("tab", m.Paused.TabID, "paused:", m.Paused.Reason)
		for a := range s.agents {
			if a.onPaused(m.Paused.TabID) {
				break
			}
		}
	case m.Reattached != nil:
		s.log("tab", m.Reattached.TabID, "re-attached")
		for a := range s.agents {
			a.onReattached(m.Reattached.TabID)
		}
	case m.Adopted != nil:
		for a := range s.agents {
			if a.adopt(m.Adopted.TabID, m.Adopted.OpenerTabID) {
				return
			}
		}
		s.extSend(p, map[string]any{"op": "close", "tabId": m.Adopted.TabID})
	case m.Detached != nil:
		for a := range s.agents {
			a.onDetached(m.Detached.TabID, detach{m.Detached.Reason, m.Detached.URL})
		}
	}
}

func (s *Server) acceptAgent(c *conn, r *http.Request, prof string, port int, early chan []byte, gone chan struct{}) {
	if o := r.Header.Get("Origin"); o != "" {
		s.log("refused agent connection from a web page:", o)
		c.close(4003, "Chrome Relay does not accept connections from web pages")
		return
	}
	if r.URL.Query().Get("token") != s.token {
		s.log("refused agent: missing or wrong secret")
		c.close(4001, "Chrome Relay: wrong or missing secret; get the URL from `chrome-relay url`")
		return
	}
	t0 := time.Now()
	who := s.caller(port)
	if who.App == "" {
		what := who.Command
		if what == "" {
			what = strconv.Itoa(who.PID)
		}
		s.log("refused agent: not started by Claude Code, Codex, Cursor or Paseo:", what)
		c.close(4003, "Chrome Relay only accepts Claude Code, Codex, Cursor and Paseo")
		return
	}
	if closed(gone) {
		s.log("agent disconnected during connection check")
		return
	}
	b := make([]byte, 3)
	rand.Read(b)
	s.mu.Lock()
	a := &Agent{s: s, conn: c, email: prof, app: who.App, cwd: who.Cwd, sid: hex.EncodeToString(b), last: time.Now()}
	a.init()
	s.agents[a] = true
	s.audit(a, "session start", "pid", who.PID, "claudeSession", who.ClaudeSession, "paseoAgent", who.PaseoAgent, "codexThread", who.CodexThread,
		"agentBrowserSession", who.BrowserSession, "cwd", who.Cwd)
	p := prof
	if p == "" {
		p = "(default)"
	}
	s.log("agent connected", p, "via", who.App, fmt.Sprintf("(check %dms)", time.Since(t0).Milliseconds()))
	s.mu.Unlock()
	for {
		var b []byte
		select {
		case b = <-early:
		case <-gone:
			select {
			case b = <-early:
			default:
				s.mu.Lock()
				c.mu.Lock()
				c.up = false
				c.mu.Unlock()
				s.audit(a, "session end", "tabs", len(a.tabs))
				delete(s.agents, a)
				a.close()
				s.mu.Unlock()
				return
			}
		}
		var m message
		if json.Unmarshal(b, &m) != nil {
			continue
		}
		s.mu.Lock() // taken here, in arrival order; released by the handler
		go func() { defer s.mu.Unlock(); a.onMessage(m) }()
	}
}

// sweep: an agent silent for 30 minutes has almost certainly finished or died without `agent-browser close`;
// close its tabs so they don't pile up in the Agents group.
func (s *Server) sweep() {
	for range time.Tick(time.Minute) {
		s.mu.Lock()
		for a := range s.agents {
			if time.Since(a.last) > 30*time.Minute && (len(a.tabs) > 0 || len(a.orphans) > 0) {
				blocked := a.orphans // tabs stuck on a sign-in page: already reported to the agent, still open
				a.orphans = nil
				s.log("closing idle agent tabs", a.app, a.email)
				s.audit(a, "idle tabs closed", "tabs", len(a.tabs)+len(blocked))
				p := a.p()
				for _, t := range a.tabIDs() {
					a.onDetached(t, detach{}) // the agent is told each tab is gone
					if p != nil {
						s.extSend(p, map[string]any{"op": "close", "tabId": t})
					}
				}
				for _, t := range blocked {
					if p != nil {
						s.extSend(p, map[string]any{"op": "close", "tabId": t})
					}
				}
			}
		}
		s.mu.Unlock()
	}
}
