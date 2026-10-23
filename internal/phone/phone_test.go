package phone

import (
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// board is an in-memory ntfy: every topic keeps its messages; subscribers get the old ones, then new ones.
type board struct {
	mu   sync.Mutex
	msgs map[string][][]byte
	subs map[string][]chan []byte
}

func newBoard() *board { return &board{msgs: map[string][][]byte{}, subs: map[string][]chan []byte{}} }

func (b *board) Publish(ctx context.Context, topic string, body []byte, _ map[string]string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.msgs[topic] = append(b.msgs[topic], body)
	for _, c := range b.subs[topic] {
		go func() { c <- body }()
	}
	return nil
}

func (b *board) Poll(ctx context.Context, topic string) ([][]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([][]byte(nil), b.msgs[topic]...), nil
}

func (b *board) Subscribe(ctx context.Context, topic string, _ time.Time) (<-chan []byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	c := make(chan []byte, 16)
	for _, m := range b.msgs[topic] {
		c <- m
	}
	b.subs[topic] = append(b.subs[topic], c)
	return c, nil
}

// softPhone does what the iPhone app does, with software keys.
type softPhone struct {
	sign  *ecdsa.PrivateKey
	box   *ecdh.PrivateKey
	t     Transport
	topic string
	relay []byte
}

func newPhone(t Transport) *softPhone {
	s, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	b, _ := ecdh.P256().GenerateKey(rand.Reader)
	return &softPhone{sign: s, box: b, t: t}
}

func (p *softPhone) signs(msg []byte) string {
	h := sha256.Sum256(msg)
	sig, _ := ecdsa.SignASN1(rand.Reader, p.sign, h[:])
	return base64.StdEncoding.EncodeToString(sig)
}

func (p *softPhone) pair(t *testing.T, link string, secretOverride []byte) {
	u, _ := url.Parse(link)
	q := u.Query()
	relay, _ := b64u.DecodeString(q.Get("k"))
	secret, _ := b64u.DecodeString(q.Get("p"))
	if secretOverride != nil {
		secret = secretOverride
	}
	p.topic, p.relay = q.Get("t"), relay
	pub, _ := p.sign.PublicKey.Bytes()
	body, _ := json.Marshal(pairBody{Sign: base64.StdEncoding.EncodeToString(pub), Device: "Test iPhone",
		Proof: p.signs(append([]byte(pairInfo+"\n"), secret...))})
	data, err := seal(p.box, relay, secret, pairInfo, body)
	if err != nil {
		t.Fatal(err)
	}
	msg, _ := json.Marshal(pairMessage{V: 1, Box: base64.StdEncoding.EncodeToString(p.box.PublicKey().Bytes()), Data: base64.StdEncoding.EncodeToString(data)})
	p.t.Publish(context.Background(), p.topic+"-pair", msg, nil)
}

// answer reads the next request, checks what it says, and approves (signed) or denies it.
func (p *softPhone) answer(t *testing.T, approve bool, forge bool) Request {
	reqs, _ := p.t.Subscribe(context.Background(), p.topic+"-req", time.Time{})
	raw := <-reqs
	var m requestMessage
	json.Unmarshal(raw, &m)
	box, _ := base64.StdEncoding.DecodeString(m.Data)
	plain, err := open(p.box, p.relay, []byte(m.ID), requestInfo, box)
	if err != nil {
		t.Error("phone can't read the request:", err)
		return Request{}
	}
	var r Request
	json.Unmarshal(plain, &r)
	a := answerMessage{V: 1, ID: m.ID, OK: approve}
	if approve {
		signer := p
		if forge {
			signer = newPhone(p.t)
		}
		a.Sig = signer.signs(approval(m.ID, plain))
	}
	out, _ := json.Marshal(a)
	p.t.Publish(context.Background(), p.topic+"-res", out, nil)
	return r
}

func paired(t *testing.T) (*Pairing, *softPhone, Transport) {
	tr := newBoard()
	o, _ := NewOffer("https://ntfy.example", "Test Mac")
	ph := newPhone(tr)
	ph.pair(t, o.URL(), nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	p, err := Pair(ctx, tr, o)
	if err != nil {
		t.Fatal(err)
	}
	return p, ph, tr
}

func TestPairAndApprove(t *testing.T) {
	p, ph, tr := paired(t)
	if p.Device != "Test iPhone" || p.Topic != ph.topic {
		t.Fatalf("%+v", p)
	}
	done := make(chan Request, 1)
	go func() { done <- ph.answer(t, true, false) }()
	err := p.Approve(context.Background(), tr, nil, "Test Mac", Details{Reason: `let Claude Code (repo) fill the password of "Twilio" on www.twilio.com`,
		Why: "Download March's invoice for the books", FromAgent: true}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if r := <-done; !strings.Contains(r.Reason, "Twilio") || r.Mac != "Test Mac" || !r.Expires.After(r.Created) ||
		r.Why != "Download March's invoice for the books" || r.NoWhy {
		t.Fatalf("phone saw %+v", r)
	}
}

// The agent's why, in its own words: Touch ID quotes it, and says so when an agent gave none.
func TestPromptQuotesWhy(t *testing.T) {
	d := Details{Reason: `let Claude Code (repo) sign in to www.twilio.com with "Twilio"`, FromAgent: true}
	if got := d.Prompt(); got != `let Claude Code (repo) sign in to www.twilio.com with "Twilio". It didn't say why` {
		t.Error(got)
	}
	d.Why = ShortWhy("  Download March's\ninvoice  for the books ")
	if got := d.Prompt(); got != `let Claude Code (repo) sign in to www.twilio.com with "Twilio". It says why: “Download March's invoice for the books”` {
		t.Error(got)
	}
	if got := (Details{Reason: "pair an iPhone"}).Prompt(); got != "pair an iPhone" { // the person's own: no why
		t.Error(got)
	}
	if got := []rune(ShortWhy(strings.Repeat("a", 300))); len(got) != 160 || got[159] != '…' {
		t.Error(len(got))
	}
}

func TestDeniedForgedAndTimedOut(t *testing.T) {
	p, ph, tr := paired(t)
	go ph.answer(t, false, false)
	if err := p.Approve(context.Background(), tr, nil, "Mac", Details{Reason: "x"}, time.Minute); !errors.Is(err, ErrDenied) {
		t.Fatal("deny:", err)
	}
	p2, ph2, tr2 := paired(t)
	go ph2.answer(t, true, true) // signed by some other key: ignored, so it times out
	if err := p2.Approve(context.Background(), tr2, nil, "Mac", Details{Reason: "x"}, 300*time.Millisecond); !errors.Is(err, ErrTimeout) {
		t.Fatal("forged:", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // approved at the Mac instead
	if err := p.Approve(ctx, tr, nil, "Mac", Details{Reason: "x"}, time.Minute); !errors.Is(err, context.Canceled) {
		t.Fatal("cancel:", err)
	}
	b := tr.(*board)
	b.mu.Lock()
	res := b.msgs[p.Topic+"-res"]
	b.mu.Unlock()
	var last answerMessage
	json.Unmarshal(res[len(res)-1], &last)
	if last.OK || last.ID == "" {
		t.Fatal("settled at the Mac: the phone is told, so the request leaves the app", string(res[len(res)-1]))
	}
}

func TestPairingNeedsTheQRSecret(t *testing.T) {
	tr := newBoard()
	o, _ := NewOffer("https://ntfy.example", "Mac")
	ph := newPhone(tr)
	ph.pair(t, o.URL(), []byte("guessed-secret!!"))
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := Pair(ctx, tr, o); err == nil {
		t.Fatal("paired without the secret")
	}
}

func TestNtfyTransport(t *testing.T) {
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			mu.Lock()
			got = append(got, r.URL.Path+" "+r.Header.Get("Title"))
			mu.Unlock()
			return
		}
		w.Write([]byte(`{"event":"open"}` + "\n" + `{"event":"message","message":"hello"}` + "\n"))
	}))
	defer srv.Close()
	n := Ntfy{Server: srv.URL}
	if err := n.Publish(context.Background(), "t-n", []byte("x"), map[string]string{"Title": "Chrome Relay"}); err != nil {
		t.Fatal(err)
	}
	c, err := n.Subscribe(context.Background(), "t-res", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if m := <-c; string(m) != "hello" {
		t.Fatal(string(m))
	}
	if got[0] != "/t-n Chrome Relay" {
		t.Fatal(got)
	}
}
