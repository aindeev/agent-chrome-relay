import CryptoKit
import Foundation
import LocalAuthentication
import Security

// The same scheme as the Mac (internal/phone/phone.go): P-256 ECDH, HKDF-SHA256, AES-GCM (nonce|ciphertext|tag),
// and ECDSA P-256 signatures in DER. Both private keys live in this iPhone's Secure Enclave and never leave it.

enum Tags {
  static let pair = "chrome-relay pair v1"
  static let request = "chrome-relay request v1"
  static let approve = "chrome-relay approve v1"
  static let device = "chrome-relay device v1"
}

enum ApproveError: LocalizedError {
  case noSecureEnclave, badMessage
  var errorDescription: String? {
    switch self {
    case .noSecureEnclave: return "This iPhone has no Secure Enclave."
    case .badMessage: return "A message this iPhone can't read."
    }
  }
}

/// This iPhone's two keys for one Mac: one to read its requests, one (behind Face ID) to sign approvals.
struct Keys {
  let box: SecureEnclave.P256.KeyAgreement.PrivateKey
  let signBlob: Data

  static func make() throws -> Keys {
    guard SecureEnclave.isAvailable else { throw ApproveError.noSecureEnclave }
    var err: Unmanaged<CFError>?
    guard let read = SecAccessControlCreateWithFlags(nil, kSecAttrAccessibleWhenUnlockedThisDeviceOnly, .privateKeyUsage, &err),
          let faceID = SecAccessControlCreateWithFlags(nil, kSecAttrAccessibleWhenUnlockedThisDeviceOnly, [.privateKeyUsage, .biometryCurrentSet], &err)
    else { throw err!.takeRetainedValue() as Error }
    let box = try SecureEnclave.P256.KeyAgreement.PrivateKey(accessControl: read)
    let sign = try SecureEnclave.P256.Signing.PrivateKey(accessControl: faceID)
    return Keys(box: box, signBlob: sign.dataRepresentation)
  }

  init(box: SecureEnclave.P256.KeyAgreement.PrivateKey, signBlob: Data) { self.box = box; self.signBlob = signBlob }

  init(boxBlob: Data, signBlob: Data) throws {
    box = try SecureEnclave.P256.KeyAgreement.PrivateKey(dataRepresentation: boxBlob)
    self.signBlob = signBlob
  }

  var signPublic: Data { get throws { try SecureEnclave.P256.Signing.PrivateKey(dataRepresentation: signBlob).publicKey.x963Representation } }

  /// Face ID, with `reason` in the prompt, then a signature over msg.
  func sign(_ msg: Data, reason: String) throws -> Data {
    let ctx = LAContext()
    ctx.localizedReason = reason
    let key = try SecureEnclave.P256.Signing.PrivateKey(dataRepresentation: signBlob, authenticationContext: ctx)
    return try key.signature(for: msg).derRepresentation
  }

  private func symmetric(_ peer: Data, salt: Data, info: String) throws -> SymmetricKey {
    let secret = try box.sharedSecretFromKeyAgreement(with: P256.KeyAgreement.PublicKey(x963Representation: peer))
    return secret.hkdfDerivedSymmetricKey(using: SHA256.self, salt: salt, sharedInfo: Data(info.utf8), outputByteCount: 32)
  }

  func seal(_ plain: Data, to peer: Data, salt: Data, info: String) throws -> Data {
    try AES.GCM.seal(plain, using: symmetric(peer, salt: salt, info: info)).combined!
  }

  func open(_ box: Data, from peer: Data, salt: Data, info: String) throws -> Data {
    try AES.GCM.open(AES.GCM.SealedBox(combined: box), using: symmetric(peer, salt: salt, info: info))
  }
}

extension Data {
  init?(base64URL s: String) {
    var t = s.replacingOccurrences(of: "-", with: "+").replacingOccurrences(of: "_", with: "/")
    while t.count % 4 != 0 { t += "=" }
    self.init(base64Encoded: t)
  }
  var hex: String { map { String(format: "%02x", $0) }.joined() }
}

/// The signed text, exactly as the Mac checks it: the request id and a hash of the request as shown.
func approval(id: String, plain: Data) -> Data {
  Data("\(Tags.approve)\n\(id)\n\(Data(SHA256.hash(data: plain)).hex)".utf8)
}

// Secure Enclave key blobs are useless on any other device; the Keychain keeps them, this device only.
enum Vault {
  static func save(_ data: Data, as name: String) throws {
    let q: [String: Any] = [kSecClass as String: kSecClassGenericPassword, kSecAttrService as String: "Chrome Relay Approve", kSecAttrAccount as String: name]
    SecItemDelete(q as CFDictionary)
    var add = q
    add[kSecValueData as String] = data
    add[kSecAttrAccessible as String] = kSecAttrAccessibleWhenUnlockedThisDeviceOnly
    let st = SecItemAdd(add as CFDictionary, nil)
    guard st == errSecSuccess else { throw NSError(domain: NSOSStatusErrorDomain, code: Int(st)) }
  }
  static func load(_ name: String) -> Data? {
    let q: [String: Any] = [kSecClass as String: kSecClassGenericPassword, kSecAttrService as String: "Chrome Relay Approve",
                            kSecAttrAccount as String: name, kSecReturnData as String: true]
    var out: CFTypeRef?
    return SecItemCopyMatching(q as CFDictionary, &out) == errSecSuccess ? out as? Data : nil
  }
  static func delete(_ name: String) {
    let q: [String: Any] = [kSecClass as String: kSecClassGenericPassword, kSecAttrService as String: "Chrome Relay Approve", kSecAttrAccount as String: name]
    SecItemDelete(q as CFDictionary)
  }
}
