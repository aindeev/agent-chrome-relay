#if DEBUG
import Foundation

// A request to look at in the simulator, which has no Mac, Secure Enclave or Face ID. Debug builds only.
// Launch arguments: -demo (a request, a paired Mac and some history), plus -demoQueue (two more waiting),
// -demoIdle (no request), -demoPair (a pairing link), -demoLog (open the log), -demoApprove (tap to sign by itself).
// Nothing here is saved: the demo Mac and history stay in memory.
extension Store {
  static var demoArgs: Set<String> { Set(ProcessInfo.processInfo.arguments.filter { $0.hasPrefix("-demo") }) }

  func startDemo() {
    let args = Self.demoArgs
    guard !args.isEmpty else { return }
    demo = true
    let mac = Mac(name: "MacBook Pro", server: "https://chrome-relay-push.aindeev.workers.dev", topic: "demo", relayKey: Data(),
                  paired: Calendar.current.date(byAdding: .day, value: -3, to: Date())!)
    if args.contains("-demoPair") {
      offer = PairOffer(server: "https://chrome-relay-push.aindeev.workers.dev", topic: "demo-pair", name: "MacBook Pro", relayKey: Data(), secret: Data())
      return
    }
    macs = [mac]
    let now = Date()
    log = [
      LogEntry(time: now.addingTimeInterval(-180), mac: "MacBook Pro", agent: "Claude Code (my-repo)", site: "www.twilio.com",
               what: "password + one-time code", result: .approved),
      LogEntry(time: now.addingTimeInterval(-1_800), mac: "MacBook Pro", agent: "Codex", site: "aws.amazon.com",
               what: "one-time code", result: .approved),
      LogEntry(time: now.addingTimeInterval(-2_700), mac: "MacBook Pro", agent: "Cursor", site: "github.com",
               what: "password", result: .denied),
      LogEntry(time: now.addingTimeInterval(-3_600), mac: "MacBook Pro", agent: "Claude Code (my-repo)", site: "accounts.google.com",
               what: "passkey as bot@acme.com", result: .approved),
      LogEntry(time: now.addingTimeInterval(-90_000), mac: "MacBook Pro", site: "chrome-relay-push.aindeev.workers.dev", result: .paired),
    ]
    if args.contains("-demoIdle") { return }
    func request(_ id: String, _ agent: String, _ site: String, _ what: String, why: String?, age: TimeInterval) -> Pending {
      Pending(id: id, mac: mac, macName: "MacBook Pro", reason: "let \(agent) fill the \(what) on \(site)",
              agent: agent, site: site, what: what, why: why, noWhy: why == nil, created: now.addingTimeInterval(-age),
              expires: now.addingTimeInterval(120 - age), plain: Data())
    }
    pending = [request("demo-1", "Claude Code (my-repo)", "www.twilio.com", "password + one-time code",
                       why: "Download March's invoice for the books", age: 0)]
    if args.contains("-demoQueue") {
      pending += [request("demo-2", "Codex", "aws.amazon.com", "one-time code", why: "Rotate the staging deploy keys", age: 20),
                  request("demo-3", "Cursor", "github.com", "password", why: nil, age: 40)]
    }
  }

  func demoAnswer(_ p: Pending, approve: Bool) async -> Outcome {
    try? await Task.sleep(for: .milliseconds(approve ? 700 : 150))
    pending.removeAll { $0.id == p.id }
    record(LogEntry(time: Date(), mac: p.macName, agent: p.agent, site: p.site, what: p.what, result: approve ? .approved : .denied))
    return .sent
  }

  func demoPair(_ o: PairOffer) async -> String? {
    try? await Task.sleep(for: .milliseconds(900))
    macs = [Mac(name: o.name, server: o.server, topic: o.topic, relayKey: o.relayKey, paired: Date())]
    record(LogEntry(time: Date(), mac: o.name, site: URL(string: o.server)?.host, result: .paired))
    return nil
  }
}
#endif
