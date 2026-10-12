// Package phone lets a paired iPhone approve Chrome Relay's requests with Face ID, for when the person is away
// from the Mac.
//
// The phone app (ios/) holds two keys in its Secure Enclave: a signing key that only Face ID unlocks, and a
// key-agreement key for reading requests. Pairing happens at the Mac (`chrome-relay phone pair`, after Touch ID)
// through a QR code carrying a one-time secret, so an agent can't pair a phone of its own. After that, each
// approval is a request encrypted to the phone, naming the agent and what it wants, and an answer signed by the
// phone's Face ID key over that exact request. The relay checks the signature; the transport only carries bytes.
//
// The transport is ntfy (https://ntfy.sh, or your own server): three topics under a random name only the Mac and
// the phone know, "<topic>-pair", "<topic>-req" and "<topic>-res", plus "<topic>-n" for a notification without
// details. Anyone who learns the name can read ciphertext, or drop or delay answers; never approve.
package phone

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Pairing is everything the Mac keeps about its phone. It lives in Chrome Relay's own Keychain item, never in a
// file an agent could edit.
type Pairing struct {
	Server    string    `json:"server"`    // the ntfy server
	Topic     string    `json:"topic"`     // random; the topics are Topic+"-pair", "-req", "-res", "-n"
	RelayKey  []byte    `json:"relayKey"`  // the Mac's P-256 key-agreement private key
	PhoneSign []byte    `json:"phoneSign"` // the phone's Face ID signing key, public, X9.63
	PhoneBox  []byte    `json:"phoneBox"`  // the phone's key-agreement key, public, X9.63
	Device    string    `json:"device"`
	Paired    time.Time `json:"paired"`
	Push      *Device   `json:"push,omitempty"` // where Apple push reaches the phone, if it has push
}

var (
	ErrDenied  = errors.New("the person denied it on their iPhone")
	ErrTimeout = errors.New("no answer from the iPhone in time")
	ErrBadSig  = errors.New("an answer that the paired iPhone did not sign")
)

const (
	pairInfo    = "chrome-relay pair v1"
	requestInfo = "chrome-relay request v1"
	approveTag  = "chrome-relay approve v1"
	deviceInfo  = "chrome-relay device v1"
)

var b64u = base64.RawURLEncoding

// --- crypto, the same as the iPhone app's CryptoKit: P-256 ECDH, HKDF-SHA256, AES-GCM (nonce|ciphertext|tag) ---

func seal(priv *ecdh.PrivateKey, peer []byte, salt []byte, info string, plain []byte) ([]byte, error) {
	aead, err := sharedAEAD(priv, peer, salt, info)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	rand.Read(nonce)
	return aead.Seal(nonce, nonce, plain, nil), nil
}

func open(priv *ecdh.PrivateKey, peer []byte, salt []byte, info string, box []byte) ([]byte, error) {
	aead, err := sharedAEAD(priv, peer, salt, info)
	if err != nil {
		return nil, err
	}
	if len(box) < aead.NonceSize()+aead.Overhead() {
		return nil, errors.New("short message")
	}
	return aead.Open(nil, box[:aead.NonceSize()], box[aead.NonceSize():], nil)
}

func sharedAEAD(priv *ecdh.PrivateKey, peer []byte, salt []byte, info string) (cipher.AEAD, error) {
	pub, err := ecdh.P256().NewPublicKey(peer)
	if err != nil {
		return nil, err
	}
	secret, err := priv.ECDH(pub)
	if err != nil {
		return nil, err
	}
	key, err := hkdf.Key(sha256.New, secret, salt, info, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// verify: a CryptoKit P256.Signing signature (DER) over msg, by the X9.63 public key.
func verify(pub, msg, sig []byte) bool {
	k, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), pub)
	if err != nil {
		return false
	}
	h := sha256.Sum256(msg)
	return ecdsa.VerifyASN1(k, h[:], sig)
}

// approval is what the phone signs: the request's id and a hash of exactly what it was shown.
func approval(id string, plain []byte) []byte {
	h := sha256.Sum256(plain)
	return []byte(approveTag + "\n" + id + "\n" + hex.EncodeToString(h[:]))
}

func randomTopic() string {
	b := make([]byte, 16)
	rand.Read(b)
	return "cr" + strings.ToLower(strings.TrimRight(base32.StdEncoding.EncodeToString(b), "="))
}

// --- pairing ---

// Offer is the Mac's half of a pairing: shown as a QR code, used once.
type Offer struct {
	Server, Topic, Mac string
	key                *ecdh.PrivateKey
	secret             []byte
}

func NewOffer(server, mac string) (*Offer, error) {
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	secret := make([]byte, 16)
	rand.Read(secret)
	return &Offer{Server: strings.TrimRight(server, "/"), Topic: randomTopic(), Mac: mac, key: key, secret: secret}, nil
}

// URL is what the QR code holds; the iPhone's Camera opens it in the Chrome Relay app.
func (o *Offer) URL() string {
	q := url.Values{}
	q.Set("v", "1")
	q.Set("s", o.Server)
	q.Set("t", o.Topic)
	q.Set("k", b64u.EncodeToString(o.key.PublicKey().Bytes()))
	q.Set("p", b64u.EncodeToString(o.secret))
	q.Set("n", o.Mac)
	return "chromerelay://pair?" + q.Encode()
}

type pairMessage struct {
	V    int    `json:"v"`
	Box  string `json:"box,omitempty"`  // the phone's key-agreement public key
	Data string `json:"data,omitempty"` // sealed pairBody
	Ack  string `json:"ack,omitempty"`  // the Mac's answer
}

type pairBody struct {
	Sign   string  `json:"sign"`   // the phone's Face ID signing public key
	Device string  `json:"device"` // "Sam's iPhone"
	Proof  string  `json:"proof"`  // signature by that key over pairInfo + secret: Face ID works, and it holds the key
	Push   *Device `json:"push,omitempty"`
}

// Accept reads the phone's answer to the offer, if it is one: sealed with the offer's secret, and proving it holds
// its signing key.
func (o *Offer) Accept(raw []byte) (*Pairing, error) {
	var m pairMessage
	if err := json.Unmarshal(raw, &m); err != nil || m.V != 1 || m.Data == "" {
		return nil, errors.New("not a pairing answer")
	}
	box, err1 := base64.StdEncoding.DecodeString(m.Box)
	data, err2 := base64.StdEncoding.DecodeString(m.Data)
	if err1 != nil || err2 != nil {
		return nil, errors.New("not a pairing answer")
	}
	plain, err := open(o.key, box, o.secret, pairInfo, data)
	if err != nil {
		return nil, errors.New("a pairing answer without this QR code's secret")
	}
	var b pairBody
	if err := json.Unmarshal(plain, &b); err != nil {
		return nil, err
	}
	sign, err1 := base64.StdEncoding.DecodeString(b.Sign)
	proof, err2 := base64.StdEncoding.DecodeString(b.Proof)
	if err1 != nil || err2 != nil || !verify(sign, append([]byte(pairInfo+"\n"), o.secret...), proof) {
		return nil, errors.New("the phone did not prove it holds its signing key")
	}
	return &Pairing{Server: o.Server, Topic: o.Topic, RelayKey: o.key.Bytes(), PhoneSign: sign, PhoneBox: box,
		Device: b.Device, Paired: time.Now().UTC(), Push: b.Push}, nil
}

// Ack is the Mac's "paired" back to the phone.
func (o *Offer) Ack() []byte {
	h := sha256.Sum256(append([]byte("ack\n"), o.secret...))
	b, _ := json.Marshal(pairMessage{V: 1, Ack: base64.StdEncoding.EncodeToString(h[:])})
	return b
}

// --- requests ---

// Details is one approval as the person sees it: the sentence (Touch ID on the Mac shows it), and its parts for
// the phone's screen.
type Details struct {
	Reason string // "let Claude Code (my-repo) fill the password of "Twilio" on www.twilio.com"
	Agent  string // "Claude Code (my-repo)"
	Site   string // "www.twilio.com"
	What   string // "password", "one-time code", "passkey as bot@acme.com"
}

// Request is what the phone shows: who asks, for what, until when.
type Request struct {
	ID      string    `json:"id"`
	Mac     string    `json:"mac"`
	Reason  string    `json:"reason"` // "let Claude Code (my-repo) fill the password of "Twilio" on www.twilio.com"
	Agent   string    `json:"agent,omitempty"`
	Site    string    `json:"site,omitempty"`
	What    string    `json:"what,omitempty"`
	Created time.Time `json:"created"`
	Expires time.Time `json:"expires"`
}

type requestMessage struct {
	V    int    `json:"v"`
	ID   string `json:"id"`
	Data string `json:"data"` // sealed Request JSON
}

type answerMessage struct {
	V   int    `json:"v"`
	ID  string `json:"id"`
	OK  bool   `json:"ok"`
	Sig string `json:"sig,omitempty"` // over approval(id, the request JSON), by the Face ID key
}

// sealRequest: the message for "<topic>-req", and the exact plaintext the answer must sign.
func (p *Pairing) sealRequest(r Request) (msg, plain []byte, err error) {
	key, err := ecdh.P256().NewPrivateKey(p.RelayKey)
	if err != nil {
		return nil, nil, err
	}
	plain, _ = json.Marshal(r)
	box, err := seal(key, p.PhoneBox, []byte(r.ID), requestInfo, plain)
	if err != nil {
		return nil, nil, err
	}
	msg, _ = json.Marshal(requestMessage{V: 1, ID: r.ID, Data: base64.StdEncoding.EncodeToString(box)})
	return msg, plain, nil
}

// check reads an answer: nil when it approves the request, ErrDenied, ErrBadSig, or errNotOurs when it is about
// another request.
func (p *Pairing) check(raw []byte, id string, plain []byte) error {
	var a answerMessage
	if err := json.Unmarshal(raw, &a); err != nil || a.ID != id {
		return errNotOurs
	}
	if !a.OK {
		return ErrDenied // unsigned: anyone with the topic can deny, never approve
	}
	sig, err := base64.StdEncoding.DecodeString(a.Sig)
	if err != nil || !verify(p.PhoneSign, approval(id, plain), sig) {
		return ErrBadSig
	}
	return nil
}

var errNotOurs = errors.New("not this request")

// Refresh reads the phone's latest push address ("<topic>-dev", sealed to the Mac: the app sends it when Apple
// gives it a new one). True when it changed, so the caller saves the pairing.
func (p *Pairing) Refresh(ctx context.Context, t Transport) bool {
	key, err := ecdh.P256().NewPrivateKey(p.RelayKey)
	if err != nil {
		return false
	}
	msgs, err := t.Poll(ctx, p.Topic+"-dev")
	if err != nil {
		return false
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		box, err := base64.StdEncoding.DecodeString(string(msgs[i]))
		if err != nil {
			continue
		}
		plain, err := open(key, p.PhoneBox, []byte(p.Topic), deviceInfo, box)
		if err != nil {
			continue
		}
		var d Device
		if json.Unmarshal(plain, &d) != nil || d.Token == "" {
			continue
		}
		if p.Push != nil && *p.Push == d {
			return false
		}
		p.Push = &d
		return true
	}
	return false
}

// Approve asks the phone and waits for its signed answer. The context ends the wait (someone approved at the Mac
// instead, or the relay gave up). With push (an APNs key, and a phone that gave its push address), Apple wakes
// the phone; otherwise ntfy's "<topic>-n" carries the notification.
func (p *Pairing) Approve(ctx context.Context, t Transport, push *APNs, mac string, d Details, ttl time.Duration) error {
	id := make([]byte, 16)
	rand.Read(id)
	now := time.Now().UTC().Truncate(time.Second)
	r := Request{ID: b64u.EncodeToString(id), Mac: mac, Reason: d.Reason, Agent: d.Agent, Site: d.Site, What: d.What, Created: now, Expires: now.Add(ttl)}
	msg, plain, err := p.sealRequest(r)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithDeadline(ctx, r.Expires)
	defer cancel()
	answers, err := t.Subscribe(ctx, p.Topic+"-res", now.Add(-time.Second))
	if err != nil {
		return err
	}
	if err := t.Publish(ctx, p.Topic+"-req", msg, nil); err != nil {
		return err
	}
	// details stay in the encrypted request; the notification only says there is one
	note := "An agent on " + mac + " is waiting for your approval."
	if push == nil || p.Push == nil || push.Push(ctx, *p.Push, "Chrome Relay", note, r.ID) != nil {
		t.Publish(ctx, p.Topic+"-n", []byte(note), map[string]string{
			"Title": "Chrome Relay", "Click": "chromerelay://open", "Priority": "high", "Tags": "key"})
	}
	for {
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return ErrTimeout
			}
			// settled at the Mac: take it off the phone (an unsigned answer, like a denial, approves nothing)
			done, _ := json.Marshal(answerMessage{V: 1, ID: r.ID, OK: false})
			bg, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			t.Publish(bg, p.Topic+"-res", done, nil)
			cancel()
			return ctx.Err()
		case raw, ok := <-answers:
			if !ok {
				return fmt.Errorf("lost the connection to %s", p.Server)
			}
			switch err := p.check(raw, r.ID, plain); err {
			case errNotOurs, ErrBadSig: // a stale or forged answer: keep waiting for the real one
				continue
			default:
				return err
			}
		}
	}
}

// Pair waits for the phone to answer an offer, then acknowledges it.
func Pair(ctx context.Context, t Transport, o *Offer) (*Pairing, error) {
	msgs, err := t.Subscribe(ctx, o.Topic+"-pair", time.Now().Add(-time.Minute))
	if err != nil {
		return nil, err
	}
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case raw, ok := <-msgs:
			if !ok {
				return nil, fmt.Errorf("lost the connection to %s", o.Server)
			}
			if bytes.Contains(raw, []byte(`"ack"`)) {
				continue // our own
			}
			p, err := o.Accept(raw)
			if err != nil {
				continue
			}
			if err := t.Publish(ctx, o.Topic+"-pair", o.Ack(), nil); err != nil {
				return nil, err
			}
			return p, nil
		}
	}
}
