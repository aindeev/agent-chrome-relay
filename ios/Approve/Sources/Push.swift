import UIKit
import UserNotifications

extension Notification.Name { static let pushAddress = Notification.Name("pushAddress") }

/// Apple push: asks once for permission, registers, and hands the push address to the Store, which seals it to
/// each paired Mac. A push only says an approval is waiting; tapping it opens the app on the request.
final class PushDelegate: NSObject, UIApplicationDelegate, UNUserNotificationCenterDelegate {
  static var address: String? { UserDefaults.standard.string(forKey: "pushAddress") }

  func application(_ app: UIApplication, didFinishLaunchingWithOptions _: [UIApplication.LaunchOptionsKey: Any]? = nil) -> Bool {
    let center = UNUserNotificationCenter.current()
    center.delegate = self
    #if DEBUG
    if !Store.demoArgs.isEmpty { return true } // the demo (screenshots) asks for nothing
    #endif
    center.requestAuthorization(options: [.alert, .sound, .badge]) { ok, _ in
      if ok { DispatchQueue.main.async { app.registerForRemoteNotifications() } }
    }
    return true
  }

  func application(_: UIApplication, didRegisterForRemoteNotificationsWithDeviceToken token: Data) {
    let hex = token.map { String(format: "%02x", $0) }.joined()
    UserDefaults.standard.set(hex, forKey: "pushAddress")
    NotificationCenter.default.post(name: .pushAddress, object: hex)
  }

  func application(_: UIApplication, didFailToRegisterForRemoteNotificationsWithError error: Error) {
    UserDefaults.standard.removeObject(forKey: "pushAddress") // no push (a free Apple ID build): the Mac keeps using ntfy
  }

  func userNotificationCenter(_: UNUserNotificationCenter, willPresent _: UNNotification,
                              withCompletionHandler done: @escaping (UNNotificationPresentationOptions) -> Void) {
    done([.banner, .sound]) // the app refreshes every few seconds anyway; the banner says why it buzzed
  }

  func userNotificationCenter(_: UNUserNotificationCenter, didReceive _: UNNotificationResponse, withCompletionHandler done: @escaping () -> Void) {
    done() // opening the app is enough: it refreshes as it becomes active
  }
}

/// Where Apple push reaches this app, as the Mac reads it (internal/phone Device).
struct PushAddress: Codable {
  let token: String
  let topic: String   // this app's bundle id
  let sandbox: Bool   // development-signed builds (./ios/build) get sandbox addresses; TestFlight and App Store ones don't

  /// Which APNs this copy of the app gets push through. A development build carries its provisioning profile, which
  /// says aps-environment "development"; TestFlight and App Store builds carry none and use production.
  static let isSandbox: Bool = {
    guard let url = Bundle.main.url(forResource: "embedded", withExtension: "mobileprovision"),
          let raw = try? Data(contentsOf: url),
          let text = String(data: raw, encoding: .isoLatin1),
          let start = text.range(of: "<?xml"), let end = text.range(of: "</plist>"),
          let plist = try? PropertyListSerialization.propertyList(from: Data(text[start.lowerBound..<end.upperBound].utf8), format: nil) as? [String: Any],
          let ent = plist["Entitlements"] as? [String: Any]
    else { return false }
    return (ent["aps-environment"] as? String) == "development"
  }()
}

extension Store {
  /// Seals the push address to each paired Mac on "<topic>-dev"; the Mac picks the latest up before it next asks.
  func sendPushAddress(_ hex: String) async {
    let address = PushAddress(token: hex, topic: Bundle.main.bundleIdentifier ?? "", sandbox: PushAddress.isSandbox)
    guard let plain = try? JSONEncoder().encode(address) else { return }
    for mac in macs {
      guard let keys = mac.keys,
            let sealed = try? keys.seal(plain, to: mac.relayKey, salt: Data(mac.topic.utf8), info: Tags.device) else { continue }
      try? await Ntfy.publish(mac.server, mac.topic + "-dev", Data(sealed.base64EncodedString().utf8))
    }
  }
}
