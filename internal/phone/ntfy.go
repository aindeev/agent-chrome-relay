package phone

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Transport carries bytes between the Mac and the phone. Nothing it does can approve anything.
type Transport interface {
	Publish(ctx context.Context, topic string, body []byte, headers map[string]string) error
	// Subscribe delivers each message on topic published since `since`, until ctx ends.
	Subscribe(ctx context.Context, topic string, since time.Time) (<-chan []byte, error)
	// Poll returns the messages the topic still holds, oldest first.
	Poll(ctx context.Context, topic string) ([][]byte, error)
}

// Ntfy is an ntfy server: https://ntfy.sh, or your own.
type Ntfy struct {
	Server string
	Client *http.Client
}

func (n Ntfy) client() *http.Client {
	if n.Client != nil {
		return n.Client
	}
	return http.DefaultClient
}

func (n Ntfy) Publish(ctx context.Context, topic string, body []byte, headers map[string]string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(n.Server, "/")+"/"+topic, bytes.NewReader(body))
	if err != nil {
		return err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := n.client().Do(req)
	if err != nil {
		return err
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", n.Server, res.Status)
	}
	return nil
}

func (n Ntfy) Subscribe(ctx context.Context, topic string, since time.Time) (<-chan []byte, error) {
	u := fmt.Sprintf("%s/%s/json?since=%d", strings.TrimRight(n.Server, "/"), topic, since.Unix())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	res, err := n.client().Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		res.Body.Close()
		return nil, fmt.Errorf("%s: %s", n.Server, res.Status)
	}
	out := make(chan []byte)
	go func() {
		defer close(out)
		defer res.Body.Close()
		sc := bufio.NewScanner(res.Body)
		sc.Buffer(make([]byte, 64<<10), 64<<10)
		for sc.Scan() {
			var m struct {
				Event   string `json:"event"`
				Message string `json:"message"`
			}
			if json.Unmarshal(sc.Bytes(), &m) != nil || m.Event != "message" {
				continue // open, keepalive
			}
			select {
			case out <- []byte(m.Message):
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

func (n Ntfy) Poll(ctx context.Context, topic string) ([][]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(n.Server, "/")+"/"+topic+"/json?poll=1&since=all", nil)
	if err != nil {
		return nil, err
	}
	res, err := n.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	var out [][]byte
	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 64<<10), 64<<10)
	for sc.Scan() {
		var m struct {
			Event   string `json:"event"`
			Message string `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &m) == nil && m.Event == "message" {
			out = append(out, []byte(m.Message))
		}
	}
	return out, sc.Err()
}
