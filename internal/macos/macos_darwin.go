//go:build darwin && cgo

package macos

/*
#cgo LDFLAGS: -L${SRCDIR} -lsecure -framework Security -framework LocalAuthentication -framework CryptoKit -framework Foundation
#include <stdlib.h>
#include <stdbool.h>
char *cr_keychain_set(const char *, const char *, const char *);
char *cr_keychain_get(const char *, const char *);
char *cr_keychain_delete(const char *, const char *);
char *cr_keychain_accounts(const char *);
char *cr_authenticate(const char *);
char *cr_authenticate_as(const char *, const char *);
void cr_authenticate_stop(const char *);
char *cr_lock(const char *, bool);
char *cr_unlock(const char *, const char *);
char *cr_passkey_create(bool);
char *cr_passkey_sign(const char *, const char *, const char *, const char *, int);
*/
import "C"

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unsafe"
)

// Linked: this build has the macOS security layer (secure.swift).
const Linked = true

type answer struct {
	OK        json.RawMessage `json:"ok"`
	Error     string          `json:"error"`
	Cancelled bool            `json:"cancelled"`
	Declined  bool            `json:"declined"`
	NotFound  bool            `json:"notFound"`
}

// call runs one Swift function: C strings in (freed after), its malloc'd JSON answer out.
func call(f func(args ...*C.char) *C.char, args ...string) (json.RawMessage, error) {
	cs := make([]*C.char, len(args))
	for i, a := range args {
		cs[i] = C.CString(a)
		defer C.free(unsafe.Pointer(cs[i]))
	}
	out := f(cs...)
	defer C.free(unsafe.Pointer(out))
	var a answer
	if err := json.Unmarshal([]byte(C.GoString(out)), &a); err != nil {
		return nil, err
	}
	switch {
	case a.Declined:
		return nil, ErrDeclined
	case a.Cancelled:
		return nil, ErrCancelled
	case a.NotFound:
		return nil, ErrNotFound
	case a.Error != "":
		return nil, errors.New(a.Error)
	}
	return a.OK, nil
}

func str(raw json.RawMessage) (string, error) { var s string; return s, json.Unmarshal(raw, &s) }
func bytesOf(raw json.RawMessage) ([]byte, error) {
	s, err := str(raw)
	if err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(s)
}
func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// KeychainSet stores value in the login keychain; the item trusts only this program.
func KeychainSet(service, account string, value []byte) error {
	_, err := call(func(a ...*C.char) *C.char { return C.cr_keychain_set(a[0], a[1], a[2]) }, service, account, b64(value))
	return err
}

// KeychainGet reads a value this program stored. ErrNotFound when there is none.
func KeychainGet(service, account string) ([]byte, error) {
	raw, err := call(func(a ...*C.char) *C.char { return C.cr_keychain_get(a[0], a[1]) }, service, account)
	if err != nil {
		return nil, err
	}
	return bytesOf(raw)
}

// KeychainDelete removes the item, if any.
func KeychainDelete(service, account string) error {
	_, err := call(func(a ...*C.char) *C.char { return C.cr_keychain_delete(a[0], a[1]) }, service, account)
	return err
}

// KeychainAccounts lists the accounts of service's items, reading no value (no prompt).
func KeychainAccounts(service string) ([]string, error) {
	raw, err := call(func(a ...*C.char) *C.char { return C.cr_keychain_accounts(a[0]) }, service)
	if err != nil {
		return nil, err
	}
	var accounts []string
	return accounts, json.Unmarshal(raw, &accounts)
}

// Authenticate asks the person for Touch ID (or their password). ErrCancelled if they decline.
func Authenticate(reason string) error {
	_, err := call(func(a ...*C.char) *C.char { return C.cr_authenticate(a[0]) }, reason)
	return err
}

// AuthenticateContext is Authenticate that goes away when ctx ends (the person approved somewhere else).
// ErrDeclined when they pressed Cancel; ErrCancelled when it timed out or was stopped.
func AuthenticateContext(ctx context.Context, reason string) error {
	tag := fmt.Sprintf("%p", &reason)
	done := make(chan error, 1)
	go func() {
		_, err := call(func(a ...*C.char) *C.char { return C.cr_authenticate_as(a[0], a[1]) }, reason, tag)
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		t := C.CString(tag)
		C.cr_authenticate_stop(t)
		C.free(unsafe.Pointer(t))
		<-done
		return ctx.Err()
	}
}

// Lock encrypts secret to a new Secure Enclave key; only Unlock on this Mac opens it again: with Touch ID, or,
// silent, without a prompt (then it is as safe as the Keychain item that holds it).
func Lock(secret []byte, silent bool) ([]byte, error) {
	raw, err := call(func(a ...*C.char) *C.char { return C.cr_lock(a[0], C.bool(silent)) }, b64(secret))
	if err != nil {
		return nil, err
	}
	s, err := str(raw)
	return []byte(s), err
}

// Unlock opens what Lock made, after the person approves Touch ID for reason.
func Unlock(locked []byte, reason string) ([]byte, error) {
	raw, err := call(func(a ...*C.char) *C.char { return C.cr_unlock(a[0], a[1]) }, string(locked), reason)
	if err != nil {
		return nil, err
	}
	return bytesOf(raw)
}

// PasskeyCreate makes a P-256 signing key in the Secure Enclave: its handle (usable only by this Mac's chip)
// and its public key (X9.63). Creating asks nothing of the person. Signing: with Touch ID each time, or, silent,
// without a prompt (Chrome Relay asks first, and keeps the handle in its own Keychain item).
func PasskeyCreate(silent bool) (handle, publicKey []byte, err error) {
	raw, err := call(func(...*C.char) *C.char { return C.cr_passkey_create(C.bool(silent)) })
	if err != nil {
		return nil, nil, err
	}
	var k struct{ Handle, PublicKey string }
	if err := json.Unmarshal(raw, &k); err != nil {
		return nil, nil, err
	}
	if handle, err = base64.StdEncoding.DecodeString(k.Handle); err == nil {
		publicKey, err = base64.StdEncoding.DecodeString(k.PublicKey)
	}
	return handle, publicKey, err
}

// PasskeySign signs data (ES256: ECDSA over SHA-256(data), DER) with a chip key, after Touch ID for reason.
// With reuse > 0 (at most 5 minutes), one approval covers scope's signatures for that long.
func PasskeySign(handle, data []byte, reason, scope string, reuse time.Duration) ([]byte, error) {
	secs := C.int(min(reuse, 5*time.Minute) / time.Second)
	raw, err := call(func(a ...*C.char) *C.char { return C.cr_passkey_sign(a[0], a[1], a[2], a[3], secs) }, b64(handle), b64(data), reason, scope)
	if err != nil {
		return nil, err
	}
	return bytesOf(raw)
}
