export const MdbUserAgent = "mirror-db/0.0.0-dev";

// https://developers.cloudflare.com/cache/concepts/default-cache-behavior/#cacheable-size-limits
export const CFCacheLimit = 1024 * 1024 * 512; // 512MB

// If Request Content-Length is less than this, it will be cached immediately
export const MdbCacheSizeLimit = CFCacheLimit;
// If a Range Request is made, and the Response Content-Length is less than this
// mdb will make a new request without range and then cache the response
export const MdbActiveCacheThreshold = 1024 * 1024 * 1; // 1MB

// The name of the cache to use for upstream requests
export const MdbCacheName = "upstream";

// The default limit for concurrent requests to the upstream server
export const MdbMaxConcurrentRequests = 64;

// How often a cached entry past its `max-age` is re-checked against its source.
// The check is a conditional HEAD (ETag / Last-Modified), so an unchanged object
// costs one round-trip and no body. Refresh this with the file's own max-age:
// the first check happens exactly one max-age after the store.
export const MdbRevalidateIntervalMs = 10 * 60 * 1000; // 10 minutes
