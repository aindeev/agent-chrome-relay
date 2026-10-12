import SwiftUI

@main
struct ApproveApp: App {
  @UIApplicationDelegateAdaptor(PushDelegate.self) private var push
  @StateObject private var store = Store()
  @Environment(\.scenePhase) private var phase

  var body: some Scene {
    WindowGroup {
      ContentView()
        .environmentObject(store)
        .onOpenURL { store.open($0) }
        .onChange(of: phase) { _, p in if p == .active { Task { await store.refresh() } } }
        .onReceive(NotificationCenter.default.publisher(for: .pushAddress)) { n in
          if let hex = n.object as? String { Task { await store.sendPushAddress(hex) } }
        }
        .task { if let hex = PushDelegate.address { await store.sendPushAddress(hex) } }
    }
  }
}

/// One screen, three states: PAIR REQUEST, INCOMING (the newest request), LISTENING.
struct ContentView: View {
  @EnvironmentObject var store: Store
  @State private var current: Pending?
  @State private var finished = Set<String>()
  @State private var showLog = false
  private let tick = Timer.publish(every: 3, on: .main, in: .common).autoconnect()

  private var waiting: Int { store.pending.filter { $0.id != current?.id && !finished.contains($0.id) }.count }

  var body: some View {
    ZStack {
      Tx.bg.ignoresSafeArea()
      ScanBand().ignoresSafeArea()
      VStack(alignment: .leading, spacing: 0) {
        HStack(alignment: .firstTextBaseline, spacing: 14) {
          if store.offer != nil {
            Heading(text: "PAIR REQUEST")
          } else if current != nil {
            Heading(text: "INCOMING")
            if waiting > 0 { Text("+\(waiting) WAITING").label() }
          } else {
            Heading(text: "LISTENING", dot: Tx.dim, period: 2.4)
          }
          Spacer()
          TextButton(title: "LOG") { showLog = true }.offset(x: 6)
        }
        .lineLimit(1)
        .minimumScaleFactor(0.6)
        .padding(.top, 12)
        Group {
          if let o = store.offer {
            PairScreen(o: o).id(o.id)
          } else if let p = current {
            RequestScreen(p: p) { finish(p.id) }.id(p.id)
          } else {
            IdleScreen()
          }
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
        .transition(.opacity)
        if let s = store.status {
          Text(s).mono(10).foregroundStyle(Tx.dim).lineLimit(2).padding(.bottom, 6)
        }
      }
      .padding(.horizontal, 28)
      Scanlines().ignoresSafeArea()
    }
    .fontDesign(.monospaced)
    .preferredColorScheme(.dark)
    .dynamicTypeSize(...DynamicTypeSize.accessibility1)
    .animation(.easeOut(duration: 0.3), value: current?.id)
    .animation(.easeOut(duration: 0.3), value: store.offer?.id)
    .sheet(isPresented: $showLog) { LogSheet() }
    .onReceive(tick) { _ in Task { await store.refresh() } }
    .onChange(of: store.pending, initial: true) { advance() }
    #if DEBUG
    .onAppear { if Store.demoArgs.contains("-demoLog") { showLog = true } }
    #endif
  }

  /// Keep the request on screen until it is answered, expires or is settled; then the newest one still waiting.
  private func advance() {
    guard current == nil else { return }
    current = store.pending.first { !finished.contains($0.id) && $0.expires > Date() }
  }

  private func finish(_ id: String) {
    finished.insert(id)
    if current?.id == id { current = nil }
    advance()
  }
}
