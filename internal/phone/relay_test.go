package phone

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
	t.Setenv("CHROME_RELAY_PUSH_RELAY", "off")
	if PushRelay() != nil {
		t.Fatal("off")
	}
}
