import CryptoKit
import Foundation
import UIKit

/// A Mac this iPhone approves for.
struct Mac: Codable, Identifiable, Hashable {
  var id: String { topic }
  let name: String
  let server: String
  let topic: String
  let relayKey: Data // the Mac's key-agreement public key
  let paired: Date

  var keys: Keys? {
    guard let box = Vault.load(topic + ".box"), let sign = Vault.load(topic + ".sign") else { return nil }
    return try? Keys(boxBlob: box, signBlob: sign)
  }
}

/// What the Mac asks (internal/phone Request), plus the exact bytes the approval signs.
struct Pending: Identifiable, Hashable {
  let id: String
  let mac: Mac
  let macName: String
  let reason: String
  let agent: String? // "Claude Code (my-repo)"; older Macs send only the reason
  let site: String?  // "www.twilio.com"
  let what: String?  // "password", "one-time code", "passkey as bot@acme.com"
  let created: Date
  let expires: Date
  let plain: Data
}

/// One line of the local history ("LOG"): what was asked and how it ended. Requests hold no secrets, nor does this.
struct LogEntry: Codable, Identifiable, Hashable {
  enum Result: String, Codable { case approved, denied, paired }
  var id = UUID()
  let time: Date
  let mac: String
  var agent: String? = nil
  var site: String? = nil
  var what: String? = nil
  let result: Result
}

/// How answering a request ended, for the screen to type out.
enum Outcome: Equatable {
  case sent       // approved (signed) or denied, and the Mac has it
  case notSigned  // Face ID cancelled or failed: the request stays
  case failed     // not sent; Store.status says why
}

/// ntfy: publish, and read what a topic holds (internal/phone/ntfy.go on the Mac).
enum Ntfy {
  static func publish(_ server: String, _ topic: String, _ body: Data) async throws {
    var req = URLRequest(url: URL(string: "\(server)/\(topic)")!)
    req.httpMethod = "POST"
    req.httpBody = body
    let (_, res) = try await URLSession.shared.data(for: req)
    guard (res as? HTTPURLResponse)?.statusCode == 200 else { throw URLError(.badServerResponse) }
  }

  static func poll(_ server: String, _ topic: String, since: String = "10m") async throws -> [Data] {
    let (data, _) = try await URLSession.shared.data(from: URL(string: "\(server)/\(topic)/json?poll=1&since=\(since)")!)
    return String(decoding: data, as: UTF8.self).split(separator: "\n").compactMap { line in
      guard let o = try? JSONSerialization.jsonObject(with: Data(line.utf8)) as? [String: Any],
            o["event"] as? String == "message", let m = o["message"] as? String else { return nil }
      return Data(m.utf8)
    }
  }
}

@MainActor
final class Store: ObservableObject {
  @Published var macs: [Mac] = []
  @Published var pending: [Pending] = []
  @Published var offer: PairOffer?
  @Published var status: String?
  @Published var log: [LogEntry] = []
  /// Requests the Mac says are settled (at the Mac, or answered here).
  @Published var settled = Set<String>()
  private var answered = Set<String>()
  #if DEBUG
  var demo = false
  #endif

  init() {
    if let d = UserDefaults.standard.data(forKey: "macs"), let m = try? JSONDecoder().decode([Mac].self, from: d) { macs = m }
    if let d = UserDefaults.standard.data(forKey: "log"), let l = try? JSONDecoder().decode([LogEntry].self, from: d) { log = l }
    #if DEBUG
    startDemo()
    #endif
  }

  private func persist() { UserDefaults.standard.set(try? JSONEncoder().encode(macs), forKey: "macs") }

  func record(_ e: LogEntry) {
    log.insert(e, at: 0)
    if log.count > 100 { log.removeLast(log.count - 100) }
    #if DEBUG
    if demo { return }
    #endif
    UserDefaults.standard.set(try? JSONEncoder().encode(log), forKey: "log")
  }

  // --- pairing: chromerelay://pair?v=1&s=<server>&t=<topic>&k=<Mac key>&p=<secret>&n=<Mac name> ---

  struct PairOffer: Identifiable {
    var id: String { topic }
    let server, topic, name: String
    let relayKey, secret: Data
  }

  func open(_ url: URL) {
    guard url.scheme == "chromerelay" else { return }
    if url.host == "pair" {
      let q = Dictionary(uniqueKeysWithValues: (URLComponents(url: url, resolvingAgainstBaseURL: false)?.queryItems ?? []).map { ($0.name, $0.value ?? "") })
      guard q["v"] == "1", let s = q["s"], let t = q["t"], let k = Data(base64URL: q["k"] ?? ""), let p = Data(base64URL: q["p"] ?? ""),
            s.hasPrefix("https://") || s.hasPrefix("http://") else { status = "That isn't a Chrome Relay pairing code."; return }
      offer = PairOffer(server: s, topic: t, name: q["n"] ?? "Mac", relayKey: k, secret: p)
    }
    Task { await refresh() }
  }

  /// Pairs with Face ID; nil when paired, else what went wrong. The pairing screen clears `offer` once it has said so.
  func pair(_ o: PairOffer) async -> String? {
    #if DEBUG
    if demo { return await demoPair(o) }
    #endif
    do {
      let keys = try Keys.make()
      let sign = try keys.signPublic
      let proof = try keys.sign(Data("\(Tags.pair)\n".utf8) + o.secret, reason: "Pair with \(o.name) to approve its agents' requests")
      var fields: [String: Any] = ["sign": sign.base64EncodedString(), "device": UIDevice.current.name, "proof": proof.base64EncodedString()]
      if let hex = PushDelegate.address { // where Apple push reaches this app, so the Mac can wake it
        fields["push"] = ["token": hex, "topic": Bundle.main.bundleIdentifier ?? "", "sandbox": PushAddress.isSandbox]
      }
      let body = try JSONSerialization.data(withJSONObject: fields)
      let sealed = try keys.seal(body, to: o.relayKey, salt: o.secret, info: Tags.pair)
      let msg = try JSONSerialization.data(withJSONObject: ["v": 1, "box": keys.box.publicKey.x963Representation.base64EncodedString(),
                                                            "data": sealed.base64EncodedString()])
      try Vault.save(keys.box.dataRepresentation, as: o.topic + ".box")
      try Vault.save(keys.signBlob, as: o.topic + ".sign")
      try await Ntfy.publish(o.server, o.topic + "-pair", msg)
      // the Mac answers with a hash of the secret once it has checked us
      let want = Data(SHA256.hash(data: Data("ack\n".utf8) + o.secret)).base64EncodedString()
      for _ in 0..<20 {
        let acks = try await Ntfy.poll(o.server, o.topic + "-pair", since: "5m")
        if acks.contains(where: { (try? JSONSerialization.jsonObject(with: $0) as? [String: Any])?["ack"] as? String == want }) {
          macs.removeAll { $0.topic == o.topic }
          macs.append(Mac(name: o.name, server: o.server, topic: o.topic, relayKey: o.relayKey, paired: Date()))
          persist()
          record(LogEntry(time: Date(), mac: o.name, site: URL(string: o.server)?.host, result: .paired))
          return nil
        }
        try await Task.sleep(for: .seconds(1.5))
      }
      return "\(o.name) didn't confirm. Is `chrome-relay phone pair` still waiting there?"
    } catch {
      return "Pairing failed: \(error.localizedDescription)"
    }
  }

  func remove(_ m: Mac) {
    Vault.delete(m.topic + ".box"); Vault.delete(m.topic + ".sign")
    macs.removeAll { $0 == m }
    persist()
  }

  // --- requests ---

  func refresh() async {
    #if DEBUG
    if demo { return }
    #endif
    var found: [Pending] = []
    var unreachable: String?
    for mac in macs {
      guard let keys = mac.keys else { continue }
      do {
        let done = try await Ntfy.poll(mac.server, mac.topic + "-res").compactMap { (try? JSONSerialization.jsonObject(with: $0) as? [String: Any])?["id"] as? String }
        settled.formUnion(done)
        for raw in try await Ntfy.poll(mac.server, mac.topic + "-req") {
          guard let m = try? JSONSerialization.jsonObject(with: raw) as? [String: Any], let id = m["id"] as? String,
                let data = Data(base64Encoded: m["data"] as? String ?? ""), !done.contains(id), !answered.contains(id),
                let plain = try? keys.open(data, from: mac.relayKey, salt: Data(id.utf8), info: Tags.request),
                let r = try? JSONSerialization.jsonObject(with: plain) as? [String: Any],
                r["id"] as? String == id,
                let expires = (r["expires"] as? String).flatMap(Self.date), expires > Date() else { continue }
          found.append(Pending(id: id, mac: mac, macName: r["mac"] as? String ?? mac.name, reason: r["reason"] as? String ?? "",
                               agent: Self.text(r["agent"]), site: Self.text(r["site"]), what: Self.text(r["what"]),
                               created: (r["created"] as? String).flatMap(Self.date) ?? Date(), expires: expires, plain: plain))
        }
      } catch {
        unreachable = "Can't reach \(mac.server): \(error.localizedDescription)"
      }
    }
    pending = found.sorted { $0.created > $1.created }
    if let u = unreachable { status = u } else if status?.hasPrefix("Can't reach") == true { status = nil }
  }

  func answer(_ p: Pending, approve: Bool) async -> Outcome {
    #if DEBUG
    if demo { return await demoAnswer(p, approve: approve) }
    #endif
    var a: [String: Any] = ["v": 1, "id": p.id, "ok": approve]
    if approve {
      guard let keys = p.mac.keys else { status = "This iPhone lost its key for \(p.mac.name): pair again."; return .failed }
      let msg = approval(id: p.id, plain: p.plain), reason = "Approve on \(p.macName): \(p.reason)"
      do {
        // off the main thread, so the screen keeps drawing while Face ID asks
        a["sig"] = try await Task.detached { try keys.sign(msg, reason: reason) }.value.base64EncodedString()
      } catch {
        status = "Not signed: \(error.localizedDescription)"
        return .notSigned
      }
    }
    do {
      try await Ntfy.publish(p.mac.server, p.mac.topic + "-res", JSONSerialization.data(withJSONObject: a))
    } catch {
      status = "Not sent: \(error.localizedDescription)"
      return .failed
    }
    answered.insert(p.id)
    pending.removeAll { $0.id == p.id }
    if status?.hasPrefix("Not s") == true { status = nil }
    record(LogEntry(time: Date(), mac: p.macName, agent: p.agent, site: p.site, what: p.what ?? (p.site == nil ? p.reason : nil),
                    result: approve ? .approved : .denied))
    return .sent
  }

  private static func date(_ s: String) -> Date? { ISO8601DateFormatter().date(from: s) }
  private static func text(_ v: Any?) -> String? {
    guard let s = (v as? String)?.trimmingCharacters(in: .whitespacesAndNewlines), !s.isEmpty else { return nil }
    return s
  }
}
