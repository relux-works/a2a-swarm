import ApplicationServices
import CoreGraphics
import Foundation
import Darwin

// Non-focus read-only observation. Never request permissions or read window text.
guard AXIsProcessTrusted() else { print("observer=unavailable accessibility_untrusted"); exit(2) }
var baseline: Set<Int>?
func snapshot() -> Bool {
    guard let windows = CGWindowListCopyWindowInfo([.optionOnScreenOnly, .excludeDesktopElements], kCGNullWindowID) as? [[String:Any]] else {
        print("observer=unavailable window_list"); return false
    }
    var ids = Set<Int>(), pids = Set<Int32>()
    for window in windows where (window[kCGWindowOwnerName as String] as? String) == "SecurityAgent" {
        guard let pid = window[kCGWindowOwnerPID as String] as? Int32,
              let id = window[kCGWindowNumber as String] as? Int else {
            print("observer=unavailable window_identity"); return false
        }
        ids.insert(id); pids.insert(pid)
    }
    if baseline == nil { baseline = ids }
    var axCount = 0, old12571 = 0
    for pid in pids {
        var value: CFTypeRef?
        let result = AXUIElementCopyAttributeValue(AXUIElementCreateApplication(pid), kAXWindowsAttribute as CFString, &value)
        guard result == .success, let axWindows = value as? [AXUIElement] else {
            print("observer=unavailable ax_windows"); return false
        }
        axCount += axWindows.count
        if pid == 12571 { old12571 = axWindows.count }
    }
    print("at=\(Date().timeIntervalSince1970) baseline_window_count=\(baseline!.count) onscreen_window_count=\(ids.count) new_window_count=\(ids.subtracting(baseline!).count) ax_window_count=\(axCount) old12571_ax_windows=\(old12571)")
    fflush(stdout)
    return true
}
guard snapshot() else { exit(2) }
print("observer=ready nonfocus=true scope=onscreen_SecurityAgent_ids_plus_AX_counts titles_read=false"); fflush(stdout)
if CommandLine.arguments.contains("--preflight") { exit(0) }
// The harness closes/sends one byte on this private pipe to stop and reap us.
// Keep a hard lifetime as a second bound; never interact with another process.
let controlled = CommandLine.arguments.contains("--controlled")
let until = Date().addingTimeInterval(120)
while Date() < until {
    if controlled {
        var input = pollfd(fd: STDIN_FILENO, events: Int16(POLLIN | POLLHUP), revents: 0)
        let result = poll(&input, 1, 100)
        if result < 0 { print("observer=unavailable control_pipe"); exit(2) }
        if result > 0 { exit(0) }
    }
    if !snapshot() { exit(2) }
    if !controlled { Thread.sleep(forTimeInterval: 0.1) }
}
print("observer=unavailable lifetime_expired"); exit(2)
