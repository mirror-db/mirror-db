# TODO

Follow-ups from the 2026-11-03 full review. Each item states how it was observed,
so it can be re-checked without redoing the investigation.

## 1. `gcr.io` — reachability from the CN egress (keep the mirror)

Observed: three probes of `https://gcr.io/v2/` timed out from the default
network, while `ghcr.io`, `quay.io`, `mcr.microsoft.com`, `nvcr.io`,
`container-registry.oracle.com` and `lscr.io` all answered.

Re-checked through `https_proxy=http://10.0.0.5:3128`: `gcr.io`, `eu.gcr.io`,
`us.gcr.io` and `mirror.gcr.io` all answer `401` with a well-formed
`Bearer realm="https://gcr.io/v2/token"` — the registry is alive. So this is a
routing/DPI problem on the path, not a shutdown. **Mirror kept.**

Open question: does `dcr/ghcr/k8s/...`-style proxying of `gcr.io` work from a
real CN client, or only from the relay? Needs one test from a domestic host
(`docker pull gcr.mirs.uk/...`).

## 2. ~~`apt.syncthing.dev` no longer resolves~~ — resolved

`apt.syncthing.dev` has no DNS record; the archive lives at
`https://apt.syncthing.net/` with suite `syncthing` (components `stable`,
`stable-v2`, `candidate`, `nightly`). The mirror now points there; `dists/` has
no listing upstream, so the suite list stays explicit and directory browsing on
this mount answers 404 by design.

## 2b. Syncthing's second signing certificate

The archive is dual-signed but only one certificate is published. Our key mirror
serves both, so the mount works; the underlying upstream inconsistency is
theirs. Recheck when they drop the 2014 signature (then the extra fetch can go).

## 3. `packages.opentofu.org` is not an APT tree

`https://packages.opentofu.org/opentofu/tofu/any/` is a packagecloud HTML UI;
`dists/` is `404` at every layout tried (`any/`, `any/any/`, `any/apt/`,
`any/deb/`). The documented source line is
`deb https://packages.opentofu.org/opentofu/tofu/` — i.e. `dists/` sits at the
repo root, but that returns `404` too. The installer script
`/install/repositories/opentofu/tofu/script.deb.sh` is the authority; read it and
derive the real layout, or drop the mirror.

## 4. `packages.k8s.io` prefix returns 403

`https://prod-cdn.packages.k8s.io/repositories/isv:/kubernetes:/…` answers `403`
without a token, so the `/kubernetes/` passthrough mirror only works for the
paths whose tokens clients send. Confirm which paths actually work; if none, drop
it (`/kube-releases/` on `dl.k8s.io` already covers the release binaries).

## 5. `/pypi/simple/` transfers 48 MB on a miss

Upstream serves 48,031,464 B identity and 7,663,752 B gzip. The mirror requests
identity, so a cold entry costs 48 MB. With the new 10-minute revalidation this
happens rarely; if it becomes a cost problem, the fix is to request `gzip` for
this one document and store the compressed variant with its own cache key —
do **not** store a re-encoded body under the identity key.

## 6. `cuda` base is a moving target

`developer.download.nvidia.com/compute/cuda/repos/` lists per-distro dirs
(`debian12/`, `ubuntu2204/`, …) — the repo layout behind them changes with each
CUDA release, and the old ones stay. Nothing to do now; revisit if a stale
`/cuda/…` path starts advertising removed packages.

## 7. `wrangler dev` leaves `pkg/sumdb/` in the repo root

Go module sumdb cache created by `go test`/`go build` when `GOFLAGS`/`GOPATH`
resolve into the repo. Add `/pkg` to `.gitignore` if it keeps reappearing (it
was deleted manually this round).
