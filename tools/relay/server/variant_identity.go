package server

import (
	"net/http"

	"github.com/mirror-db/mirror-db/tools/relay/hashstore"
	"github.com/mirror-db/mirror-db/tools/relay/internal/variant"
)

// acceptEncodingHeader is never a variant dimension for the relay. The coding
// is a property of the stored body, negotiated at delivery, so a relay request
// and a background refresh must not be filed under a second identity because
// the client's coding preference differs. This is relay policy, not a rule of
// the shared identity: the hash store still supports any Vary field.
const acceptEncodingHeader = "accept-encoding"

// relayResponseIdentity builds the identity the relay files a response under
// from the response's Vary field values and the request that produced it.
func relayResponseIdentity(noVary bool, varyValues []string, requestHeaders http.Header) variant.Identity {
	return variant.New(noVary, varyValues, requestHeaders).Excluding(acceptEncodingHeader)
}

// relayStoredIdentity rebuilds that same identity from a stored variant. It is
// the lookup half of relayResponseIdentity, so a request can never match under
// different normalisation than the one that wrote the entry.
func relayStoredIdentity(v hashstore.Variant) variant.Identity {
	return v.Identity().Excluding(acceptEncodingHeader)
}

// storedVariantIdentity is the identity as persisted, without relay policy
// applied. Cleanup compares these: the store may hold a variant whose Vary set
// names a coding, and leaving it behind would make every later lookup for the
// URL ambiguous even though the relay would never match it differently.
func storedVariantIdentity(v hashstore.Variant) variant.Identity {
	return v.Identity()
}
