#!/usr/bin/env bash
# mirrors-test.sh — end-to-end smoke tests for all mirrors.
# Runs on a Debian host with Docker installed.
set -euo pipefail

BASE="${BASE_DOMAIN:-mirs.uk}"

LOGFILE="result.log"
: > "$LOGFILE"

log() {
  echo "$1" >> "$LOGFILE"
  echo "---------------------------------"
  echo "$1"
  echo "---------------------------------"
}

pass() { log "✓ $1"; }
fail() { log "✗ $1"; FAILURES=$((FAILURES + 1)); }

FAILURES=0

pull_rmi() {
  if docker pull "$1" >/dev/null 2>&1 && docker rmi "$1" >/dev/null 2>&1; then
    pass "$1"
  else
    fail "$1"
  fi
}

http_ok() {
  local url="$1"
  local status
  status=$(curl -sSo /dev/null -w '%{http_code}' "$url")
  if [[ "$status" == 2* ]]; then
    pass "$url -> $status"
  else
    fail "$url -> $status"
  fi
}

# ── APT test framework ───────────────────────────────────────────────────────

APT_CONTAINER=""
APT_SRCDIR=""
APT_KEYDIR=""

aptest_init() {
  local image="$1"
  APT_SRCDIR=$(mktemp -d)
  APT_KEYDIR=$(mktemp -d)
  chmod 755 "$APT_SRCDIR" "$APT_KEYDIR"
  APT_CONTAINER="mdb-aptest-$$"
  docker run -d --rm --name "$APT_CONTAINER" \
    -v "$APT_SRCDIR:/etc/apt/sources.list.d:ro" \
    -v "$APT_KEYDIR:/etc/apt/keyrings:ro" \
    "$image" sleep infinity >/dev/null
}

aptest_key() {
  local name="$1"
  cat > "$APT_KEYDIR/$name.gpg"
  chmod 644 "$APT_KEYDIR/$name.gpg"
}

aptest_src() {
  local name="$1"
  cat > "$APT_SRCDIR/$name.sources"
}

aptest_pkg() {
  local pkg="$1"
  docker exec "$APT_CONTAINER" apt-get update -o Dir::Etc::sourcelist=/dev/null -qq
  if docker exec "$APT_CONTAINER" apt-get download "$pkg" 2>&1; then
    pass "$pkg"
  else
    fail "$pkg"
  fi
}

aptest_reset() {
  docker rm -f "$APT_CONTAINER" >/dev/null 2>&1
  rm -rf "$APT_SRCDIR" "$APT_KEYDIR"
  APT_CONTAINER=""
  APT_SRCDIR=""
  APT_KEYDIR=""
}

# ── Environment check ────────────────────────────────────────────────────────

check_cmd() { command -v "$1" &>/dev/null || { log "FATAL: $1 not found"; exit 1; }; }
check_cmd docker
check_cmd curl

log "Environment OK | BASE_DOMAIN=$BASE"

echo "── Preparing base images ──────────────────────────────────────────────────────"

docker pull "dcr.$BASE/debian:trixie" >/dev/null 2>&1
docker pull "dcr.$BASE/ubuntu:noble" >/dev/null 2>&1

docker image inspect mdb-test-debian >/dev/null 2>&1 || docker build -t mdb-test-debian - <<DOCKERFILE
FROM dcr.$BASE/debian:trixie
RUN cat >/etc/apt/sources.list.d/debian.sources <<EOF
Types: deb
URIs: http://$BASE/debian/
Suites: trixie
Components: main
Signed-By: /usr/share/keyrings/debian-archive-keyring.gpg

Types: deb
URIs: http://$BASE/debian-security/
Suites: trixie-security
Components: main
Signed-By: /usr/share/keyrings/debian-archive-keyring.gpg
EOF

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates apt-transport-https curl gpg \
    && rm -rf /var/lib/apt/lists/*
DOCKERFILE

docker image inspect mdb-test-ubuntu >/dev/null 2>&1 || docker build -t mdb-test-ubuntu - <<DOCKERFILE
FROM dcr.$BASE/ubuntu:noble
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates apt-transport-https curl gpg \
    && rm -rf /var/lib/apt/lists/*
DOCKERFILE

echo "── OCI mirrors ──────────────────────────────────────────────────────────────"

pull_rmi "dcr.$BASE/hello-world:latest"
pull_rmi "ghcr.$BASE/astral-sh/uv:latest"
pull_rmi "gcr.$BASE/distroless/static:latest"
pull_rmi "k8s.$BASE/pause:3.9"
pull_rmi "quay.$BASE/prometheus/busybox:latest"
pull_rmi "mcr.$BASE/mcr/hello-world:latest"
pull_rmi "nvcr.$BASE/nvidia/cuda:12.2.0-base-ubuntu22.04"
pull_rmi "ocr.$BASE/os/oraclelinux:8-slim"

echo "── npm mirror ───────────────────────────────────────────────────────────────"

docker pull node:22-slim >/dev/null 2>&1
if docker run --rm node:22-slim bash -c "
  npm config set registry https://$BASE/npm/
  npm info lodash version
  npm pack lodash@4.17.21
" 2>/dev/null; then pass "npm"; else fail "npm"; fi

echo "── PyPI mirror ──────────────────────────────────────────────────────────────"

docker pull python:3-slim >/dev/null 2>&1
if docker run --rm python:3-slim bash -c "
  pip install --index-url https://$BASE/pypi/simple/ --trusted-host $BASE six==1.16.0
"; then pass "pypi"; else fail "pypi"; fi

echo "── Debian + Debian Security mirror ─────────────────────────────────────────"

aptest_init mdb-test-debian

aptest_src debian <<EOF
Types: deb
URIs: https://$BASE/debian/
Suites: trixie
Components: main
Signed-By: /usr/share/keyrings/debian-archive-keyring.gpg

Types: deb
URIs: https://$BASE/debian-security/
Suites: trixie-security
Components: main
Signed-By: /usr/share/keyrings/debian-archive-keyring.gpg
EOF

aptest_pkg coreutils
aptest_pkg libssl3t64
aptest_reset

echo "── Ubuntu mirror ────────────────────────────────────────────────────────────"

aptest_init mdb-test-ubuntu

aptest_src ubuntu <<EOF
Types: deb
URIs: https://$BASE/ubuntu/
Suites: noble noble-updates
Components: main universe
Signed-By: /usr/share/keyrings/ubuntu-archive-keyring.gpg
EOF

aptest_pkg coreutils
aptest_reset

echo "── Kubernetes APT mirror ────────────────────────────────────────────────────"

aptest_init mdb-test-debian

aptest_key kubernetes < <(curl -fsSL "https://$BASE/kubernetes/core:/stable:/v1.34/deb/Release.key" | gpg --dearmor 2>/dev/null)
aptest_src kubernetes <<EOF
Types: deb
URIs: https://$BASE/kubernetes/core:/stable:/v1.34/deb/
Suites: /
Signed-By: /etc/apt/keyrings/kubernetes.gpg
EOF

aptest_pkg kubectl
aptest_reset

echo "── Docker CE APT mirror ─────────────────────────────────────────────────────"

aptest_init mdb-test-debian

aptest_key docker < <(curl -fsSL "https://$BASE/docker-ce/linux/debian/gpg" | gpg --dearmor 2>/dev/null)
aptest_src docker <<EOF
Types: deb
URIs: https://$BASE/docker-ce/linux/debian/
Suites: trixie
Components: stable
Signed-By: /etc/apt/keyrings/docker.gpg
EOF

aptest_pkg docker-ce-cli
aptest_reset

echo "── Mise APT mirror ──────────────────────────────────────────────────────────"

aptest_init mdb-test-debian

aptest_key mise < <(curl -fsSL "https://mise.jdx.dev/gpg-key.pub" | gpg --dearmor 2>/dev/null)
aptest_src mise <<EOF
Types: deb
URIs: https://$BASE/mise/
Suites: stable
Components: main
Signed-By: /etc/apt/keyrings/mise.gpg
EOF

aptest_pkg mise
aptest_reset

echo "── OpenTofu APT mirror ──────────────────────────────────────────────────────"

aptest_init mdb-test-debian

aptest_key opentofu < <(curl -fsSL "https://packages.opentofu.org/opentofu/tofu/gpgkey" | gpg --dearmor 2>/dev/null)
aptest_src opentofu <<EOF
Types: deb
URIs: https://$BASE/opentofu/
Suites: any
Components: main
Signed-By: /etc/apt/keyrings/opentofu.gpg
EOF

aptest_pkg tofu
aptest_reset

echo "── NVIDIA CUDA APT mirror ───────────────────────────────────────────────────"

aptest_init mdb-test-debian
aptest_key cuda < <(curl -fsSL "https://$BASE/cuda/debian13/x86_64/8793F200.pub" | gpg --dearmor 2>/dev/null)
aptest_src cuda <<EOF
Types: deb
URIs: https://$BASE/cuda/debian13/x86_64/
Suites: /
Signed-By: /etc/apt/keyrings/cuda.gpg
EOF
aptest_pkg cuda-toolkit-config-common
aptest_reset

echo "── Microsoft APT mirror ─────────────────────────────────────────────────────"

aptest_init mdb-test-debian

aptest_key microsoft < <(curl -fsSL "https://$BASE/microsoft/keys/microsoft.asc" | gpg --dearmor 2>/dev/null)
aptest_src microsoft <<EOF
Types: deb
URIs: https://$BASE/microsoft/repos/code/
Suites: stable
Components: main
Signed-By: /etc/apt/keyrings/microsoft.gpg
EOF

aptest_pkg code
aptest_reset

echo "── Hashicorp APT mirror ─────────────────────────────────────────────────────"

aptest_init mdb-test-debian

aptest_key hashicorp < <(curl -fsSL "https://$BASE/hashicorp/gpg" | gpg --dearmor 2>/dev/null)
aptest_src hashicorp <<EOF
Types: deb
URIs: https://$BASE/hashicorp/
Suites: trixie
Components: main
Signed-By: /etc/apt/keyrings/hashicorp.gpg
EOF

aptest_pkg terraform
aptest_reset

echo "── Syncthing APT mirror ─────────────────────────────────────────────────────"

aptest_init mdb-test-debian

aptest_key syncthing < <(curl -fsSL "https://syncthing.net/release-key.gpg")
aptest_src syncthing <<EOF
Types: deb
URIs: https://$BASE/syncthing-pkgs/
Suites: syncthing
Components: stable
Signed-By: /etc/apt/keyrings/syncthing.gpg
EOF

aptest_pkg syncthing
aptest_reset

echo "── libnvidia-container APT mirror ─────────────────────────────────────────"

aptest_init mdb-test-debian

aptest_key nvidia-container < <(curl -fsSL "https://$BASE/libnvidia-container/gpgkey" | gpg --dearmor 2>/dev/null)
aptest_src nvidia-container <<EOF
Types: deb
URIs: https://$BASE/libnvidia-container/stable/deb/amd64/
Suites: /
Signed-By: /etc/apt/keyrings/nvidia-container.gpg
EOF

aptest_pkg libnvidia-container-tools
aptest_reset

echo "── Debian Ports APT mirror ────────────────────────────────────────────────"

aptest_init mdb-test-debian

aptest_key debian-ports < <(curl -fsSL "https://www.ports.debian.org/archive_2026.key" | gpg --dearmor 2>/dev/null)
aptest_src debian-ports <<EOF
Types: deb
URIs: https://$BASE/debian-ports/
Suites: sid
Components: main
Architectures: alpha
Signed-By: /etc/apt/keyrings/debian-ports.gpg
EOF

if docker exec "$APT_CONTAINER" apt-get update -o Dir::Etc::sourcelist=/dev/null -qq; then
  pass "debian-ports"
else
  fail "debian-ports"
fi
aptest_reset

echo "── Ubuntu Ports APT mirror ────────────────────────────────────────────────"

aptest_init mdb-test-ubuntu

aptest_src ubuntu-ports <<EOF
Types: deb
URIs: https://$BASE/ubuntu-ports/
Suites: noble
Components: main universe
Architectures: arm64
Signed-By: /usr/share/keyrings/ubuntu-archive-keyring.gpg
EOF

if docker exec "$APT_CONTAINER" apt-get update -o Dir::Etc::sourcelist=/dev/null -qq; then
  pass "ubuntu-ports"
else
  fail "ubuntu-ports"
fi
aptest_reset

echo "── Go releases mirror ───────────────────────────────────────────────────────"

http_ok "https://$BASE/go-releases/go1.22.0.linux-amd64.tar.gz"

echo "── Node.js mirror ───────────────────────────────────────────────────────────"

http_ok "https://$BASE/nodejs/v22.0.0/SHASUMS256.txt"

echo "── Anaconda mirror ──────────────────────────────────────────────────────────"

http_ok "https://$BASE/anaconda/pkgs/main/channeldata.json"

echo "── gh-proxy mirror ──────────────────────────────────────────────────────────"

http_ok "https://$BASE/gh-proxy/https://github.com/cli/cli/releases/download/v2.62.0/gh_2.62.0_checksums.txt"

echo "── Kube releases mirror ─────────────────────────────────────────────────────"

http_ok "https://$BASE/kube-releases/release/stable.txt"

echo "── Debian CD mirror ─────────────────────────────────────────────────────────"

http_ok "https://$BASE/debian-cd/current/amd64/iso-cd/"

echo "── Ubuntu CD images mirror ──────────────────────────────────────────────────"

http_ok "https://$BASE/ubuntu-cdimages/noble/"

echo "── Ubuntu Cloud Images mirror ───────────────────────────────────────────────"

http_ok "https://$BASE/ubuntu-cloud-images/noble/"

echo "── Ubuntu Releases mirror ───────────────────────────────────────────────────"

http_ok "https://$BASE/ubuntu-releases/noble/"

if [[ $FAILURES -eq 0 ]]; then
  log "All tests passed."
else
  log "$FAILURES test(s) failed."
  exit 1
fi
