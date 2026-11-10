package daemonsvc

import "encoding/binary"

// Darwin ATTR_CMN_EXTENDED_SECURITY is an attrreference to kauth_filesec;
// getattrlist(2) specifies only its ACL entry is used, not its owner/GUIDs.
// Bounds, unknown ACE kinds and write-capable permits fail closed. Denies and
// read/search-only permits remain valid, including the usual deny-delete ACL.
func darwinACLNoWrite(buf []byte) bool {
	if len(buf) < 12 {
		return false
	}
	order := binary.NativeEndian
	total := uint64(order.Uint32(buf[:4]))
	start := int64(4) + int64(int32(order.Uint32(buf[4:8])))
	size := uint64(order.Uint32(buf[8:12]))
	if total < 12 || total > uint64(len(buf)) {
		return false
	}
	if size == 0 {
		return true
	}
	if start < 12 || size < 44 || uint64(start)+size > total {
		return false
	}
	sec := buf[start : uint64(start)+size]
	count := order.Uint32(sec[36:40])
	if count == ^uint32(0) {
		return true
	} // KAUTH_FILESEC_NOACL
	if count > 128 || uint64(44)+uint64(count)*24 > size {
		return false
	}
	const writeRights = 1<<2 | 1<<4 | 1<<5 | 1<<6 | 1<<8 | 1<<10 | 1<<12 | 1<<13 | 1<<21 | 1<<23
	for i := uint32(0); i < count; i++ {
		ace := sec[44+i*24 : 44+(i+1)*24]
		kind := order.Uint32(ace[16:20]) & 15
		if kind != 1 && kind != 2 {
			return false
		}
		if kind == 1 && order.Uint32(ace[20:24])&writeRights != 0 {
			return false
		}
	}
	return true
}
