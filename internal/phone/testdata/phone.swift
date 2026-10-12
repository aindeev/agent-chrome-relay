// The iPhone app's crypto (ios/Approve/Sources/Crypto.swift) with software keys in place of the Secure Enclave,
// for interop_test.go. Usage: swift phone.swift pair <url> | answer <relayKeyB64url> <requestJSON> <stateFile>
import CryptoKit
import Foundation

func b64url(_ s: String) -> Data {
  var t = s.replacingOccurrences(of: "-", with: "+").replacingOccurrences(of: "_", with: "/")
  while t.count % 4 != 0 { t += "=" }
  return Data(base64Encoded: t)!
}
func key(_ box: P256.KeyAgreement.PrivateKey, _ peer: Data, _ salt: Data, _ info: String) throws -> SymmetricKey {
  try box.sharedSecretFromKeyAgreement(with: P256.KeyAgreement.PublicKey(x963Representation: peer))
    .hkdfDerivedSymmetricKey(using: SHA256.self, salt: salt, sharedInfo: Data(info.utf8), outputByteCount: 32)
}
func json(_ o: Any) -> String { String(decoding: try! JSONSerialization.data(withJSONObject: o), as: UTF8.self) }

let a = CommandLine.arguments
if a[1] == "pair" {
  let q = Dictionary(uniqueKeysWithValues: URLComponents(string: a[2])!.queryItems!.map { ($0.name, $0.value!) })
  let relay = b64url(q["k"]!), secret = b64url(q["p"]!)
  let box = P256.KeyAgreement.PrivateKey(), sign = P256.Signing.PrivateKey()
  let proof = try sign.signature(for: Data("chrome-relay pair v1\n".utf8) + secret).derRepresentation
  let body = json(["sign": sign.publicKey.x963Representation.base64EncodedString(), "device": "Swift", "proof": proof.base64EncodedString()])
  let sealed = try AES.GCM.seal(Data(body.utf8), using: key(box, relay, secret, "chrome-relay pair v1")).combined!
  try json(["box": box.rawRepresentation.base64EncodedString(), "sign": sign.rawRepresentation.base64EncodedString()]).write(toFile: a[3], atomically: true, encoding: .utf8)
  print(json(["v": 1, "box": box.publicKey.x963Representation.base64EncodedString(), "data": sealed.base64EncodedString()]))
} else {
  let st = try JSONSerialization.jsonObject(with: Data(contentsOf: URL(fileURLWithPath: a[4]))) as! [String: String]
  let box = try P256.KeyAgreement.PrivateKey(rawRepresentation: Data(base64Encoded: st["box"]!)!)
  let sign = try P256.Signing.PrivateKey(rawRepresentation: Data(base64Encoded: st["sign"]!)!)
  let m = try JSONSerialization.jsonObject(with: Data(a[3].utf8)) as! [String: Any]
  let id = m["id"] as! String
  let plain = try AES.GCM.open(AES.GCM.SealedBox(combined: Data(base64Encoded: m["data"] as! String)!), using: key(box, b64url(a[2]), Data(id.utf8), "chrome-relay request v1"))
  let hash = SHA256.hash(data: plain).map { String(format: "%02x", $0) }.joined()
  let sig = try sign.signature(for: Data("chrome-relay approve v1\n\(id)\n\(hash)".utf8)).derRepresentation
  print(json(["v": 1, "id": id, "ok": true, "sig": sig.base64EncodedString()]))
}
