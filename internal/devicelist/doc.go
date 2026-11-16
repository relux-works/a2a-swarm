// Package devicelist is the LIP-0029 device-list check for Matrix devices
// (swarma-link-spec v0.1.51): the device list codec and h(D) of section 3.2,
// a bounded cache keyed by h(D) (O3.11 to O3.14), the admission of section 11
// as OM7 changes it for a messenger event, and the ChainView that steps 4 and
// 8 read, with one adapter: the root snapshot file of the chain gateway
// contract at d1cab6f8 (part (B)), including the four seal states and
// parser-validated confirmations/script bytes (not exposed by Snapshot, and
// no seal-script validation is claimed).
//
// Darwin does not check ACLs: its filesystem profile is weaker than the
// contract and requires separate native qualification before installation.
// Linux rejects POSIX ACL xattrs, but ENOTSUP is accepted: a filesystem without
// the POSIX ACL xattr namespace is not checked for ACLs.
//
// The package is OBSERVE ONLY. It executes no effect and performs no network
// access. Admit is a deterministic function of its arguments: it reads no
// clock (the caller passes its check time), no file and no socket, and the
// only state it changes is the ObserveJournal it is handed. Every verdict is
// an observation and never a source for enforcement, for three reasons that
// each hold alone: a snapshot cannot see a seal spend that happens after it
// was written (Result.Live is false); its collection is only bracketed; and
// the journal that holds the closures is observe-grade, in memory and inside
// the consumer process (Result.ObserveOnly is true on every result).
//
// LIP-0029 and LIP-0010 section 11 are Drafts whose co-author agreement is
// pending. This package follows the released bytes of the tag and is a
// library: nothing in the delivery path calls it yet.
package devicelist
