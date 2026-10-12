// Draws Chrome Relay's icon as vector art, at every size it's used:
//   swift icon/icon.swift            (from the repo root)
// writes extension/icons/icon-{16,32,48,128}.png, docs/icon{,-256,-1024}.png (a rounded navy tile,
// transparent corners) and the iPhone app's full-bleed square (iOS rounds the corners itself).
import AppKit

let cyan = CGColor(srgbRed: 0.37, green: 0.89, blue: 1.0, alpha: 1)
let glow = CGColor(srgbRed: 0.16, green: 0.71, blue: 1.0, alpha: 1)

// the pointer, in a 24-unit box (tip at the top left)
func pointer() -> CGPath {
  let p = CGMutablePath()
  p.move(to: CGPoint(x: 4, y: 2)); p.addLine(to: CGPoint(x: 4, y: 19)); p.addLine(to: CGPoint(x: 8.5, y: 14.8))
  p.addLine(to: CGPoint(x: 11.4, y: 21.4)); p.addLine(to: CGPoint(x: 14.2, y: 20.2)); p.addLine(to: CGPoint(x: 11.4, y: 13.7))
  p.addLine(to: CGPoint(x: 17.6, y: 13.7)); p.closeSubpath()
  return p
}


func draw(size: Int, tile: Bool) -> NSBitmapImageRep {
  let s = CGFloat(size)
  let ctx = CGContext(data: nil, width: size, height: size, bitsPerComponent: 8, bytesPerRow: 0, space: CGColorSpace(name: CGColorSpace.sRGB)!,
                      bitmapInfo: tile ? CGImageAlphaInfo.premultipliedLast.rawValue : CGImageAlphaInfo.noneSkipLast.rawValue)!
  ctx.translateBy(x: 0, y: s); ctx.scaleBy(x: 1, y: -1) // y down, like the path
  // the tile: deep navy, lighter at the top, rounded unless iOS rounds it
  let inset = tile ? s * 0.04 : 0, r = tile ? s * 0.225 : 0
  let rect = CGRect(x: inset, y: inset, width: s - 2 * inset, height: s - 2 * inset)
  let shape = CGPath(roundedRect: rect, cornerWidth: r, cornerHeight: r, transform: nil)
  ctx.saveGState(); ctx.addPath(shape); ctx.clip()
  let bg = CGGradient(colorsSpace: nil, colors: [CGColor(srgbRed: 0.04, green: 0.15, blue: 0.25, alpha: 1), CGColor(srgbRed: 0.01, green: 0.04, blue: 0.08, alpha: 1)] as CFArray, locations: [0, 1])!
  ctx.drawLinearGradient(bg, start: CGPoint(x: s / 2, y: 0), end: CGPoint(x: s / 2, y: s), options: [])
  let halo = CGGradient(colorsSpace: nil, colors: [CGColor(srgbRed: 0.16, green: 0.71, blue: 1, alpha: 0.12), CGColor(srgbRed: 0.16, green: 0.71, blue: 1, alpha: 0)] as CFArray, locations: [0, 1])!
  ctx.drawRadialGradient(halo, startCenter: CGPoint(x: s * 0.55, y: s * 0.45), startRadius: 0, endCenter: CGPoint(x: s * 0.55, y: s * 0.45), endRadius: s * 0.5, options: [])
  ctx.restoreGState()
  // the pointer: 40% of the tile, a crisp edge, barely any glow; one faint ghost behind it at larger sizes
  let unit = s * 0.40 / 19.4, path = pointer()
  let cx = s * 0.53, cy = s * 0.5 // where the pointer's middle sits
  func place(_ dx: CGFloat, _ dy: CGFloat) -> CGAffineTransform {
    CGAffineTransform(translationX: cx - 11 * unit + dx, y: cy - 11.7 * unit + dy).scaledBy(x: unit, y: unit)
  }
  if size >= 48 {
    let back = s * 0.07
    var t = place(-back, back * 0.45)
    ctx.addPath(path.copy(using: &t)!); ctx.setFillColor(cyan.copy(alpha: 0.12)!); ctx.fillPath()
  }
  var t = place(0, 0)
  let main = path.copy(using: &t)!
  ctx.saveGState() // the glow, tight
  ctx.setShadow(offset: .zero, blur: max(1, s * 0.012), color: glow.copy(alpha: 0.9))
  ctx.addPath(main); ctx.setFillColor(cyan); ctx.fillPath()
  ctx.restoreGState()
  ctx.saveGState() // the body: white to ice cyan, crisp
  ctx.addPath(main); ctx.clip()
  let body = CGGradient(colorsSpace: nil, colors: [CGColor(srgbRed: 1, green: 1, blue: 1, alpha: 1), CGColor(srgbRed: 0.72, green: 0.94, blue: 1, alpha: 1)] as CFArray, locations: [0, 1])!
  let b = main.boundingBox
  ctx.drawLinearGradient(body, start: CGPoint(x: b.minX, y: b.minY), end: CGPoint(x: b.maxX, y: b.maxY), options: [])
  ctx.restoreGState()
  ctx.addPath(main); ctx.setStrokeColor(CGColor(srgbRed: 1, green: 1, blue: 1, alpha: 0.9)); ctx.setLineWidth(max(0.5, s * 0.004)); ctx.setLineJoin(.round); ctx.strokePath()
  return NSBitmapImageRep(cgImage: ctx.makeImage()!)
}

func save(_ rep: NSBitmapImageRep, _ path: String) {
  try! rep.representation(using: .png, properties: [:])!.write(to: URL(fileURLWithPath: path))
  print("wrote", path)
}
for n in [16, 32, 48, 128] { save(draw(size: n, tile: true), "extension/icons/icon-\(n).png") }
save(draw(size: 1024, tile: true), "docs/icon-1024.png")
save(draw(size: 256, tile: true), "docs/icon-256.png")
save(draw(size: 128, tile: true), "docs/icon.png")
save(draw(size: 1024, tile: false), "ios/Approve/Assets.xcassets/AppIcon.appiconset/icon-1024.png")
