// Chrome Relay's macOS security, linked into the chrome-relay binary (see macos_darwin.go). Everything that must
// stay out of other programs' reach lives here: the Keychain item only this signed program may read, Secure
// Enclave keys (their private halves never leave the chip), and Touch ID prompts that say what they are for.
// Each function returns a malloc'd JSON string: {"ok": ...} or {"error": "...", "cancelled": bool, "notFound": bool}.
import CryptoKit
import Foundation
import LocalAuthentication
import Security

private func json(_ o: [String: Any]) -> UnsafeMutablePointer<CChar> {
  strdup(String(decoding: (try? JSONSerialization.data(withJSONObject: o)) ?? Data("{\"error\":\"encoding\"}".utf8), as: UTF8.self))
}
private func ok(_ v: Any = true) -> UnsafeMutablePointer<CChar> { json(["ok": v]) }
private func fail(_ e: Error) -> UnsafeMutablePointer<CChar> {
  let ns = e as NSError, text = "\(e)"
  let cancelled = (e as? LAError).map { [.userCancel, .systemCancel, .appCancel, .authenticationFailed, .userFallback].contains($0.code) } ?? false
    || ns.code == -128 || ns.code == errSecUserCanceled || text.lowercased().contains("cancel")
  return json(["error": cancelled ? "the person did not approve with Touch ID" : text, "cancelled": cancelled])
}
private func s(_ p: UnsafePointer<CChar>) -> String { String(cString: p) }
private func b64(_ p: UnsafePointer<CChar>) -> Data { Data(base64Encoded: s(p)) ?? Data() }
private func touchIDOnly() -> SecAccessControl {
  SecAccessControlCreateWithFlags(nil, kSecAttrAccessibleWhenUnlockedThisDeviceOnly, [.privateKeyUsage, .userPresence], nil)!
}
private func context(_ reason: UnsafePointer<CChar>) -> LAContext { let c = LAContext(); c.localizedReason = s(reason); return c }

// ---- Keychain: a generic password in the login keychain, created by this program, so only it reads it freely
private func query(_ service: UnsafePointer<CChar>, _ account: UnsafePointer<CChar>) -> [String: Any] {
  [kSecClass as String: kSecClassGenericPassword, kSecAttrService as String: s(service), kSecAttrAccount as String: s(account)]
}
@_cdecl("cr_keychain_set") public func cr_keychain_set(_ service: UnsafePointer<CChar>, _ account: UnsafePointer<CChar>, _ value: UnsafePointer<CChar>) -> UnsafeMutablePointer<CChar> {
  SecItemDelete(query(service, account) as CFDictionary)
  var add = query(service, account); add[kSecValueData as String] = b64(value)
  let st = SecItemAdd(add as CFDictionary, nil)
  return st == errSecSuccess ? ok() : json(["error": "Keychain error \(st)"])
}
@_cdecl("cr_keychain_get") public func cr_keychain_get(_ service: UnsafePointer<CChar>, _ account: UnsafePointer<CChar>) -> UnsafeMutablePointer<CChar> {
  var q = query(service, account); q[kSecReturnData as String] = true
  var out: CFTypeRef?; let st = SecItemCopyMatching(q as CFDictionary, &out)
  if st == errSecItemNotFound { return json(["error": "not in the Keychain", "notFound": true]) }
  return st == errSecSuccess ? ok((out as! Data).base64EncodedString()) : json(["error": "Keychain error \(st)"])
}
@_cdecl("cr_keychain_delete") public func cr_keychain_delete(_ service: UnsafePointer<CChar>, _ account: UnsafePointer<CChar>) -> UnsafeMutablePointer<CChar> {
  let st = SecItemDelete(query(service, account) as CFDictionary)
  return st == errSecSuccess || st == errSecItemNotFound ? ok() : json(["error": "Keychain error \(st)"])
}

// ---- Touch ID (or the Mac's password) with a reason: "chrome-relay is trying to <reason>"
@_cdecl("cr_authenticate") public func cr_authenticate(_ reason: UnsafePointer<CChar>) -> UnsafeMutablePointer<CChar> {
  let c = LAContext(); var err: NSError?
  guard c.canEvaluatePolicy(.deviceOwnerAuthentication, error: &err) else { return fail(err!) }
  let done = DispatchSemaphore(value: 0); var result: Error?
  c.evaluatePolicy(.deviceOwnerAuthentication, localizedReason: s(reason)) { success, error in if !success { result = error }; done.signal() }
  done.wait()
  return result.map(fail) ?? ok()
}

// ---- Lock a secret to this Mac's chip: ECDH with a Secure Enclave key that needs Touch ID, then AES-GCM
@_cdecl("cr_lock") public func cr_lock(_ secret: UnsafePointer<CChar>) -> UnsafeMutablePointer<CChar> {
  do {
    let chip = try SecureEnclave.P256.KeyAgreement.PrivateKey(accessControl: touchIDOnly())
    let eph = P256.KeyAgreement.PrivateKey()
    let key = try eph.sharedSecretFromKeyAgreement(with: chip.publicKey).hkdfDerivedSymmetricKey(using: SHA256.self, salt: Data(), sharedInfo: Data("chrome-relay".utf8), outputByteCount: 32)
    let box = try AES.GCM.seal(b64(secret), using: key).combined!
    let o = ["handle": chip.dataRepresentation.base64EncodedString(), "eph": eph.publicKey.x963Representation.base64EncodedString(), "box": box.base64EncodedString()]
    return ok(String(decoding: try JSONSerialization.data(withJSONObject: o), as: UTF8.self))
  } catch { return fail(error) }
}
@_cdecl("cr_unlock") public func cr_unlock(_ locked: UnsafePointer<CChar>, _ reason: UnsafePointer<CChar>) -> UnsafeMutablePointer<CChar> {
  do {
    guard let o = try JSONSerialization.jsonObject(with: Data(s(locked).utf8)) as? [String: String],
          let h = o["handle"].flatMap({ Data(base64Encoded: $0) }), let e = o["eph"].flatMap({ Data(base64Encoded: $0) }), let b = o["box"].flatMap({ Data(base64Encoded: $0) })
    else { return json(["error": "the locked secret is damaged"]) }
    let chip = try SecureEnclave.P256.KeyAgreement.PrivateKey(dataRepresentation: h, authenticationContext: context(reason))
    let key = try chip.sharedSecretFromKeyAgreement(with: P256.KeyAgreement.PublicKey(x963Representation: e)).hkdfDerivedSymmetricKey(using: SHA256.self, salt: Data(), sharedInfo: Data("chrome-relay".utf8), outputByteCount: 32)
    return ok(try AES.GCM.open(AES.GCM.SealedBox(combined: b), using: key).base64EncodedString())
  } catch { return fail(error) }
}

// ---- Passkeys: a signing key made by the chip; every signature needs Touch ID
@_cdecl("cr_passkey_create") public func cr_passkey_create() -> UnsafeMutablePointer<CChar> {
  do {
    let k = try SecureEnclave.P256.Signing.PrivateKey(accessControl: touchIDOnly())
    return ok(["handle": k.dataRepresentation.base64EncodedString(), "publicKey": k.publicKey.x963Representation.base64EncodedString()])
  } catch { return fail(error) }
}
// One Touch ID may cover a scope's signatures (one agent's) for a few seconds: the approved LocalAuthentication
// context is reused, so the chip still signs only with a recent approval. 0 seconds: every signature asks.
private let approvals = NSLock()
private var approved: [String: (context: LAContext, until: Date)] = [:]

@_cdecl("cr_passkey_sign") public func cr_passkey_sign(_ handle: UnsafePointer<CChar>, _ data: UnsafePointer<CChar>, _ reason: UnsafePointer<CChar>,
                                                      _ scope: UnsafePointer<CChar>, _ reuseSeconds: Int32) -> UnsafeMutablePointer<CChar> {
  approvals.lock()
  defer { approvals.unlock() } // one prompt at a time: a second request waits for the first approval
  do {
    var ctx = context(reason)
    if reuseSeconds > 0 {
      let key = s(scope)
      if let a = approved[key], a.until > Date() {
        ctx = a.context
      } else {
        approved[key]?.context.invalidate()
        approved[key] = nil
        let done = DispatchSemaphore(value: 0)
        var err: Error?
        ctx.evaluatePolicy(.deviceOwnerAuthentication, localizedReason: s(reason)) { ok, e in
          if !ok { err = e ?? LAError(.authenticationFailed) }
          done.signal()
        }
        done.wait()
        if let err { return fail(err) }
        approved[key] = (ctx, Date().addingTimeInterval(TimeInterval(min(reuseSeconds, 300))))
      }
    }
    let k = try SecureEnclave.P256.Signing.PrivateKey(dataRepresentation: b64(handle), authenticationContext: ctx)
    return ok(try k.signature(for: b64(data)).derRepresentation.base64EncodedString())
  } catch { return fail(error) }
}
