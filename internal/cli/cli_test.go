package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aindeev/agent-chrome-relay/internal/macos"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestExtensionIDMatchesPython(t *testing.T) {
	// Python: hashlib.sha256(os.path.realpath(p).encode()).hexdigest()[:32], 0-f mapped to a-p
	for path, want := range map[string]string{
		"/Users/someone/src/agent-chrome-relay/extension":     "fainbmaedfeaahmpdkmoiemeafoiiooo",
		"/Users/someone/src/agent-chrome-relay-dev/extension": "cjajjckcnconjbhjmigfpjooghcoiegf",
	} {
		if got := extensionID(path); got != want {
			t.Errorf("extensionID(%s) = %s, want %s", path, got, want)
		}
	}
}

func TestExtensionIDResolvesSymlinks(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	os.Mkdir(real, 0o755)
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skip(err)
	}
	if extensionID(link) != extensionID(real) {
		t.Error("a symlinked checkout must get the same ID as its real path")
	}
}

func TestDevMode(t *testing.T) {
	cases := []struct {
		args []string
		dev  bool
		rest []string
	}{
		{[]string{"/x/chrome-relay", "url"}, false, []string{"/x/chrome-relay", "url"}},
		{[]string{"/x/chrome-relay", "--dev", "url"}, true, []string{"/x/chrome-relay", "url"}},
		{[]string{"/x/chrome-relay", "setup", "--dev"}, true, []string{"/x/chrome-relay", "setup"}},
		{[]string{"/home/me/.local/bin/chrome-relay-dev-mode", "doctor"}, true, []string{"/home/me/.local/bin/chrome-relay-dev-mode", "doctor"}},
	}
	for _, c := range cases {
		dev, rest := devMode(c.args)
		if dev != c.dev || strings.Join(rest, " ") != strings.Join(c.rest, " ") {
			t.Errorf("devMode(%v) = %v %v, want %v %v", c.args, dev, rest, c.dev, c.rest)
		}
	}
}

func TestInstanceNaming(t *testing.T) {
	n := newInstance(false, env(nil), "/Users/me", "/src/cr/chrome-relay")
	if n.Name != "chrome-relay" || n.Home != "/Users/me/.chrome-relay" || n.Port != 9333 || n.Label != "com.chrome-relay" ||
		n.Plist != "/Users/me/Library/LaunchAgents/com.chrome-relay.plist" || n.SkillLink != "/Users/me/.claude/skills/chrome-relay" ||
		n.Tool != "/src/cr" || n.Ext != "/src/cr/extension" || n.vaultInstance() != "" {
		t.Errorf("normal instance: %+v", n)
	}
	d := newInstance(true, env(nil), "/Users/me", "/src/wt/chrome-relay")
	if d.Name != "chrome-relay-dev-mode" || d.Home != "/Users/me/.chrome-relay-dev-mode" || d.Port != 9334 ||
		d.Label != "com.chrome-relay-dev-mode" || d.Plist != "/Users/me/Library/LaunchAgents/com.chrome-relay-dev-mode.plist" ||
		d.SkillLink != "" || d.vaultInstance() != "dev-mode" {
		t.Errorf("dev instance: %+v", d)
	}
	o := newInstance(true, env(map[string]string{"CHROME_RELAY_HOME": "/tmp/h", "CHROME_RELAY_PORT": "9400"}), "/Users/me", "/src/wt/chrome-relay")
	if o.Home != "/tmp/h" || o.Port != 9400 {
		t.Errorf("env overrides: %+v", o)
	}
	opts := o.serveOptions("v1")
	if opts.Home != "/tmp/h" || opts.Port != 9400 || opts.Instance != "dev-mode" || opts.Version != "v1" {
		t.Errorf("serve options: %+v", opts)
	}
}

func TestPlist(t *testing.T) {
	n := newInstance(false, env(nil), "/Users/me", "/src/cr/chrome-relay")
	want := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>com.chrome-relay</string>
  <key>ProgramArguments</key><array><string>/src/cr/chrome-relay</string><string>serve</string></array>
  <key>WorkingDirectory</key><string>/src/cr</string>
  <key>EnvironmentVariables</key><dict><key>CHROME_RELAY_HOME</key><string>/Users/me/.chrome-relay</string><key>CHROME_RELAY_PORT</key><string>9333</string></dict>
  <key>RunAtLoad</key><true/><key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>/Users/me/.chrome-relay/relay.out</string><key>StandardErrorPath</key><string>/Users/me/.chrome-relay/relay.out</string>
</dict></plist>
`
	if got := n.plist(); got != want {
		t.Errorf("plist:\n%s\nwant:\n%s", got, want)
	}
	d := newInstance(true, env(nil), "/Users/me", "/src/a&b/chrome-relay")
	p := d.plist()
	for _, s := range []string{
		"<array><string>/src/a&amp;b/chrome-relay</string><string>serve</string><string>--dev</string></array>",
		"<key>CHROME_RELAY_PORT</key><string>9334</string><key>CHROME_RELAY_INSTANCE</key><string>dev-mode</string></dict>",
		"<key>Label</key><string>com.chrome-relay-dev-mode</string>",
	} {
		if !strings.Contains(p, s) {
			t.Errorf("dev plist lacks %s:\n%s", s, p)
		}
	}
	// setup --dev refuses when the normal LaunchAgent runs this checkout, new binary or old Node relay
	other := newInstance(true, env(nil), "/Users/me", "/src/cr/chrome-relay")
	if !other.runsThisCheckout(n.plist()) {
		t.Error("should see the normal plist runs this checkout")
	}
	if !other.runsThisCheckout("<string>/usr/bin/node</string><string>/src/cr/relay/relay.mjs</string>") {
		t.Error("should see the old Node plist runs this checkout")
	}
	if d.runsThisCheckout(n.plist()) {
		t.Error("another checkout is not this one")
	}
}

func TestAuditLine(t *testing.T) {
	// reference output from the Python CLI
	line := `{"t":"2026-10-10T17:25:01.123Z","s":"s1","via":"Claude Code","profile":"a@b.com","event":"open","url":"https://x","n":3,"skip":null,"ok":true,"list":["a",1]}`
	want := "17:25:01 s1 Claude Code a@b.com                    open                   url=https://x n=3 ok=True list=['a', 1]"
	got, ok := auditLine(line)
	if !ok || got != want {
		t.Errorf("auditLine:\n%q\nwant\n%q", got, want)
	}
	if _, ok := auditLine("not json"); ok {
		t.Error("bad lines are skipped")
	}
}

func TestAuditArgs(t *testing.T) {
	for _, c := range []struct {
		args   []string
		n      int
		filter string
	}{
		{nil, 40, ""},
		{[]string{"10"}, 10, ""},
		{[]string{"github.com"}, 40, "github.com"},
		{[]string{"github.com", "5"}, 5, "github.com"},
		{[]string{"5", "x", "7", "y"}, 5, "x"},
	} {
		if n, f := auditArgs(c.args); n != c.n || f != c.filter {
			t.Errorf("auditArgs(%v) = %d %q, want %d %q", c.args, n, f, c.n, c.filter)
		}
	}
}

func TestPythonFormatting(t *testing.T) {
	for in, want := range map[string]string{
		"1.0": "1.0", "0.1": "0.1", "1.5e-05": "1.5e-05", "1e16": "1e+16", "123456789012345.6": "123456789012345.6",
		"1e-4": "0.0001", "2.5e20": "2.5e+20", "-3.25": "-3.25", "100.0": "100.0", "42": "42",
	} {
		if got := pyNumber(json.Number(in)); got != want {
			t.Errorf("pyNumber(%s) = %s, want %s", in, got, want)
		}
	}
	v, _ := parseJSON([]byte(`["x",1,true,null,{"k":1.5}]`))
	if got := pyRepr(v); got != `['x', 1, True, None, {'k': 1.5}]` {
		t.Errorf("pyRepr list = %s", got)
	}
	if got := pyRepr("it's") + " " + pyRepr("a\"b'c\n"); got != `"it's" 'a"b\'c\n'` {
		t.Errorf("pyQuote = %s", got)
	}
	if got := pyFormat(true, 6) + "|" + pyFormat(json.Number("3"), 6) + "|" + pyFormat("ab", 6) + "|"; got != "     1|     3|ab    |" {
		t.Errorf("pyFormat = %q", got)
	}
}

func TestOrderedJSONRoundTrip(t *testing.T) {
	in := `{"profiles":{"z@x.com":{"chromeProfile":"Default","use":"work <main>"},"a@x.com":{"chromeProfile":"Profile 1","use":""}},"sites":{"rules":{},"learned":{"b.com":"z@x.com"}},"n":1.50}`
	v, err := parseJSON([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "identities.json")
	if err := writeJSON(path, v); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	var want bytes.Buffer
	json.Indent(&want, []byte(in), "", "  ")
	if string(got) != want.String() {
		t.Errorf("round trip changed the file:\n%s\nwant\n%s", got, want.String())
	}
	o := v.(*object)
	o.set("profiles", o.get("profiles")) // existing keys keep their place
	o.set("new", "x")
	o.del("sites")
	if strings.Join(o.keys, ",") != "profiles,n,new" {
		t.Errorf("keys = %v", o.keys)
	}
}

func TestStatusIndentLikePython(t *testing.T) {
	body := `{"profiles":["a@b.com"],"agents":[{"profile":"a@b.com","via":"Claude Code","tabs":2,"idleSeconds":5}],"e":[]}`
	want := `{
 "profiles": [
  "a@b.com"
 ],
 "agents": [
  {
   "profile": "a@b.com",
   "via": "Claude Code",
   "tabs": 2,
   "idleSeconds": 5
  }
 ],
 "e": []
}`
	var b bytes.Buffer
	json.Indent(&b, []byte(body), "", " ")
	if b.String() != want {
		t.Errorf("status:\n%s", b.String())
	}
}

func TestAgentURL(t *testing.T) {
	if got := agentURL(9334, "abc", "me+1@x.com"); got != "ws://127.0.0.1:9334/devtools/browser/agent?token=abc&profile=me%2B1%40x.com" {
		t.Error(got)
	}
	if got := agentURL(9333, "abc", ""); got != "ws://127.0.0.1:9333/devtools/browser/agent?token=abc" {
		t.Error(got)
	}
}

func TestParseCodesign(t *testing.T) {
	signed := `Executable=/src/cr/chrome-relay
Identifier=chrome-relay
Format=Mach-O thin (arm64)
CodeDirectory v=20500 size=1 flags=0x10000(runtime) hashes=1+2 location=embedded
Signature size=4790
Authority=Apple Development: someone@example.com (ABCDE12345)
Authority=Apple Worldwide Developer Relations Certification Authority
Authority=Apple Root CA
TeamIdentifier=ABCDE12345
`
	s := parseCodesign(signed)
	if s.Authority != "Apple Development: someone@example.com (ABCDE12345)" || !s.Runtime || s.Adhoc {
		t.Errorf("signed: %+v", s)
	}
	adhoc := `Executable=/src/cr/chrome-relay
CodeDirectory v=20400 size=1 flags=0x20002(adhoc,linker-signed) hashes=1+0 location=embedded
Signature=adhoc
TeamIdentifier=not set
`
	if s := parseCodesign(adhoc); !s.Adhoc || s.Runtime || s.Authority != "" {
		t.Errorf("adhoc: %+v", s)
	}
	if s := parseCodesign("/x: code object is not signed at all\n"); s.Adhoc || s.Runtime || s.Authority != "" {
		t.Errorf("unsigned: %+v", s)
	}
}

func TestVaultListFormat(t *testing.T) {
	// one row as Python printed it: title[:30]:30, websites[:40]:40, username[:28]:28, then what it holds
	got := "  " + pyFormat(cut("GitHub", 30), 30) + " " + pyFormat(cut("https://github.com", 40), 40) + " " + pyFormat(cut("bot@x.com", 28), 28) + " " + "password, one-time code"
	want := "  GitHub                         https://github.com                       bot@x.com                    password, one-time code"
	if got != want {
		t.Errorf("\n%q\n%q", got, want)
	}
}

func TestPasskeysCommandWritesConfig(t *testing.T) {
	home := t.TempDir()
	os.WriteFile(filepath.Join(home, "config.json"), []byte(`{"extensionIds":["x"],"onepassword":{"vault":"A"}}`), 0o600)
	run := func(args ...string) string {
		var out bytes.Buffer
		c := &cli{instance: instance{Name: "chrome-relay", Home: home}, args: append([]string{"chrome-relay", "passkeys"}, args...), out: &out, errw: &out}
		if err := c.run(); err != nil {
			t.Fatal(args, err)
		}
		return out.String()
	}
	if !macos.Linked { // the core-only build keeps no passkeys of its own
		if out := run("reuse", "120"); !strings.Contains(out, "on. ") || !strings.Contains(out, "1Password's prompt") {
			t.Fatal("core build:", out)
		}
	} else {
		if !strings.Contains(run(), "on. ") || !strings.Contains(run(), "each sign-in asks for Touch ID") {
			t.Fatal("on by default, Touch ID every time")
		}
		if !strings.Contains(run("reuse", "120"), "for 120 seconds") {
			t.Fatal("reuse window")
		}
	}
	if !strings.Contains(run("off"), "off.") {
		t.Fatal("off")
	}
	b, _ := os.ReadFile(filepath.Join(home, "config.json"))
	if !strings.Contains(string(b), `"passkeys": {`) || !strings.Contains(string(b), `"enabled": false`) || !strings.Contains(string(b), `"touchIdReuseSeconds": 120`) || !strings.Contains(string(b), `"onepassword"`) {
		t.Fatal("config keeps other keys and gains passkeys:", string(b))
	}
}
