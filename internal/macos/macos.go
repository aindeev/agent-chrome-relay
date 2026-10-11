// Package macos is Chrome Relay's macOS security: a Keychain item only this signed program reads freely, secrets
// locked to the Secure Enclave, Secure Enclave passkeys, and Touch ID prompts that say what they are for. The work
// is done by secure.swift, compiled into libsecure.a by `go generate` and linked in (macos_darwin.go).
//
// Prompts read "chrome-relay is trying to <reason>", so a reason is a phrase like
// "let Claude Code (my-repo) fill your GitHub password on github.com".
package macos

//go:generate sh -c "swiftc -O -parse-as-library -emit-library -static -module-name secure secure.swift -o libsecure.a"

import "errors"

var (
	ErrCancelled   = errors.New("the person did not approve with Touch ID")
	ErrNotFound    = errors.New("not in the Keychain")
	ErrUnsupported = errors.New("needs a Mac with a Secure Enclave and Touch ID, and a chrome-relay built with Xcode's command line tools (`xcode-select --install`, then ./build)")
)
