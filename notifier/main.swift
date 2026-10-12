import AppKit
import UserNotifications

// Chrome Relay.app: macOS notifications in Chrome Relay's own name, for when an agent needs the person. Chrome's
// notifications are often switched off, and the extension's toolbar badge only shows while it is pinned.
//   notify <id> <title> <body> [<chrome-relay binary> <profile> <tab>]   post one; a click brings that tab forward
//   clear <id>                                                            take one back
//   status                                                                print authorized | denied | not asked
// Launched with nothing (by a click on a notification), it runs `<binary> attend <profile> <tab>` and quits.
final class Notifier: NSObject, NSApplicationDelegate, UNUserNotificationCenterDelegate {
  let center = UNUserNotificationCenter.current()
  func quit(after s: Double = 0) { DispatchQueue.main.asyncAfter(deadline: .now() + s) { NSApp.terminate(nil) } }

  func applicationDidFinishLaunching(_ n: Notification) {
    center.delegate = self
    let a = Array(CommandLine.arguments.dropFirst())
    switch a.first {
    case "notify" where a.count >= 4:
      center.requestAuthorization(options: [.alert, .sound]) { _, _ in
        let c = UNMutableNotificationContent()
        c.title = a[2]; c.body = a[3]; c.sound = .default
        if a.count >= 7 { c.userInfo = ["bin": a[4], "profile": a[5], "tab": a[6]] }
        self.center.add(UNNotificationRequest(identifier: a[1], content: c, trigger: nil)) { _ in self.quit(after: 1) }
      }
    case "clear" where a.count >= 2:
      center.removeDeliveredNotifications(withIdentifiers: [a[1]]); quit(after: 0.5)
    case "status":
      center.getNotificationSettings { s in
        switch s.authorizationStatus {
        case .authorized, .provisional: print("authorized")
        case .denied: print("denied")
        default: print("not asked")
        }
        exit(0)
      }
    default: quit(after: 3) // a click launched us: didReceive comes next
    }
  }

  func userNotificationCenter(_ c: UNUserNotificationCenter, didReceive r: UNNotificationResponse, withCompletionHandler done: @escaping () -> Void) {
    let info = r.notification.request.content.userInfo
    if let bin = info["bin"] as? String, let profile = info["profile"] as? String, let tab = info["tab"] as? String,
       URL(fileURLWithPath: bin).lastPathComponent.hasPrefix("chrome-relay") {
      let p = Process()
      p.executableURL = URL(fileURLWithPath: bin); p.arguments = ["attend", profile, tab]
      try? p.run(); p.waitUntilExit()
      NSRunningApplication.runningApplications(withBundleIdentifier: "com.google.Chrome").first?.activate()
    }
    done(); quit(after: 0.3)
  }

  func userNotificationCenter(_ c: UNUserNotificationCenter, willPresent n: UNNotification, withCompletionHandler done: @escaping (UNNotificationPresentationOptions) -> Void) {
    done([.banner, .sound])
  }
}

let app = NSApplication.shared
let notifier = Notifier()
app.delegate = notifier
app.setActivationPolicy(.accessory)
app.run()
