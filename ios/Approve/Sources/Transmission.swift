import SwiftUI
import UIKit

// "Transmission" (ios/design/concepts.html): a dark terminal, small monospaced type, faint scanlines, lines that type in.

enum Tx {
  static let bg = Color(hex: 0x01070e)
  static let cyan = Color(hex: 0x5fe3ff)
  static let glow = Color(hex: 0x29b6ff)
  static let green = Color(hex: 0x4dffb5)
  static let red = Color(hex: 0xff5f7a)
  static let dim = Color(hex: 0x6f8aa3)
  static let ice = Color(hex: 0xe8fbff)

  static let perChar: Duration = .milliseconds(14)
  static let betweenLines: Duration = .milliseconds(70)

  static func tick() { UIImpactFeedbackGenerator(style: .light).impactOccurred() }
  static func notify(_ t: UINotificationFeedbackGenerator.FeedbackType) { UINotificationFeedbackGenerator().notificationOccurred(t) }

  /// "www.twilio.com" → "twilio.com"
  static func host(_ s: String) -> String { s.hasPrefix("www.") ? String(s.dropFirst(4)) : s }
  static func host(url: String) -> String { URL(string: url)?.host ?? url }
  static func shortDate(_ d: Date) -> String { d.formatted(.dateTime.month(.twoDigits).day(.twoDigits)) }
}

extension Color {
  init(hex: UInt32) {
    self.init(red: Double((hex >> 16) & 0xff) / 255, green: Double((hex >> 8) & 0xff) / 255, blue: Double(hex & 0xff) / 255)
  }
}

// MARK: type

/// Monospaced at a fixed design size that still follows Dynamic Type.
private struct Mono: ViewModifier {
  @ScaledMetric private var size: CGFloat
  let weight: Font.Weight
  let tracking: CGFloat

  init(size: CGFloat, weight: Font.Weight, tracking: CGFloat, relativeTo style: Font.TextStyle) {
    _size = ScaledMetric(wrappedValue: size, relativeTo: style)
    self.weight = weight
    self.tracking = tracking
  }

  func body(content: Content) -> some View {
    content.font(.system(size: size, weight: weight, design: .monospaced)).tracking(tracking)
  }
}

extension View {
  func mono(_ size: CGFloat, _ weight: Font.Weight = .regular, tracking: CGFloat = 0) -> some View {
    modifier(Mono(size: size, weight: weight, tracking: tracking, relativeTo: size < 12 ? .caption2 : size < 15 ? .footnote : .body))
  }
  /// The uppercase labels: 10–11 pt, wide tracking.
  func label(_ size: CGFloat = 10, _ color: Color = Tx.dim) -> some View {
    mono(size, .semibold, tracking: size * 0.22).foregroundStyle(color)
  }
}

// MARK: atmosphere

/// Faint horizontal lines every 3 pt over everything.
struct Scanlines: View {
  var body: some View {
    Canvas { ctx, size in
      var y: CGFloat = 0
      var path = Path()
      while y < size.height { path.addRect(CGRect(x: 0, y: y, width: size.width, height: 1)); y += 3 }
      ctx.fill(path, with: .color(Tx.cyan.opacity(0.035)))
    }
    .allowsHitTesting(false)
    .accessibilityHidden(true)
  }
}

/// A soft band of light sweeping top to bottom every 5 s. Off with Reduce Motion.
struct ScanBand: View {
  @Environment(\.accessibilityReduceMotion) private var reduceMotion
  private let band: CGFloat = 140, period = 5.0

  var body: some View {
    if !reduceMotion {
      GeometryReader { geo in
        TimelineView(.animation(minimumInterval: 1.0 / 30)) { tl in
          let f = tl.date.timeIntervalSinceReferenceDate.truncatingRemainder(dividingBy: period) / period
          LinearGradient(colors: [.clear, Tx.cyan.opacity(0.07), .clear], startPoint: .top, endPoint: .bottom)
            .frame(height: band)
            .offset(y: -band + f * (geo.size.height + band))
        }
      }
      .allowsHitTesting(false)
      .accessibilityHidden(true)
    }
  }
}

/// On for `period`/2, off for `period`/2, like CSS steps(1).
struct Blink<Content: View>: View {
  var period = 1.0
  @ViewBuilder var content: (Bool) -> Content

  var body: some View {
    TimelineView(.periodic(from: .now, by: period / 2)) { tl in
      content(Int(tl.date.timeIntervalSinceReferenceDate / (period / 2)) % 2 == 0)
    }
  }
}

/// "● INCOMING": the dot blinks.
struct Heading: View {
  let text: String
  var dot: Color = Tx.green
  var period = 1.2

  var body: some View {
    Blink(period: period) { on in
      Text("\(Text("● ").foregroundStyle(dot.opacity(on ? 1 : 0)))\(Text(text))")
    }
    .mono(11, .semibold, tracking: 2.6)
    .foregroundStyle(Tx.cyan)
    .accessibilityElement()
    .accessibilityLabel(text.capitalized)
  }
}

// MARK: typing

/// One typed line and how it looks.
struct TxLine: Hashable {
  enum Kind { case label, site, body }
  let text: String
  let kind: Kind
  static func label(_ s: String) -> TxLine { TxLine(text: s.uppercased(), kind: .label) }
  static func site(_ s: String) -> TxLine { TxLine(text: s, kind: .site) }
  static func body(_ s: String) -> TxLine { TxLine(text: s, kind: .body) }
}

/// Lines that type in fast with a block caret, a light haptic tick per finished line. Each line holds its space from
/// the start, so nothing below moves while it types.
struct TypedLines: View {
  let lines: [TxLine]
  var onDone: () -> Void = {}
  @Environment(\.accessibilityReduceMotion) private var reduceMotion
  @State private var line = 0
  @State private var chars = 0

  var body: some View {
    VStack(alignment: .leading, spacing: 0) {
      ForEach(Array(lines.enumerated()), id: \.offset) { i, l in
        row(l, shown: i < line ? l.text.count : i == line ? chars : 0, caret: i == line)
          .padding(.bottom, l.kind == .label ? 8 : 12)
      }
    }
    .accessibilityElement(children: .ignore)
    .accessibilityLabel(lines.map(\.text).joined(separator: ", "))
    .task { await type() }
  }

  private func row(_ l: TxLine, shown: Int, caret: Bool) -> some View {
    ZStack(alignment: .topLeading) {
      Text(l.text + " ").hidden()
      if caret || shown > 0 {
        Blink(period: 1) { on in
          Text("\(Text(String(l.text.prefix(shown))))\(Text("█").foregroundStyle(caret && on ? Tx.cyan : .clear))")
        }
      }
    }
    .modifier(LineStyle(kind: l.kind))
    .fixedSize(horizontal: false, vertical: true)
  }

  private func type() async {
    if reduceMotion { line = lines.count; onDone(); return }
    for (i, l) in lines.enumerated() {
      line = i
      chars = 0
      for _ in l.text {
        try? await Task.sleep(for: Tx.perChar)
        if Task.isCancelled { return }
        chars += 1
      }
      Tx.tick()
      try? await Task.sleep(for: Tx.betweenLines)
    }
    line = lines.count
    onDone()
  }
}

private struct LineStyle: ViewModifier {
  let kind: TxLine.Kind
  func body(content: Content) -> some View {
    switch kind {
    case .label: content.label(11)
    case .site: content.mono(16).foregroundStyle(Tx.cyan).shadow(color: Tx.glow.opacity(0.6), radius: 7)
    case .body: content.mono(16).foregroundStyle(Tx.ice)
    }
  }
}

/// A one-line result ("✓ SIGNED · SENT TO …") that types at 20 ms a character.
struct TypedResult: View {
  let text: String
  let color: Color
  @Environment(\.accessibilityReduceMotion) private var reduceMotion
  @State private var chars = 0

  var body: some View {
    Text(String(text.prefix(chars)))
      .mono(12, .semibold, tracking: 1.7)
      .foregroundStyle(color)
      .fixedSize(horizontal: false, vertical: true)
      .accessibilityLabel(text)
      .task(id: text) {
        if reduceMotion { chars = text.count; return }
        chars = 0
        for _ in text {
          try? await Task.sleep(for: .milliseconds(20))
          if Task.isCancelled { return }
          chars += 1
        }
      }
  }
}

// MARK: controls

/// The one thing to tap: a thin cyan ring around the Face ID glyph, its label under it.
struct FaceIDButton: View {
  let label: String
  var busy = false
  let action: () -> Void
  @ScaledMetric(relativeTo: .body) private var size: CGFloat = 84
  @Environment(\.accessibilityReduceMotion) private var reduceMotion

  var body: some View {
    Button(action: action) {
      VStack(spacing: 14) {
        ZStack {
          Circle().fill(Tx.cyan.opacity(0.05))
          Circle().strokeBorder(Tx.cyan.opacity(0.35), lineWidth: 1.5)
          Image(systemName: "faceid")
            .font(.system(size: size * 0.4, weight: .light))
            .foregroundStyle(Tx.cyan)
            .shadow(color: Tx.glow.opacity(0.5), radius: 6)
        }
        .frame(width: size, height: size)
        .overlay {
          if busy && !reduceMotion {
            Blink(period: 1.2) { on in
              Circle().strokeBorder(Tx.cyan.opacity(on ? 0.7 : 0.15), lineWidth: 1.5).animation(.easeInOut(duration: 0.5), value: on)
            }
          }
        }
        Text(label).label()
      }
      .contentShape(Rectangle())
    }
    .buttonStyle(.plain)
    .disabled(busy)
    .accessibilityLabel(label.capitalized)
  }
}

/// "DENY", "CANCEL", "LOG": small tracked text.
struct TextButton: View {
  let title: String
  var color: Color = Tx.dim
  let action: () -> Void

  var body: some View {
    Button(action: action) {
      Text(title).label(10, color).padding(.vertical, 10).padding(.horizontal, 6).contentShape(Rectangle())
    }
    .buttonStyle(.plain)
    .accessibilityLabel(title.capitalized)
  }
}
