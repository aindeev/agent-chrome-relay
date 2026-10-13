# The push relay

A Cloudflare Worker (`worker.mjs`, about 80 lines) that wakes the *Chrome Relay Approve* iPhone app when an agent
needs your approval. It is the only server-side piece of Chrome Relay, it stores nothing, and Chrome Relay works
without it (see [Without the relay](#without-the-relay)).

## Why it is needed

When an agent needs your approval and you are away from the Mac, the Mac asks your paired iPhone. The request
itself reaches the phone through an encrypted channel the app reads (ntfy). But an iPhone app that isn't open
doesn't read anything: the only way to put a notification on a locked iPhone is **Apple Push Notification service
(APNs)**.

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
 Mac (chrome-relay)                     ntfy server                  iPhone (Chrome Relay Approve)
   |-- 1. encrypted request ------------->|                                   |
   |-- 2. POST /wake {push address} --> push relay --> Apple (APNs) --------->| notification:
   |                                      |                                   | "An agent is waiting"
   |                                      |<-- 3. app opens, reads request ---|
   |                                      |    (decrypts it, shows agent/site)|
   |<------------- 5. signed answer ------|<-- 4. Face ID signs the answer ---|
   |-- 6. checks the signature against the key it paired with
```

1. The Mac seals the request (which agent, which site, what it wants) to the iPhone's key and publishes it to the
   pairing's ntfy topic. Only the paired iPhone can open it.
2. The Mac asks the relay to wake the phone. It sends the phone's **push address** (the APNs device token the app
   gave it when pairing, itself sent sealed to the Mac), the app's bundle ID, whether the app is a development
   build, and the request's random id. Nothing else.
3. The relay signs a push with the app's key and gives it to Apple. The notification's text is fixed in the
   Worker: *"Chrome Relay: An agent is waiting for your approval."*
4. You tap the notification; the app fetches the sealed request, decrypts it, and shows who is asking for what.
5. Face ID unlocks a signing key in the iPhone's Secure Enclave; the app signs "approve" or "deny" and publishes
   the answer.
6. The Mac checks the signature against the public key it stored when you paired. Only then does the agent
   proceed.

## What the relay can and cannot do

**It sees:** a push address, the bundle ID, a random request id, the caller's IP address and the time. Cloudflare's
own request logs see the same.

**It does not see:** the agent, the site, the account, the Mac's name, or anything from 1Password. Those are in
the sealed request, which never passes through the relay.

**It cannot approve anything.** An approval is a signature made by a key that exists only in your iPhone's Secure
Enclave, behind Face ID. A compromised relay (or a stolen APNs key) can make your phone show the fixed
notification; opening it shows nothing to approve unless your own Mac sent a request.

**It cannot be used to put words on a phone.** The notification's title and body are constants in the Worker; a
caller cannot supply them.

**Who can make it notify your phone:** only someone who knows your phone's push address. The app gives that
address only to Macs it has paired with, sealed to their key. As a second line, the Worker rate-limits wake-ups per
phone (20 a minute) so a paired Mac gone wrong can't flood it.

**It keeps nothing.** No database, no key-value store, no log of its own. The only state is Apple's provider
token, cached in memory for 50 minutes as Apple requires.

**If it is down** the push isn't sent, and the Mac falls back to an ntfy notification (visible if you use the
ntfy app); the request still waits in the app, and approving at the Mac with Touch ID is unaffected.

## The endpoint

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

## How a Mac chooses

`chrome-relay phone` shows which is in use.

1. **This Mac's own key**, if you imported one (`chrome-relay phone push --key AuthKey_XXXX.p8 --team-id …`).
2. Otherwise **the push relay**: `https://chrome-relay-push.aindeev.workers.dev` by default, which serves the
   Chrome Relay Approve app from TestFlight. `CHROME_RELAY_PUSH_RELAY=https://your-relay.example` (in the
   environment of `chrome-relay serve`) names another, and `CHROME_RELAY_PUSH_RELAY=off` turns it off.
3. If the push fails, or the phone hasn't sent a push address, **ntfy**: a notification on the pairing's
   `<topic>-n`, for the ntfy iPhone app.

## Without the relay

Everything except the wake-up works with no server of Chrome Relay's at all: pairing, the encrypted requests, Face
ID approvals. With `CHROME_RELAY_PUSH_RELAY=off` and no key of your own, you either open the app yourself when an
agent says it is waiting, or get the notification through the ntfy app.

## Running your own

You need your own relay if you build the iPhone app yourself (`./ios/build` or `./ios/release` give it your
team's bundle ID, which the default relay doesn't serve) and want more than one Mac to wake it without copying
your key around. It runs on Cloudflare's free plan (100,000 requests a day; a wake-up is one request).

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
   `chrome-relay phone` shows the relay in use, and `chrome-relay phone test` sends a request through it.

The secrets are encrypted by Cloudflare and readable only by the Worker; `wrangler secret put` reads the key from
the file, so it never appears in your terminal or shell history.

## Tests

`node --test test/push-relay.test.mjs` runs the Worker against a generated key and a stand-in for Apple: the
provider token verifies as ES256, the notification text is the Worker's own, and bad addresses, other apps and
floods are refused. `internal/phone/relay_test.go` covers the Mac's side.
