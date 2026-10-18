import Foundation
import Security
import Darwin

// Status only: no items, stdin, unlock, ACL operations or UI interaction.
// Both this default and a2a address the login file explicitly, never a search list.
// --fixture is used only by the repository's authorized platform sentinel.
let args = CommandLine.arguments
let path: String
if args.count == 1 {
    guard let entry = getpwuid(getuid()), let home = entry.pointee.pw_dir else {
        print("error"); exit(2)
    }
    path = String(cString: home) + "/Library/Keychains/login.keychain-db"
} else if args.count == 3 && args[1] == "--fixture" && args[2].hasPrefix("/") {
    path = args[2]
} else {
    print("error"); exit(2)
}
var keychain: SecKeychain?
guard SecKeychainOpen(path, &keychain) == errSecSuccess, let keychain else {
    print("error"); exit(2)
}
var status: SecKeychainStatus = 0
guard SecKeychainGetStatus(keychain, &status) == errSecSuccess else {
    print("error"); exit(2)
}
if status & UInt32(kSecUnlockStateStatus) != 0 {
    print("unlocked"); exit(0)
}
print("locked"); exit(3)
