package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"rsc.io/qr"

	"github.com/aindeev/agent-chrome-relay/internal/macos"
	"github.com/aindeev/agent-chrome-relay/internal/phone"
)

// phone pairs an iPhone that approves agents' requests with Face ID when you're away from the Mac:
//
//	chrome-relay phone                  which phone is paired
//	chrome-relay phone pair [--server]  pair one (Touch ID, then scan a QR code with the iPhone's Camera)
//	chrome-relay phone test             send it a test request
//	chrome-relay phone forget           unpair it
//	chrome-relay phone push --key <AuthKey_KEYID.p8> --team-id <TEAMID> [--key-id <KEYID>]
//	                                    notify the phone through Apple push, with your own APNs key
//	chrome-relay phone push forget      back to ntfy
func (c *cli) phone() error {
	if !macos.Linked {
		return errors.New("this chrome-relay was built without Xcode's command line tools, so it has no iPhone approvals: " +
			"`xcode-select --install`, then ./build again")
	}
	inst := c.vaultInstance()
	switch c.arg(2) {
	case "":
		p, err := phone.Load(inst)
		if err != nil {
			return err
		}
		if p == nil {
			c.printf("No iPhone paired. `%s phone pair` lets one approve agents' requests with Face ID when you're away.\n", c.Name)
		} else {
			c.printf("Paired: %s, since %s, through %s.\nApprovals go to Touch ID here and to the iPhone at once; the first answer wins.\n",
				p.Device, p.Paired.Local().Format("2006-01-02 15:04"), p.Server)
		}
		a := phone.LoadAPNs(inst)
		if a != nil {
			c.printf("Apple push: key %s, team %s.\n", a.KeyID, a.TeamID)
		} else {
			c.println("Apple push: not set up; notifications go through ntfy.")
		}
		if p != nil && (a == nil || p.Push == nil) {
			if a != nil {
				c.println("The paired iPhone hasn't sent its push address yet; until it does, notifications go through ntfy.")
			}
			c.printf("Notifications: in the ntfy iPhone app, subscribe to %s on %s.\n", p.Topic+"-n", p.Server)
		}
		return nil
	case "push":
		if c.arg(3) == "forget" {
			if err := phone.ForgetAPNs(inst); err != nil {
				return err
			}
			c.println("Removed the APNs key: notifications go through ntfy.")
			return nil
		}
		a, path, err := parsePush(c.args[3:], os.ReadFile)
		if err != nil {
			return fmt.Errorf("%w\nusage: %s phone push --key <AuthKey_KEYID.p8> --team-id <TEAMID> [--key-id <KEYID>] | push forget", err, c.Name)
		}
		if err := phone.SaveAPNs(inst, a); err != nil {
			return err
		}
		c.printf("Saved the APNs key in Chrome Relay's Keychain item: key %s, team %s. Approvals now notify the phone through Apple push.\n", a.KeyID, a.TeamID)
		c.printf("The .p8 file is still at %s: keep a copy in 1Password (it can't be downloaded again), then delete it from Downloads.\n", path)
		return nil
	case "pair":
		server := "https://ntfy.sh"
		for i := 3; i < len(c.args); i++ {
			if v, ok := strings.CutPrefix(c.args[i], "--server="); ok {
				server = v
			} else if c.args[i] == "--server" && i+1 < len(c.args) {
				server, i = c.args[i+1], i+1
			}
		}
		if err := macos.Authenticate("pair an iPhone that can approve your agents' Chrome Relay requests (decline unless you just ran `" + c.Name + " phone pair`)"); err != nil {
			return err
		}
		o, err := phone.NewOffer(server, phone.MacName())
		if err != nil {
			return err
		}
		code, err := qr.Encode(o.URL(), qr.M)
		if err != nil {
			return err
		}
		c.println("Scan this with your iPhone's Camera and open it in Chrome Relay Approve (the code works once, for 5 minutes):")
		c.println(renderQR(code))
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		p, err := phone.Pair(ctx, phone.Ntfy{Server: server}, o)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return errors.New("no iPhone answered within 5 minutes: run it again")
			}
			return err
		}
		if err := phone.Save(inst, p); err != nil {
			return err
		}
		c.printf("Paired with %s. Agents' approvals now go to Touch ID here and Face ID there; the first answer wins.\n", p.Device)
		c.printf("For notifications, subscribe to %s in the ntfy iPhone app (server %s). Try it: `%s phone test`\n", p.Topic+"-n", server, c.Name)
		return nil
	case "test":
		p, err := phone.Load(inst)
		if err != nil {
			return err
		}
		if p == nil {
			return fmt.Errorf("no iPhone paired: `%s phone pair`", c.Name)
		}
		c.printf("Sent a test request to %s; approve or deny it there (2 minutes)…\n", p.Device)
		err = phone.Ask(context.Background(), inst, p, phone.Details{Reason: "test Chrome Relay's iPhone approvals (nothing happens either way)",
			Agent: c.Name + " phone test", Site: phone.MacName(), What: "a test (nothing happens either way)"}, 2*time.Minute)
		switch {
		case err == nil:
			c.println("Approved, with a valid Face ID signature from the paired iPhone.")
		case errors.Is(err, phone.ErrDenied):
			c.println("Denied on the iPhone.")
		default:
			return err
		}
		return nil
	case "forget":
		if err := phone.Forget(inst); err != nil {
			return err
		}
		c.println("Unpaired: approvals are Touch ID on this Mac only. Delete the Mac in the iPhone app too.")
		return nil
	}
	return fmt.Errorf("usage: %s phone [pair [--server <ntfy server>] | test | forget | push --key <AuthKey_KEYID.p8> --team-id <TEAMID> | push forget]", c.Name)
}

// parsePush reads `phone push` arguments (--key, --key-id, --team-id, as `--flag v` or `--flag=v`) and the key
// file, and checks the result signs an APNs token. The key ID defaults to the one in the file's name
// (AuthKey_<KEYID>.p8). Returns the key file's path for the reminder to store and delete it.
func parsePush(args []string, read func(string) ([]byte, error)) (*phone.APNs, string, error) {
	vals := map[string]string{}
	for i := 0; i < len(args); i++ {
		name, v, hasValue := strings.Cut(args[i], "=")
		switch name {
		case "--key", "--key-id", "--team-id":
		default:
			return nil, "", fmt.Errorf("unknown argument %q", args[i])
		}
		if !hasValue {
			if i+1 >= len(args) {
				return nil, "", fmt.Errorf("%s needs a value", name)
			}
			v, i = args[i+1], i+1
		}
		vals[name] = strings.TrimSpace(v)
	}
	path := vals["--key"]
	if path == "" {
		return nil, "", errors.New("--key <path to AuthKey_KEYID.p8> is required")
	}
	if vals["--team-id"] == "" {
		return nil, "", errors.New("--team-id is required (Apple Developer > Membership details)")
	}
	keyID := vals["--key-id"]
	if keyID == "" {
		id, ok := strings.CutPrefix(strings.TrimSuffix(filepath.Base(path), ".p8"), "AuthKey_")
		if !ok {
			return nil, "", errors.New("--key-id is required when the file isn't named AuthKey_<KEYID>.p8")
		}
		keyID = id
	}
	pem, err := read(path)
	if err != nil {
		return nil, "", err
	}
	a := &phone.APNs{KeyID: strings.ToUpper(keyID), TeamID: strings.ToUpper(vals["--team-id"]), PEM: string(pem)}
	if err := a.Validate(); err != nil {
		return nil, "", err
	}
	return a, path, nil
}

// renderQR draws the code with half blocks, two rows per line, dark on light with a quiet zone.
func renderQR(code *qr.Code) string {
	var b strings.Builder
	n, q := code.Size, 2
	dark := func(x, y int) bool { return x >= 0 && y >= 0 && x < n && y < n && code.Black(x, y) }
	for y := -q; y < n+q; y += 2 {
		for x := -q; x < n+q; x++ {
			top, bottom := dark(x, y), dark(x, y+1)
			switch {
			case top && bottom:
				b.WriteString(" ")
			case top:
				b.WriteString("▄")
			case bottom:
				b.WriteString("▀")
			default:
				b.WriteString("█")
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}
