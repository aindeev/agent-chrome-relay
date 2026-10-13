package decide

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Model answers a Request's questions.
type Model interface {
	Name() string
	Ask(ctx context.Context, r *Request) (map[string]Answer, error)
	Close()
}

// Jev is TypeSafe's decision model (https://docs.typesafe.ai): one request answers every question, ~150 ms.
type Jev struct {
	Key, Model, URL string
	Client          *http.Client
}

func (j *Jev) Name() string { return "jev" }
func (j *Jev) Close()       {}

func (j *Jev) Ask(ctx context.Context, r *Request) (map[string]Answer, error) {
	model, url := j.Model, j.URL
	if model == "" {
		model = "jev-latest"
	}
	if url == "" {
		url = "https://api.typesafe.ai/v1/systemone"
	}
	client := j.Client
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	body := map[string]any{"model": model}
	for k, v := range r.Body {
		body[k] = v
	}
	b, _ := json.Marshal(body)
	for attempt := 0; ; attempt++ {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+j.Key)
		req.Header.Set("Content-Type", "application/json")
		res, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("Jev: %w", err)
		}
		raw, _ := io.ReadAll(io.LimitReader(res.Body, 4<<20))
		res.Body.Close()
		if (res.StatusCode == 429 || res.StatusCode == 529 || res.StatusCode == 503) && attempt < 2 {
			time.Sleep(time.Duration(300<<attempt) * time.Millisecond)
			continue
		}
		if res.StatusCode == 401 {
			return nil, errors.New("Jev: the TypeSafe key was refused (`chrome-relay decider jev` sets a new one)")
		}
		if res.StatusCode != 200 {
			return nil, fmt.Errorf("Jev: HTTP %d: %s", res.StatusCode, strings.TrimSpace(string(raw[:min(len(raw), 200)])))
		}
		var out struct {
			Answers map[string]Answer `json:"answers"`
		}
		if err := json.Unmarshal(raw, &out); err != nil || out.Answers == nil {
			return nil, fmt.Errorf("Jev: unreadable answer")
		}
		return out.Answers, nil
	}
}

// Claude asks Claude Haiku through the person's own Claude Code (`claude -p`), kept running between decisions: a
// fresh process costs 4–8 s, a running one ~1–1.5 s. It's restarted every few decisions so pages don't blend.
type Claude struct {
	Path, Model string
	mu          sync.Mutex
	cmd         *exec.Cmd
	in          io.WriteCloser
	out         *bufio.Reader
	n           int
	dir         string
}

const claudeSystem = `You decide the next browser action for an automation. Each message holds STATE (the page) and QUESTIONS.
Answer every question with one of its allowed option ids (the keys of its "criteria"). Also say "confident": true only if
you are sure the operation and its target are right for the goal; false if a person should check.
Each message is a new, independent page. Reply with only JSON: {"<question id>": "<option id>", ..., "confident": true|false}`

func (c *Claude) Name() string { return "claude" }

func (c *Claude) start() error {
	c.stop()
	model := c.Model
	if model == "" {
		model = "haiku"
	}
	cmd := exec.Command(c.Path, "-p", "--model", model, "--system-prompt", claudeSystem, "--tools", "", "--strict-mcp-config",
		"--mcp-config", `{"mcpServers":{}}`, "--setting-sources", "", "--disable-slash-commands", "--no-session-persistence",
		"--no-chrome", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose")
	// its own Claude Code, not a child of the agent's session: without the session's markers, sockets and folder
	cmd.Env = childEnv(os.Environ())
	if dir, err := os.MkdirTemp("", "chrome-relay-decider-"); err == nil {
		cmd.Dir, c.dir = dir, dir
	}
	in, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("Claude Code: %w", err)
	}
	c.cmd, c.in, c.out, c.n = cmd, in, bufio.NewReaderSize(out, 1<<20), 0
	return nil
}

func (c *Claude) stop() {
	if c.cmd != nil {
		c.in.Close()
		c.cmd.Process.Kill()
		c.cmd.Wait()
		c.cmd = nil
	}
	if c.dir != "" {
		os.RemoveAll(c.dir)
		c.dir = ""
	}
}

// childEnv drops what ties a process to the Claude Code session running `chrome-relay do`.
func childEnv(env []string) []string {
	out := env[:0:0]
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		switch {
		case k == "CLAUDECODE", k == "CLAUDE_CODE_SESSION_ID", k == "CLAUDE_CODE_ENTRYPOINT", k == "CLAUDE_CODE_SSE_PORT",
			strings.HasPrefix(k, "CLAUDE_CODE_MESSAGING_"), strings.HasPrefix(k, "CLAUDE_CODE_TASK"), k == "CLAUDE_CODE_TMPDIR":
			continue
		}
		out = append(out, kv)
	}
	return out
}

func (c *Claude) Close() { c.mu.Lock(); defer c.mu.Unlock(); c.stop() }

var jsonObject = regexp.MustCompile(`(?s)\{.*\}`)

func (c *Claude) Ask(ctx context.Context, r *Request) (map[string]Answer, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cmd == nil || c.n >= 6 {
		if err := c.start(); err != nil {
			return nil, err
		}
	}
	c.n++
	text, _ := json.Marshal(map[string]any{"STATE": r.Body["state"], "QUESTIONS": r.Body["questions"]})
	msg, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": string(text)}}}})
	if _, err := c.in.Write(append(msg, '\n')); err != nil {
		c.stop()
		return nil, fmt.Errorf("Claude Code: %w", err)
	}
	type line struct {
		b   []byte
		err error
	}
	got := make(chan line, 1)
	go func() {
		for {
			b, err := c.out.ReadBytes('\n')
			if err != nil {
				got <- line{nil, err}
				return
			}
			var m struct {
				Type    string `json:"type"`
				Result  string `json:"result"`
				IsError bool   `json:"is_error"`
			}
			if json.Unmarshal(b, &m) == nil && m.Type == "result" {
				if m.IsError {
					got <- line{nil, fmt.Errorf("%s", trunc(m.Result, 200))}
				} else {
					got <- line{[]byte(m.Result), nil}
				}
				return
			}
		}
	}()
	var res line
	timer := time.NewTimer(30 * time.Second) // one decision; a stuck process is restarted next time
	defer timer.Stop()
	select {
	case res = <-got:
	case <-timer.C:
		c.stop()
		return nil, fmt.Errorf("Claude Code: no answer within 30s")
	case <-ctx.Done():
		c.stop()
		return nil, fmt.Errorf("Claude Code: %w", ctx.Err())
	}
	if res.err != nil {
		c.stop()
		return nil, fmt.Errorf("Claude Code: %v", res.err)
	}
	var picks map[string]any
	if err := json.Unmarshal(jsonObject.Find(res.b), &picks); err != nil {
		return nil, fmt.Errorf("Claude Code: unreadable answer %q", trunc(string(res.b), 120))
	}
	ans := map[string]Answer{}
	for q, v := range picks {
		if q != "confident" {
			ans[q] = Answer{Choice: fmt.Sprint(v), Confidence: 1}
		}
	}
	if sure, ok := picks["confident"].(bool); ok && !sure {
		op := ans["operation"]
		op.Confidence = 0.3
		ans["operation"] = op
	}
	return ans, nil
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
