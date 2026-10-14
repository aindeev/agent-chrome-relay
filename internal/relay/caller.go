package relay

import (
	"cmp"
	"context"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Caller: which program is on the other end of a local TCP connection, and which agent app started it.
type Caller struct {
	PID                                                         int
	App, Command                                                string
	ClaudeSession, PaseoAgent, Cwd, CodexThread, BrowserSession string
}

var agentEnv = []struct {
	app string
	re  *regexp.Regexp
}{{"claude", regexp.MustCompile(`^(CLAUDECODE|CLAUDE_CODE_[A-Z_]+)=`)}, {"paseo", regexp.MustCompile(`^PASEO_AGENT_ID=`)},
	{"codex", regexp.MustCompile(`^CODEX_[A-Z_]+=`)}, {"cursor", regexp.MustCompile(`^CURSOR_[A-Z_]+=`)},
	{"pi", regexp.MustCompile(`^PI_SESSION_ID=`)}} // pi harness: PI_SESSION_ID is injected into every tool call
var agentProc = regexp.MustCompile(`(?i)(^|/)(claude|codex|cursor-agent|Cursor|Paseo)( |$|\.app)`)

func sh(name string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return ""
	}
	return string(out)
}

// findCaller looks the connection up with lsof, reads the process's environment (ps eww) for the markers the
// agent apps set, and failing that walks up the process tree. noTree: tests, markers only.
func findCaller(remotePort int, noTree bool) Caller {
	pid := 0
	for _, l := range strings.Split(sh("lsof", "-nP", "-Fp", "-iTCP@127.0.0.1:"+strconv.Itoa(remotePort)), "\n") {
		if strings.HasPrefix(l, "p") {
			if n, _ := strconv.Atoi(l[1:]); n != os.Getpid() && n != 0 {
				pid = n
				break
			}
		}
	}
	if pid == 0 {
		return Caller{}
	}
	env := strings.Fields(sh("ps", "eww", "-o", "command=", "-p", strconv.Itoa(pid)))
	pick := func(k string) string {
		for _, v := range env {
			if strings.HasPrefix(v, k+"=") {
				return v[len(k)+1:]
			}
		}
		return ""
	}
	c := Caller{PID: pid, ClaudeSession: pick("CLAUDE_CODE_SESSION_ID"), PaseoAgent: pick("PASEO_AGENT_ID"), Cwd: pick("PASEO_AGENT_CWD"),
		CodexThread: pick("CODEX_THREAD_ID"), BrowserSession: cmp.Or(pick("CHROME_RELAY_SESSION"), pick("AGENT_BROWSER_SESSION"))}
	if c.Cwd == "" {
		c.Cwd = pick("PWD")
	}
	if len(env) > 0 {
		c.Command = strings.Join(env[:min(2, len(env))], " ")
	}
	for _, a := range agentEnv {
		for _, v := range env {
			if a.re.MatchString(v) {
				c.App = a.app
				return c
			}
		}
	}
	if noTree {
		return c
	}
	p := pid // no markers: walk up the process tree
	for i := 0; i < 12 && p > 1; i++ {
		f := strings.Fields(sh("ps", "-o", "ppid=,command=", "-p", strconv.Itoa(p)))
		if len(f) == 0 {
			break
		}
		if m := agentProc.FindStringSubmatch(strings.Join(f[1:], " ")); m != nil {
			c.App = strings.ToLower(m[2])
			return c
		}
		p, _ = strconv.Atoi(f[0])
	}
	return c
}

func isChrome(pid int) bool {
	cmd := sh("ps", "-o", "command=", "-p", strconv.Itoa(pid))
	// macOS: "Google Chrome"; Linux: the binary is .../google/chrome/chrome
	return strings.Contains(cmd, "Google Chrome") || strings.Contains(cmd, "/google/chrome") || strings.Contains(cmd, "/chrome/chrome")
}
