package phone

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestThePushRelayGetsOnlyThePushAddress(t *testing.T) {
	var got map[string]any
	status, answer := 200, `{"ok":true}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/wake" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		got = nil
		json.Unmarshal(b, &got)
		w.WriteHeader(status)
		io.WriteString(w, answer)
	}))
	defer srv.Close()
	r := &Relay{URL: srv.URL}
	d := Device{Token: strings.Repeat("ab", 32), Topic: "dev.example.approve", Sandbox: true}
	if err := r.Push(context.Background(), d, "Chrome Relay", "An agent on Sam's MacBook Pro is waiting", "req_1"); err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 || got["token"] != d.Token || got["topic"] != d.Topic || got["sandbox"] != true || got["request"] != "req_1" {
		t.Fatalf("the relay is told where to push and which request, not what it says or which Mac: %v", got)
	}
	status, answer = 422, `{"ok":false,"reason":"Apple push: BadDeviceToken"}`
	if err := r.Push(context.Background(), d, "", "", "req_2"); err == nil || err.Error() != "push relay: Apple push: BadDeviceToken" {
		t.Fatal("the relay's reason comes back, so ntfy takes over:", err)
	}
}

func TestWhichPushRelay(t *testing.T) {
	if r := PushRelay(); r == nil || r.URL != DefaultPushRelay {
		t.Fatal("the app's relay by default:", r)
	}
	t.Setenv("CHROME_RELAY_PUSH_RELAY", "https://push.example.test/")
	if r := PushRelay(); r == nil || r.URL != "https://push.example.test" {
		t.Fatal("your own:", r)
	}
	if MessageServer() != "https://push.example.test" || !IsRelay("https://push.example.test/") || !IsRelay(DefaultPushRelay) || IsRelay("https://ntfy.sh") {
		t.Fatal("new pairings send their messages through the relay in use; ntfy servers are told apart")
	}
	t.Setenv("CHROME_RELAY_PUSH_RELAY", "off")
	if PushRelay() != nil || MessageServer() != "https://ntfy.sh" {
		t.Fatal("off: no relay, and new pairings use ntfy.sh")
	}
}

// TestLiveRelay checks a deployed relay with the Mac's own transport: CHROME_RELAY_LIVE_RELAY=https://… go test
// ./internal/phone -run TestLiveRelay. Skipped otherwise: tests never touch a real server by themselves.
func TestLiveRelay(t *testing.T) {
	server := os.Getenv("CHROME_RELAY_LIVE_RELAY")
	if server == "" {
		t.Skip("set CHROME_RELAY_LIVE_RELAY to a relay's URL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	n, topic := Ntfy{Server: server}, randomTopic()
	if err := n.Publish(ctx, topic+"-req", []byte("sealed request"), nil); err != nil {
		t.Fatal(err)
	}
	if got, err := n.Poll(ctx, topic+"-req"); err != nil || len(got) != 1 || string(got[0]) != "sealed request" {
		t.Fatalf("what was published is there to fetch: %q %v", got, err)
	}
	answers, err := n.Subscribe(ctx, topic+"-res", time.Now().Add(-time.Second))
	if err != nil {
		t.Fatal(err)
	}
	go n.Publish(ctx, topic+"-res", []byte("signed answer"), nil)
	select {
	case m := <-answers:
		if string(m) != "signed answer" {
			t.Fatal(string(m))
		}
	case <-ctx.Done():
		t.Fatal("the answer never reached the waiting Mac")
	}
	if got, _ := n.Poll(ctx, topic+"-dev"); len(got) != 0 {
		t.Fatal("another topic of the pairing is separate:", got)
	}
}
