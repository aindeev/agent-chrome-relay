import SwiftUI

/// What a screen types last: "✓ SIGNED · SENT TO MACBOOK PRO", "✕ DENIED", "· NOT SIGNED".
struct Said: Equatable {
  let text: String
  let color: Color
}

/// The gap between the heading and the typed lines: generous on tall iPhones, less on small ones.
private struct TopGap: View {
  var body: some View { Spacer(minLength: 32).frame(maxHeight: 120) }
}

// MARK: a request

struct RequestScreen: View {
  @EnvironmentObject var store: Store
  let p: Pending
  let done: () -> Void

  private enum Phase { case typing, ready, busy, finished }
  @State private var phase = Phase.typing
  @State private var said: Said?
  @State private var signing = false

  private var lines: [TxLine] {
    var l = [TxLine.label(p.agent ?? "Request")]
    if let site = p.site {
      l.append(.site(Tx.host(site)))
      if let what = p.what { l.append(.body(what)) }
    } else {
      l.append(.body(p.reason.prefix(1).uppercased() + p.reason.dropFirst()))
    }
    // the agent's reason, in its own words: quoted, since Chrome Relay checks the site and account, not this
    if let why = p.why {
      l += [.label("It says why"), .quote("“\(why)”")]
    } else if p.noWhy {
      l.append(.label("No reason given"))
    }
    return l
  }

  private var controls: Bool { phase == .ready || phase == .busy }

  var body: some View {
    VStack(alignment: .leading, spacing: 0) {
      TopGap()
      TypedLines(lines: lines) { if phase == .typing { phase = .ready } }
      HStack(alignment: .firstTextBaseline) {
        Text(p.macName.uppercased()).label()
        Spacer()
        TimelineView(.periodic(from: .now, by: 1)) { tl in
          let s = max(0, Int(p.expires.timeIntervalSince(tl.date).rounded(.up)))
          Text("\(s / 60):\(String(format: "%02d", s % 60))").mono(11, .semibold).foregroundStyle(Tx.dim)
            .accessibilityLabel("Expires in \(s) seconds")
        }
      }
      .padding(.top, 6)
      .opacity(phase == .typing ? 0 : 1)
      Group {
        if let said { TypedResult(text: said.text, color: said.color) } else { Text(" ").mono(12) }
      }
      .padding(.top, 28)
      Spacer(minLength: 24)
      ZStack(alignment: .topTrailing) {
        FaceIDButton(label: "TAP TO SIGN", busy: signing) { Task { await approve() } }
          .frame(maxWidth: .infinity)
        TextButton(title: "DENY") { Task { await deny() } }
          .padding(.top, 38)
          .offset(x: 6)
          .disabled(phase != .ready)
      }
      .opacity(controls ? 1 : 0)
      .allowsHitTesting(controls)
      .animation(.easeOut(duration: 0.3), value: controls)
      .padding(.bottom, 20)
    }
    .task { await expire() }
    .onChange(of: store.settled) { _, settled in
      // answered at the Mac (Touch ID there, or it gave up): nothing left to do here
      guard settled.contains(p.id), phase == .typing || phase == .ready else { return }
      Task { await end(Said(text: "· SETTLED ON THE MAC", color: Tx.dim), after: 1.5) }
    }
    #if DEBUG
    .onChange(of: phase) { _, now in
      if now == .ready, Store.demoArgs.contains("-demoApprove") {
        Task { try? await Task.sleep(for: .seconds(1.2)); await approve() }
      }
    }
    #endif
  }

  private func approve() async {
    guard phase == .ready else { return }
    phase = .busy
    signing = true
    said = nil
    let outcome = await store.answer(p, approve: true)
    signing = false
    switch outcome {
    case .sent:
      Tx.notify(.success)
      await end(Said(text: "✓ SIGNED · SENT TO \(p.macName.uppercased())", color: Tx.green), after: 2.5)
    case .notSigned:
      said = Said(text: "· NOT SIGNED", color: Tx.dim)
      phase = .ready
    case .failed:
      said = Said(text: "· NOT SENT", color: Tx.red)
      phase = .ready
    }
  }

  private func deny() async {
    guard phase == .ready else { return }
    phase = .busy
    said = nil
    if await store.answer(p, approve: false) == .sent {
      Tx.tick()
      await end(Said(text: "✕ DENIED", color: Tx.red), after: 2)
    } else {
      said = Said(text: "· NOT SENT", color: Tx.red)
      phase = .ready
    }
  }

  private func expire() async {
    try? await Task.sleep(for: .seconds(max(0, p.expires.timeIntervalSinceNow)))
    guard !Task.isCancelled, phase == .typing || phase == .ready else { return }
    await end(Said(text: "· EXPIRED", color: Tx.dim), after: 1.5)
  }

  /// Types the last word, waits, and hands over to the next request (or LISTENING).
  private func end(_ s: Said, after seconds: Double) async {
    phase = .finished
    said = s
    try? await Task.sleep(for: .milliseconds(20 * s.text.count) + .seconds(seconds))
    done()
  }
}

// MARK: pairing

struct PairScreen: View {
  @EnvironmentObject var store: Store
  let o: Store.PairOffer

  private enum Phase { case typing, ready, busy, finished }
  @State private var phase = Phase.typing
  @State private var said: Said?

  private var controls: Bool { phase == .ready || phase == .busy }

  var body: some View {
    VStack(alignment: .leading, spacing: 0) {
      TopGap()
      TypedLines(lines: [.label("From"), .site(o.name), .label("Via"), .body(Tx.host(url: o.server))]) {
        if phase == .typing { phase = .ready }
      }
      Text("Only pair a Mac where you just ran chrome-relay phone pair.")
        .mono(11).foregroundStyle(Tx.dim)
        .fixedSize(horizontal: false, vertical: true)
        .opacity(phase == .typing ? 0 : 1)
      Group {
        if let said { TypedResult(text: said.text, color: said.color) } else { Text(" ").mono(12) }
      }
      .padding(.top, 28)
      Spacer(minLength: 24)
      ZStack(alignment: .topTrailing) {
        FaceIDButton(label: phase == .busy ? "PAIRING…" : "TAP TO PAIR", busy: phase == .busy) { Task { await pair() } }
          .frame(maxWidth: .infinity)
        TextButton(title: "CANCEL") { store.offer = nil }
          .padding(.top, 38)
          .offset(x: 6)
          .disabled(phase != .ready)
      }
      .opacity(controls ? 1 : 0)
      .allowsHitTesting(controls)
      .animation(.easeOut(duration: 0.3), value: controls)
      .padding(.bottom, 20)
    }
  }

  private func pair() async {
    guard phase == .ready else { return }
    phase = .busy
    said = nil
    if let err = await store.pair(o) {
      said = Said(text: err, color: Tx.red)
      phase = .ready
      return
    }
    Tx.notify(.success)
    phase = .finished
    said = Said(text: "✓ PAIRED", color: Tx.green)
    try? await Task.sleep(for: .seconds(2.5))
    if store.offer?.id == o.id { store.offer = nil }
  }
}

// MARK: listening

struct IdleScreen: View {
  @EnvironmentObject var store: Store

  var body: some View {
    ScrollView {
      VStack(alignment: .leading, spacing: 0) {
        if store.macs.isEmpty {
          Text("NO MAC PAIRED").label(11).padding(.bottom, 14)
          Text("On your Mac, run\n\(Text("chrome-relay phone pair").foregroundStyle(Tx.cyan))\nthen scan its code with the Camera.")
            .mono(14).foregroundStyle(Tx.ice).lineSpacing(4)
            .fixedSize(horizontal: false, vertical: true)
        } else {
          Blink(period: 1) { on in
            Text("NO REQUESTS\(Text("█").foregroundStyle(on ? Tx.dim.opacity(0.7) : .clear))")
          }
          .label(11)
          .accessibilityLabel("No requests")
          .padding(.bottom, 44)
          ForEach(store.macs) { m in
            VStack(alignment: .leading, spacing: 6) {
              Text(m.name).mono(14).foregroundStyle(Tx.ice)
              Text("PAIRED \(Tx.shortDate(m.paired)) · \(Tx.host(url: m.server))").label()
            }
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(.vertical, 10)
            .contentShape(Rectangle())
            .contextMenu {
              Button("Unpair \(m.name)", systemImage: "xmark", role: .destructive) { store.remove(m) }
            }
          }
        }
      }
      .padding(.top, 120)
      .frame(maxWidth: .infinity, alignment: .leading)
    }
    .scrollIndicators(.hidden)
    .refreshable { await store.refresh() }
  }
}

// MARK: history

struct LogSheet: View {
  @EnvironmentObject var store: Store
  @Environment(\.dismiss) private var dismiss

  private static let clock: DateFormatter = { let f = DateFormatter(); f.dateFormat = "HH:mm"; return f }()

  var body: some View {
    ZStack {
      Tx.bg.ignoresSafeArea()
      ScrollView {
        VStack(alignment: .leading, spacing: 0) {
          HStack {
            Text("LOG").mono(11, .semibold, tracking: 2.6).foregroundStyle(Tx.cyan)
            Spacer()
            TextButton(title: "CLOSE") { dismiss() }.offset(x: 6)
          }
          .padding(.bottom, 12)
          if store.log.isEmpty {
            Text("NOTHING YET").label(11).padding(.top, 10)
          }
          let manyMacs = Set(store.log.map(\.mac)).count > 1
          ForEach(store.log) { e in row(e, manyMacs: manyMacs) }
        }
        .padding(.horizontal, 24)
        .padding(.top, 28)
      }
      Scanlines().ignoresSafeArea()
    }
    .presentationBackground(Tx.bg)
    .presentationDragIndicator(.visible)
  }

  private func row(_ e: LogEntry, manyMacs: Bool) -> some View {
    let (mark, color): (String, Color) = switch e.result {
    case .approved: ("✓", Tx.green)
    case .denied: ("✕", Tx.red)
    case .paired: ("◆", Tx.cyan)
    }
    let title: String, detail: [String]
    switch e.result {
    case .paired:
      title = "PAIRED \(e.mac.uppercased())"
      detail = ["FACE ID + QR"] + (e.site.map { [$0.uppercased()] } ?? [])
    case .approved, .denied:
      let who = (e.agent.map(Self.agentName) ?? e.mac).uppercased()
      title = e.site.map { "\(who)  \(Tx.host($0))" } ?? who
      detail = (e.what.map { [$0.uppercased()] } ?? []) + [e.result == .approved ? "FACE ID" : "DENIED"]
        + (manyMacs ? [e.mac.uppercased()] : [])
    }
    return HStack(alignment: .firstTextBaseline, spacing: 6) {
      Text(when(e.time)).foregroundStyle(Tx.dim).frame(width: 46, alignment: .leading)
      Text(mark).foregroundStyle(color).frame(width: 16, alignment: .leading)
      VStack(alignment: .leading, spacing: 4) {
        Text(title).foregroundStyle(Tx.ice)
        Text(detail.joined(separator: " · ")).mono(10, tracking: 1).foregroundStyle(Tx.dim)
      }
      .fixedSize(horizontal: false, vertical: true)
      Spacer(minLength: 0)
    }
    .mono(12)
    .padding(.vertical, 10)
    .overlay(alignment: .bottom) {
      Line().stroke(Tx.cyan.opacity(0.12), style: StrokeStyle(lineWidth: 1, dash: [3, 3])).frame(height: 1)
    }
    .accessibilityElement(children: .combine)
  }

  /// "Claude Code (my-repo)" → "Claude Code"
  private static func agentName(_ a: String) -> String {
    a.range(of: " (").map { String(a[..<$0.lowerBound]) } ?? a
  }

  private func when(_ d: Date) -> String {
    if Calendar.current.isDateInToday(d) { return Self.clock.string(from: d) }
    if Calendar.current.isDateInYesterday(d) { return "YDAY" }
    return Tx.shortDate(d)
  }
}

private struct Line: Shape {
  func path(in r: CGRect) -> Path { Path { $0.move(to: CGPoint(x: r.minX, y: r.midY)); $0.addLine(to: CGPoint(x: r.maxX, y: r.midY)) } }
}
