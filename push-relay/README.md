# The relay

A Cloudflare Worker (`worker.mjs`, about 150 lines) between a Mac running Chrome Relay and the *Chrome Relay
Approve* iPhone app. It is the only server-side piece of Chrome Relay, and it does two things:

1. **Wakes the phone.** It holds the app's Apple push key and asks Apple to show a fixed notification.
2. **Carries the messages.** Pairing, requests and answers are sealed end to end between the Mac and the iPhone;
   the relay holds the ciphertext until the other side fetches it.

It can read or approve nothing, and Chrome Relay works without it (see [Without the relay](#without-the-relay)).

## Why it is needed

When an agent needs your approval and you are away from the Mac, the Mac asks your paired iPhone. Two things have
to happen, and neither can happen directly between the two devices.

**The messages need a meeting point.** The Mac sits behind your home or office network and the iPhone on a mobile
one; neither can accept a connection from the other. So each posts sealed messages to a place both can reach, and
fetches the other's from there. That used to be the public ntfy.sh service; it is now this relay (any ntfy server
still works: `chrome-relay phone pair --server`).

**The phone has to be woken.** An iPhone app that isn't open doesn't fetch anything: the only way to put a
notification on a locked iPhone is **Apple Push Notification service (APNs)**.

Apple accepts a push for an app only if it is signed with a key that belongs to **the developer account that
published that app**. There is one such key for the Chrome Relay Approve app on TestFlight, and it can't be given
to everyone who installs Chrome Relay:

- a key in the open-source repo, or inside the `chrome-relay` binary, is public, and Apple revokes keys it finds
  published;
- a key copied to each Mac by hand (`chrome-relay phone push --key …`) works, and stays supported, but only for
  people the developer trusts with it, and it is one more secret to hand around.

So the key sits in one place that can use it and never gives it out: this Worker. A Mac says "wake this phone",
the Worker signs the push, Apple delivers it.

| | Who holds the APNs key | Setup per Mac |
|---|---|---|
| **Push relay** (default) | the Worker | none |
| Your own key (`chrome-relay phone push --key …`) | that Mac's Keychain | copy the key to each Mac |
| No push (ntfy) | nobody | install the ntfy app on the iPhone, subscribe to a topic |

## How an approval travels

```
 Mac (chrome-relay)                        relay                      iPhone (Chrome Relay Approve)
   |-- 1. POST /<topic>-req (sealed) ------->| holds it                        |
   |-- 2. POST /wake {push address} -------->|--> Apple (APNs) --------------->| notification:
   |                                         |                                 | "An agent is waiting"
   |                                         |<-- 3. GET /<topic>-req/json ----| app opens, decrypts,
   |                                         |                                 | shows agent and site
   |<-- 5. stream of /<topic>-res -----------|<-- 4. POST /<topic>-res --------| Face ID signs the answer
   |-- 6. checks the signature against the key it paired with
```

1. The Mac seals the request (which agent, which site, what it wants) to the iPhone's key and posts it to the
   pairing's request topic. Only the paired iPhone can open it.
2. The Mac asks the relay to wake the phone. It sends the phone's **push address** (the APNs device token the app
   gave it, itself sent sealed to the Mac), the app's bundle ID, whether the app is a development build, and the
   request's random id. Nothing else.
3. The relay signs a push with the app's key and gives it to Apple. The notification's text is fixed in the
   Worker: *"Chrome Relay: An agent is waiting for your approval."* You tap it; the app fetches the sealed
   request, decrypts it, and shows who is asking for what.
4. Face ID unlocks a signing key in the iPhone's Secure Enclave; the app signs "approve" or "deny" and posts the
   answer to the pairing's answer topic.
5. The Mac, which has been listening on that topic, receives it.
6. The Mac checks the signature against the public key it stored when you paired. Only then does the agent
   proceed.

**Topics.** A pairing has a random name (`cr` and 26 characters, 128 bits) that only the Mac and the iPhone know:
it is in the QR code you scan. Its topics are `<name>-pair` (the pairing handshake), `-req` (requests), `-res`
(answers) and `-dev` (the phone's push address, sealed to the Mac). Each topic is one small Durable Object.

## What the relay can and cannot do

**It sees:** topic names, sealed messages (ciphertext), a push address, the app's bundle ID, a random request id,
the caller's IP address and the time. Cloudflare's own request logs see the same.

**It does not see:** the agent, the site, the account, the Mac's name, or anything from 1Password. Those are
inside the sealed messages, which only the Mac and the iPhone can open: each is encrypted with a key the two
agreed on when pairing (P-256 ECDH, HKDF-SHA256, AES-GCM), from a secret that travels in the QR code and never
through the relay.

**It cannot approve anything.** An approval is a signature made by a key that exists only in your iPhone's Secure
Enclave, behind Face ID, over that exact request; the Mac checks it. A compromised relay (or a stolen APNs key)
can make your phone show the fixed notification, and can drop or delay messages. Opening the app then shows
nothing to approve unless your own Mac sent a request.

**It cannot be used to put words on a phone.** The notification's title and body are constants in the Worker; a
caller cannot supply them.

**Who can make it notify your phone:** only someone who knows your phone's push address. The app gives that
address only to Macs it has paired with, sealed to their key. As a second line, the Worker rate-limits wake-ups per
phone (20 a minute) so a paired Mac gone wrong can't flood it.

**What it keeps:** the sealed messages of a topic, for up to 12 hours (so the phone's push address is still there
when the Mac next needs it) and at most 50 per topic, 16 KB each; then they are deleted. Nothing else: no accounts,
no log of its own. Apple's provider token is cached in memory for 50 minutes, as Apple requires.

**If it is down** nothing reaches the phone until it is back; approving at the Mac with Touch ID is unaffected.

## The endpoints

```
POST /wake
{"token": "<hex APNs device token>", "topic": "<bundle ID>", "sandbox": false, "request": "<id>"}

200 {"ok": true}
400 token is not a device token, or the body is not JSON
403 the relay does not serve that app (bundle ID not in TOPICS)
422 Apple refused the address (for example BadDeviceToken: the app was deleted)
429 too many wake-ups for this phone
502 Apple push could not be reached, or refused the relay's key
```

`sandbox: true` is for development builds of the app (installed with `./ios/build`), which Apple serves from its
sandbox; TestFlight and App Store builds use production. The app tells the Mac which it is.

```
POST /<topic>                          publish the body (up to 16 KB) as one message      200 | 413 | 429
GET  /<topic>/json?poll=1&since=<s>    the messages since <s>, oldest first, one JSON object per line:
                                       {"id": "…", "time": 1760000000, "event": "message", "topic": "…", "message": "…"}
GET  /<topic>/json?since=<s>           {"event": "open"}, the same messages, then each new one as it is published,
                                       with {"event": "keepalive"} every 25 seconds
```

`<s>` is `all`, a duration (`30s`, `10m`, `12h`) or a Unix time; without it, only what comes next. A topic is 16 to
80 letters, digits, `-` or `_`. This is the part of [ntfy](https://ntfy.sh)'s protocol Chrome Relay uses, so the
Mac and the app talk to the relay and to an ntfy server with the same code.

## How a Mac chooses

`chrome-relay phone` shows what is in use.

**Messages.** `chrome-relay phone pair` uses the relay; `--server https://ntfy.example` uses an ntfy server
instead. The choice is part of the pairing (the QR code carries it), so pairings made earlier keep the server they
were made with: pair again to move one.

**Waking the phone.**

1. **This Mac's own key**, if you imported one (`chrome-relay phone push --key AuthKey_XXXX.p8 --team-id …`).
2. Otherwise **the relay**: `https://chrome-relay-push.aindeev.workers.dev` by default, which serves the Chrome
   Relay Approve app from TestFlight. `CHROME_RELAY_PUSH_RELAY=https://your-relay.example` (in the environment of
   `chrome-relay serve`) names another, and `CHROME_RELAY_PUSH_RELAY=off` turns it off (new pairings then use
   ntfy.sh).
3. If the push fails, or the phone hasn't sent a push address: on an ntfy server, a notification on the pairing's
   `<topic>-n` for the ntfy iPhone app; on the relay, nothing: open the app yourself.

## Without the relay

Pair with `chrome-relay phone pair --server https://ntfy.sh` (or your own ntfy server) and set
`CHROME_RELAY_PUSH_RELAY=off`: nothing of Chrome Relay's then sits between the Mac and the phone. Pairing, the
sealed requests and Face ID approvals work the same. For notifications, import your own APNs key, or install the
ntfy app on the iPhone and subscribe to the topic `chrome-relay phone` prints.

## Running your own

You need your own relay if you build the iPhone app yourself (`./ios/build` or `./ios/release` give it your
team's bundle ID, which the default relay doesn't wake) and want more than one Mac to wake it without copying
your key around, or if you simply want the messages on infrastructure you run. It fits Cloudflare's free plan
(100,000 requests a day: an approval is about ten, and the app fetches every 3 seconds while it is open).

1. In [Apple Developer > Keys](https://developer.apple.com/account/resources/authkeys/list), create a key with
   *Apple Push Notifications service (APNs)*; restrict it to your app's bundle ID. Download `AuthKey_<KEYID>.p8`.
2. In this folder:

   ```bash
   npx wrangler login
   npx wrangler secret put APNS_KEY < ~/Downloads/AuthKey_<KEYID>.p8
   npx wrangler secret put APNS_KEY_ID      # <KEYID>
   npx wrangler secret put APNS_TEAM_ID     # Apple Developer > Membership details
   npx wrangler secret put TOPICS           # your app's bundle ID (several: comma-separated)
   npx wrangler deploy                      # prints the relay's URL
   ```

3. On each Mac, set `CHROME_RELAY_PUSH_RELAY` to that URL for the background service (in
   `~/Library/LaunchAgents/com.chrome-relay.plist`, under `EnvironmentVariables`), then `chrome-relay update`.
   `chrome-relay phone pair` then pairs through it; `chrome-relay phone` shows the relay in use, and
   `chrome-relay phone test` sends a request through it.

The secrets are encrypted by Cloudflare and readable only by the Worker; `wrangler secret put` reads the key from
the file, so it never appears in your terminal or shell history.

## Tests

`node --test test/push-relay.test.mjs` runs the Worker against a generated key and stand-ins for Apple and for
Cloudflare's storage: the provider token verifies as ES256, the notification text is the Worker's own, bad
addresses, other apps and floods are refused, and messages are published, fetched, streamed, capped and expired.
`internal/phone/relay_test.go` covers the Mac's side, and checks a deployed relay with the Mac's own code:

```bash
CHROME_RELAY_LIVE_RELAY=https://your-relay.example go test ./internal/phone -run TestLiveRelay
```
