//go:build !darwin || !cgo

package macos

import "time"

// Elsewhere (CI on Linux), or built without Xcode's command line tools (the core-only build), there is no Secure
// Enclave, Keychain or Touch ID: everything reports ErrUnsupported.

// Linked: this build has the macOS security layer (secure.swift).
const Linked = false

func KeychainSet(service, account string, value []byte) error { return ErrUnsupported }
func KeychainGet(service, account string) ([]byte, error)     { return nil, ErrUnsupported }
func KeychainDelete(service, account string) error            { return ErrUnsupported }
func Authenticate(reason string) error                        { return ErrUnsupported }
func Lock(secret []byte) ([]byte, error)                      { return nil, ErrUnsupported }
func Unlock(locked []byte, reason string) ([]byte, error)     { return nil, ErrUnsupported }
func PasskeyCreate() (handle, publicKey []byte, err error)    { return nil, nil, ErrUnsupported }
func PasskeySign(handle, data []byte, reason, scope string, reuse time.Duration) ([]byte, error) {
	return nil, ErrUnsupported
}
