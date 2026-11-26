// Package activecache manages one in-process tiered cache per immutable object.
//
// A Manager maps an opaque key to an Entry. The first Acquire for a key creates
// the Entry and becomes its creator; every later Acquire reuses that Entry until
// it is published into the hash store or fails. A creator learns nothing about
// the upstream when it acquires: it must later call Init with the range source,
// the shared metadata and the declared size, or Fail to wake waiters and let
// them fall back to the upstream.
//
// While readers consume an Entry, tiered.PassiveCache fetches only the missing
// ranges into a scratch file. Once the whole object is cached, a custom
// tiered.Store publishes it through hashstore.Adopt, which validates the digest
// and makes the body visible to every other request. Published Entries leave the
// active map immediately; existing readers keep reading the local file
// descriptor until they release, then the manager closes every adapter.
//
// The manager never blocks a request on capacity: MaxEntries, capacity and the
// disk watermark are admission checks, and a rejected Acquire or Init tells the
// caller to take the ordinary upstream path. An entry that was already admitted
// is never truncated or failed because capacity ran out mid-write; only new
// entries are refused.
package activecache
