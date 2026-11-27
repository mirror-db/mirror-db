# Relay 静态 immutable 覆盖调研

调研日期 2026-11-27。对象是 Worker 下发的 manifest policy 中
`rules.immutable`（见 [relay-policy.md](relay-policy.md)、
[ADR 0015](../../docs/adr/0015-immutable-policy-rule.md)），不是 relay 的响应侧
`X-MDB-Relay: immutable`。本文记录 40 个 mirror 的逐项覆盖、证据、外部路径与
mount-relative 路径的对应关系，以及明确不声明的情况。

## 结论先行

- manifest 中的 immutable 只有两个合法来源：
  1. **内容寻址**：路径本身带内容摘要（OCI `blobs/`/`manifests/`、
     APT `by-hash/SHA256|SHA1/<hex>`）；
  2. **发布契约**：上游按路径承诺不再改写字节（版本目录、PyPI 文件名、
     npm tarball、日期化镜像构建）。
- 响应头不能成为 policy 来源。`ETag`、`Docker-Content-Digest`、
  `Content-Digest`、`Cache-Control` 都只在响应到达后可见，且校验器不等于
  “该 URL 的字节不可变”。本调研记录它们，但不据此新增规则。
- 所有 `match.path_pattern` 与 `rules.immutable` 都匹配 **mount-relative
  escaped path**：Worker/relay 已去掉 `/npm`、`/pypi`、`/debian` 等 mount
  前缀。外部 URL 不能直接抄进 RE2。
- 当前共 14 条 policy：11 条声明 `immutable`（2 条内容寻址、9 条发布契约），
  3 条只做 transport。所有 immutable 规则排在 transport 规则之前，后续
  `large-artifacts` 等规则仍可按字段合并追加 `speed`。

## 分类记号

| 记号 | 含义 | 是否进 policy |
|------|------|----------------|
| A | 路径携带内容摘要，可直接判定不可变 | 是，`immutable` + 可选的 `sha256`/`sha1` 命名捕获 |
| B | 路径由发布契约固定，不携带摘要 | 是，只声明 `immutable`，不捕获 digest |
| C | 响应头带内容摘要形态（如 `Docker-Content-Digest`） | 否，仅记录；响应侧事实 |
| D | 响应头只是校验器（MD5 ETag、multipart ETag、inode/mtime ETag） | 否，仅记录；不证明 URL 不可变 |
| M | 已知可变或移动别名（tag、index、`latest`、`current`） | 否 |
| U | 未证明、证据不足或有意不声明（可能事实上不可变） | 否，保留普通缓存路径 |

“M”与“U”都不是“已证明会变”，区别只在是否已观察到移动语义。B 类也不代表
永久承诺，只代表当前上游发布契约；契约变化要重新调研。

## 当前 policy 清单（14）

顺序即 manifest 求值顺序。前 11 条只声明 `immutable`，后 3 条只声明
transport。

| # | name | match | rules.immutable |
|---|------|-------|-----------------|
| 1 | `oci-digests` | `tag: oci` + 下表 RE2 | 同 match |
| 2 | `apt-by-hash` | `tag: apt` + 下表 RE2 | 同 match |
| 3 | `pypi-packages` | `mirror: pypi` + 下表 RE2 | 同 match |
| 4 | `npm-tarballs` | `mirror: npm` + 下表 RE2 | 同 match |
| 5 | `nodejs-releases` | `mirror: nodejs` + 下表 RE2 | 同 match |
| 6 | `go-releases` | `mirror: go-releases` + 下表 RE2 | 同 match |
| 7 | `kube-releases` | `mirror: kube-releases` + 下表 RE2 | 同 match |
| 8 | `debian-cd` | `mirror: debian-cd` + 下表 RE2 | 同 match |
| 9 | `debian-cloud` | `mirror: debian-cloud` + 下表 RE2 | 同 match |
| 10 | `ubuntu-cloud` | `mirror: ubuntu-cloud` + 下表 RE2 | 同 match |
| 11 | `ubuntu-releases` | `mirror: ubuntu-releases` + 下表 RE2 | 同 match |
| 12 | `oci-blobs` | `tag: oci`, `^/v2/.+/blobs/` | —（`speed`） |
| 13 | `apt-packages` | `tag: apt`, `path_prefix: /pool/` | —（`speed`） |
| 14 | `large-artifacts` | 通用后缀 RE2 | —（`speed`） |

### 精确 RE2

```text
oci-digests:
^/v2/.+/(?:blobs|manifests)/sha256:(?<sha256>[0-9a-f]{64})$

apt-by-hash:
^/(?:.+/)?dists/.+/by-hash/(?:SHA256/(?<sha256>[0-9a-f]{64})|SHA1/(?<sha1>[0-9a-f]{40}))$

pypi-packages:
^/packages/

npm-tarballs:
^/(?:@[^/]+/)?[^/]+/-/[^/]+\.tgz$

nodejs-releases:
^/v\d+\.\d+\.\d+/

go-releases:
^/go\d+(?:\.\d+)+(?:beta\d*|rc\d*)?(?:\.(?:src\.tar\.gz|(?:linux|darwin|windows|freebsd|netbsd|openbsd|dragonfly|solaris|illumos|aix|plan9)-[a-z0-9]+(?:-osx[0-9.]+)?\.(?:tar\.gz|zip|pkg|msi|tar\.xz))|-bootstrap-\d{8}\.tar\.gz)$

kube-releases:
^/release/v\d+\.\d+\.\d+/

debian-cd:
^/\d+(?:\.\d+)+(?:-live)?/

debian-cloud:
^/[a-z]+/\d{8}-\d{4}/

ubuntu-cloud:
^/releases/[a-z]+/release-\d{8}(?:\.\d+)?/

ubuntu-releases:
^/\d+\.\d+\.\d+(?:\.\d+)*/
```

全部表达式只使用 RE2 支持的结构：非捕获组、命名捕获、字符类与量词，没有
反向引用或 lookaround。

### 外部路径与 mount-relative 示例

| mirror | 客户端外部路径 | 上游真实 URL | policy 看到的路径 |
|--------|----------------|--------------|-------------------|
| pypi | `https://mirs.uk/pypi/packages/ab/cd/…/pkg-1.0.whl` | `https://files.pythonhosted.org/packages/ab/cd/…/pkg-1.0.whl` | `/packages/ab/cd/…/pkg-1.0.whl` |
| npm | `https://mirs.uk/npm/lodash/-/lodash-4.17.21.tgz` | `https://registry.npmjs.org/lodash/-/lodash-4.17.21.tgz` | `/lodash/-/lodash-4.17.21.tgz` |
| nodejs | `https://mirs.uk/nodejs/v22.12.0/node-v22.12.0-linux-x64.tar.xz` | `https://nodejs.org/dist/v22.12.0/…` | `/v22.12.0/node-v22.12.0-linux-x64.tar.xz` |
| go-releases | `https://mirs.uk/go-releases/go1.24.0.linux-amd64.tar.gz` | `https://go.dev/dl/go1.24.0.linux-amd64.tar.gz` | `/go1.24.0.linux-amd64.tar.gz` |
| kube-releases | `https://mirs.uk/kube-releases/release/v1.32.0/bin/linux/amd64/kubectl` | `https://dl.k8s.io/release/v1.32.0/bin/linux/amd64/kubectl` | `/release/v1.32.0/bin/linux/amd64/kubectl` |
| debian-cd | `https://mirs.uk/debian-cd/12.9.0/amd64/iso-cd/…iso` | `https://cdimage.debian.org/debian-cd/12.9.0/…` | `/12.9.0/amd64/iso-cd/…iso` |
| debian-cloud | `https://mirs.uk/debian-cloud/bookworm/20250519-1410/…qcow2` | `https://cloud.debian.org/images/cloud/bookworm/20250519-1410/…` | `/bookworm/20250519-1410/…qcow2` |
| ubuntu-cloud | `https://mirs.uk/ubuntu-cloud/releases/noble/release-20240523.1/…img` | `https://cloud-images.ubuntu.com/releases/noble/release-20240523.1/…` | `/releases/noble/release-20240523.1/…img` |
| ubuntu-releases | `https://mirs.uk/ubuntu-releases/24.04.5/…iso` | `https://releases.ubuntu.com/24.04.5/…iso` | `/24.04.5/…iso` |
| debian | `https://mirs.uk/debian/dists/bookworm/main/binary-amd64/by-hash/SHA256/<64>` | `http://ftp.us.debian.org/debian/dists/…` | `/dists/bookworm/main/binary-amd64/by-hash/SHA256/<64>` |
| microsoft | `https://mirs.uk/microsoft/code/dists/bookworm/…/by-hash/SHA256/<64>` | `https://packages.microsoft.com/repos/code/dists/…` | `/code/dists/bookworm/…/by-hash/SHA256/<64>` |
| pigsty（路径形状示意，实际无 by-hash） | `https://mirs.uk/pigsty/pgsql/bookworm/dists/bookworm/…/by-hash/SHA256/<64>` | `https://repo.pigsty.io/apt/pgsql/bookworm/dists/…` | `/pgsql/bookworm/dists/bookworm/…/by-hash/SHA256/<64>` |

`microsoft` 的 upstream 根是 `repos/`，所以 `code/` 属于 mount-relative
path；旧的 `^/dists/…` 只覆盖根目录就是仓库根的 APT mount。新的
`^/(?:.+/)?dists/…` 同时覆盖两类，不改变普通 path 的匹配。

## 40 个 mirror 逐项分类

### 1. OCI registry（9）

`dcr` 走 Docker Hub 专用代理，其余 8 个是 `createRegistryHost` 的匿名
passthrough；全部带 `oci` tag。

| mirror | 路径 | 分类 | 说明 |
|--------|------|------|------|
| dcr, ghcr, gcr, k8s, quay, mcr, nvcr, ocr, lscr | `/v2/<repo>/blobs/sha256:<64>` | A | `oci-digests`；digest 即内容地址 |
| 同上 | `/v2/<repo>/manifests/sha256:<64>` | A | 同上；摘要 manifest 不会变 |
| 同上 | `/v2/<repo>/manifests/<tag>` | M | tag 可重新指向；即使响应带 `Docker-Content-Digest` 也不声明 |
| 同上 | `/v2/<repo>/referrers/sha256:<64>` | M | 这是按 digest 生成的索引（referrers list），不是被指向对象本身 |
| 同上 | `/v2/<repo>/blobs/uploads/…`、`/v2/_catalog`、`/v2/<repo>/tags/list` | M | 写路径或列表 |
| gcr | `/artifacts-downloads/…`（由 blob 的相对 redirect 产生） | U | Worker 只原样代理该路径；没有可判定的路径契约，也不从 redirect/响应头推导 |

Docker Hub 的 `dcr` 有 tag 解析和 `library/` 补全，但 digest 路径形态与其他
registry 相同，因此共用 `oci-digests`。`referrers` 在 Docker Hub 的
`normalizeDockerPath` 中被识别为独立操作，目录仍可能变化。

### 2. APT（11）

11 个 mount 全部带 `apt` tag：`debian`、`debian-ports`、`debian-security`、
`ubuntu`、`ubuntu-ports`、`mise`、`syncthing-pkgs`、`hashicorp`、
`opentofu`、`github-cli`、`gitlab-runner-deb`。其中 `mise`、
`syncthing-pkgs`、`hashicorp`、`opentofu`、`github-cli` 是
`snapshot: true`，会发布 `.mdb/apt-index.json`；`gitlab-runner-deb` 走普通
APT 代理。

| 路径 | 分类 | 说明 |
|------|------|------|
| `…/by-hash/SHA256/<64>`、`…/by-hash/SHA1/<40>` | A | `apt-by-hash`；命名捕获供 relay 校验 |
| `…/by-hash/SHA512/<128>`、`MD5Sum/<32>` 等未命名算法 | U | 不匹配捕获规则；relay 可自行判 `immutable` 不捕获，但 Worker 不声明 |
| `dists/…/Release`、`InRelease`、`Packages*`、`Sources*` | M | 可随发布更新；APT proxy 另外注入 `no-store` |
| `pool/…` | U | 版本化路径通常只增不改，但第三方 pool 可能重传；证据不足，不静态声明 |
| `snapshot` mount 的 `/.mdb/apt-index.json` | M | Worker 生成的控制索引，随 generation 重建 |
| `snapshot` mount 的 `dists/…` | M | 同普通 APT index |
| `snapshot` mount 的 `pool/` | U | 由 metadata generation 覆盖的包文件可由 relay 做 hash prediction，但那是响应/索引事实，不是静态 policy |

嵌套布局：

| mirror | mount-relative 仓库根 | by-hash 示例 |
|--------|----------------------|--------------|
| debian / ubuntu / 其他根即仓库的 mount | `/dists/…` | `/dists/bookworm/main/binary-amd64/by-hash/SHA256/<64>` |
| microsoft（web-list，apt tag） | `/code/dists/…` 等仓库子目录 | `/code/dists/bookworm/main/binary-amd64/by-hash/SHA256/<64>` |
| pigsty（passthrough，apt tag，仅示意形状） | `/infra/dists/…`、`/pgsql/<codename>/dists/…` | `/pgsql/bookworm/dists/bookworm/main/binary-amd64/by-hash/SHA256/<64>` |

`apt` tag 只是选择策略，不代表上游一定发布 by-hash：`microsoft` 有可用的
嵌套 `by-hash` 目录，是本次扩展的真实用例；`pigsty` 没有任何 by-hash 目录且
所有对象 `Cache-Control: no-store`；`cuda` 返回 `Acquire-By-Hash: no`，
`docker-ce` 的 by-hash 请求 404，`kubernetes` 与 `libnvidia-container` 未声明。
这些 mirror 上规则不会产生实际命中，不是“已确认存在但未验证”的 A。

### 3. web-list（8）

`cuda`、`debian-cd`、`debian-cloud`、`docker-ce`、`ubuntu-cd`、
`ubuntu-cloud`、`ubuntu-releases`、`microsoft`。其中 `cuda`、`docker-ce`、
`microsoft` 带 `apt` tag。

| mirror | 路径 | 分类 | 说明 |
|--------|------|------|------|
| cuda | `…/by-hash/…` | U | 实测 `Acquire-By-Hash: no`，上游未发布可用 by-hash；apt tag 规则不会命中 |
| cuda | `dists/…`、`pool/…`、`…/<distro>/<arch>/…deb` | M/U | 没有独立静态规则；pool/包路径证据不足 |
| docker-ce | `linux/…/dists/…/by-hash/…` | U | 实测 by-hash 请求 404；上游未发布可用目录 |
| docker-ce | `dists/…`、`pool/…` | M/U | 不新增静态规则 |
| ubuntu-cd | 整站 | U | 未做完整探测；不声明 |
| microsoft | `code/dists/…/by-hash/…` | A | 本次嵌套扩展的直接用例 |
| microsoft | `code/dists/…`、`code/pool/…` | M/U | 不新增静态规则 |
| debian-cd | `/12.9.0/…`、`/12.9.0-live/…` | B | `debian-cd` |
| debian-cd | `/current/`、`/project/` | M | 移动别名 |
| debian-cloud | `/bookworm/20250519-1410/…` | B | `debian-cloud`；日期 + build 号 |
| debian-cloud | `/bookworm/latest/…`、`/bookworm/daily/…` | M | 移动别名/每日构建 |
| ubuntu-cloud | `/releases/noble/release-20240523/…` | B | `ubuntu-cloud` |
| ubuntu-cloud | `/releases/noble/release-20240523.1/…` | B | 上游会为重发的日期构建加 `.N`；实测存在，规则覆盖 |
| ubuntu-cloud | `/releases/noble/latest/…`、`daily-*` | M | 移动别名/每日构建 |
| ubuntu-releases | `/24.04.5/…`、`/24.04.5.1/…` | B | `ubuntu-releases`；至少三段数字 |
| ubuntu-releases | `/jammy/`、`/bionic/` 等 codename 别名 | M | 不匹配 |
| ubuntu-releases | `/24.04/`、`/26.04/` 等两段数字目录 | M | 有意排除的移动别名：上游把它指向当前 point release，不是未知项，也不声明 immutable |

### 4. passthrough（8）

`anaconda`、`gh-proxy`、`go-releases`、`kube-releases`、`kubernetes`、
`libnvidia-container`、`nodejs`、`pigsty`。其中 `kubernetes`、
`libnvidia-container`、`pigsty` 带 `apt` tag。

| mirror | 路径 | 分类 | 说明 |
|--------|------|------|------|
| anaconda | `pkgs/main/…`、`archive/…` 包文件 | U | repodata 索引可变；包文件未证明不重传 |
| anaconda | `pkgs/*/repodata.json*` | M | 每次 solve 都会更新 |
| gh-proxy | `/…` 任意 GitHub URL 代理 | U | 目标 release asset 可被 owner 替换；路径不含内容地址 |
| go-releases | `/go<version>.src.tar.gz`、`/go<version>.<os>-<arch>.<ext>`、`/go<version>-bootstrap-<YYYYMMDD>.tar.gz` | B | `go-releases`；实测 9022 个根文件全部命中 |
| kube-releases | `/release/v1.32.0/…` | B | `kube-releases` |
| kube-releases | `/release/stable.txt`、`stable-1.32.txt`、`latest-1.32.txt` | M | 移动指针 |
| nodejs | `/v22.12.0/…` | B | `nodejs-releases` |
| nodejs | `/latest-v22.x/`、`/latest/` | M | 移动别名 |
| kubernetes | `…/dists/…/by-hash/…` | U | 上游未声明 by-hash；apt tag 规则不会命中 |
| kubernetes | `pool/…`、`dists/…` | U/M | 不新增静态规则 |
| libnvidia-container | `…/dists/…/by-hash/…` | U | 上游未声明可用 by-hash；apt tag 规则不会命中 |
| libnvidia-container | `pool/…`、`dists/…` | U/M | 证据不足 |
| pigsty | `infra/dists/…`、`pgsql/…/dists/…` 的 by-hash | U | 上游实际没有 by-hash 目录，全站 `no-store`；规则不会命中 |
| pigsty | 其他包/index 路径 | U/M | 不新增静态规则 |

### 5. 特殊 mirror（4）

| mirror | 路径 | 分类 | 说明 |
|--------|------|------|------|
| npm | `/<pkg>/-/<pkg>-<ver>.tgz`、`/@scope/<pkg>/-/…tgz` | B | `npm-tarballs`；tarball 内容地址由 registry 固定 |
| npm | `/<pkg>`、`/@scope/<pkg>` 元数据、`/-/v1/search` | M | 新版本会重写元数据 |
| npm | unpublish 后的同版本 tarball | U | registry 允许受限 unpublish；见“未知与风险” |
| pypi | `/packages/…` | B | `pypi-packages`；上游禁止同文件名重传 |
| pypi | `/simple/`、`/simple/<pkg>/`、`/pypi/…` JSON | M | 索引与 API 会更新（root index 另外带 `prefetch`） |
| charts | `<repo>/index.yaml`、`<repo>/generated`、repo metadata | M | 每次发布重建 |
| charts | `<repo>/res/<uuid>/…` | U | `<uuid>` 是绝对 URL 的 uuidv5，不是内容 hash；同一 URL 的 tarball 可被上游替换 |
| debian-snapshot | `/archive/<archive>/<timestamp ≤ now>/…` | 动态 A | 不在 manifest 静态声明；由 Worker 在稳定分支的响应上加 `X-MDB-Relay: immutable` |
| debian-snapshot | 未来 timestamp、`/mr/…`、`404` | M/U | 未来解析到最新快照，仍在移动；`mr` 是 JSON API |

`debian-snapshot` 刻意不进静态 policy：同一路径前缀下的 timestamp 可能在未来，
未来 timestamp 被解析成“当前最新快照”，字节会继续移动；只有 `timestamp ≤ now`
的稳定分支才是不可变，这个判断需要请求时的当前时间，只能由 Worker 响应声明。

### 6. 非 mirror 端点

`/keys`、`/conf`、`/scripts`、`/api`、`/speedtest`、`/relaytest`、SPA 路由
不解析到 mirror，因此 policy 不参与。`/api/mirrors` 自己的 sha256 ETag 是
manifest 内容的校验器（D），不是任何 URL 的 immutable 声明。

## 响应头调研（只记录，不转化）

| 来源 | 观察 | 分类 | 结论 |
|------|------|------|------|
| OCI | `Docker-Content-Digest: sha256:<64>`；部分 registry 的 ETag 也是 `"sha256:<64>"` | C | 摘要描述响应体；tag 仍可重新指向，不能把 tag 路径变 immutable |
| npm tarball | 实测 `ETag: "25247d3dd7029d08a6ac99adab09086b"`（32 hex，MD5 形态） | D | 只是校验器；不证明 URL 不重传 |
| PyPI | ETag 随页面/文件变化，常见 MD5 形态或非摘要形态 | D | 同上 |
| nodejs | 实测 `ETag: "6b4a8df45195924e4d0021577cce823f"`（32 hex，疑似 MD5） | D | 同上 |
| Anaconda | 大文件/分段对象使用 multipart ETag（`"<hex>-<n>"`） | D | multipart 校验器，不是单一内容摘要 |
| Worker manifest API | `/api/mirrors` 的 ETag 是响应 JSON 的 sha256 | D | 即使形态是 sha256，也只校验这份可变文档 |
| APT / debian-snapshot | 常见 inode/mtime/size 派生 ETag | D | 不包含内容摘要 |
| 其他 | 未观察到 `Content-Digest`、`Repr-Digest`、merkle-tree 形态 | U | 不据此设计 |

即使某个 ETag 恰好等于 body 的 SHA-256，它仍然只是**响应对某次表示**的
校验器：tag→digest 的解析、CDN 重写、同 URL 重传都可以保持 URL 不变而改变
校验器。immutable 是“这个 URL 的字节不再改变”的请求前承诺，必须由路径或
发布契约给出。

## body 内 hash（只记录，不自动转化）

| 来源 | 位置 | 用途 | 不转化的原因 |
|------|------|------|--------------|
| APT | `Release`/`InRelease` 的 `SHA256:`/`SHA1:`/`MD5Sum:` 段 | 校验 `Packages`/`Sources` 和 pool 文件 | relay 解析 index 是运行期行为；Worker policy 只做请求前路径判断 |
| Go / k8s | 邻接的 `.sha256` 文件、`SHA256SUMS` | 客户端校验下载 | 需要先请求另一个 URL；且 checksum 文件本身不是 body 内容地址 |
| npm | 元数据 `dist.integrity`（sha512）与 `dist.shasum`（sha1） | 客户端校验 tarball | 元数据可变；integrity 是 body 摘要，不把 tarball URL 变成 digest 路径 |
| PyPI | Simple 页面的 `#sha256=…` fragment、`hashes` 字段 | 客户端校验文件 | fragment 不参与 HTTP 路径；文件名路径仍由发布契约保证 |
| Helm | `index.yaml` 的 `digest` | 客户端校验 chart tarball | 索引可重建；`res/<uuid>` 是 URL uuid，不是内容地址 |

这些值证明**完整性或校验关系**，不证明 URL 的字节不可变。没有一项被转换成
`rules.immutable`。

## 未知与风险

1. **ubuntu-cd 未完整探测。** 其目录语义（版本目录、`current/` 别名、点发布
   重发）未像 debian-cd 一样核实，因此不加静态规则。
2. **第三方 APT pool 可能重传。** Debian/Ubuntu 主池通常只增不改，但
   `microsoft`、`cuda`、`docker-ce`、`kubernetes`、`libnvidia-container`、
   `pigsty` 等由厂商维护的 pool 没有统一的“同路径不重传”承诺；因此 pool 保持
   U，不静态声明。
3. **npm unpublish 残余。** npm 允许在受限窗口内 unpublish；被删除版本可能
   重新发布为不同字节。绝大多数 tarball 事实上不可变，但 registry 契约不是
   绝对的。当前选择接受这一残余风险。
4. **B 类版本目录依赖发布契约。** `debian-cd`、`debian-cloud`、
   `ubuntu-cloud`、`nodejs` 的“目录只增不改”是上游契约而非协议保证；规则只
   表达当前契约，契约变化需要重新调研。`ubuntu-releases` 的两段数字目录
   （`/24.04/`、`/26.04/`）不是未知项：它们是有意排除的移动别名，上游会把它
   重新指向当前 point release，因此规则要求至少三段数字（`/24.04.5/`、
   `/24.04.5.1/`）。代价是尚未出现 point release 的两段版本目录成为漏报
   （false negative），不会误报。
5. **未来 snapshot 已由动态判断解决。** `debian-snapshot` 的静态前缀无法区分
   过去与未来 timestamp；稳定分支由 Worker 在响应侧声明 `immutable`，未来
   timestamp 与 `mr/` 不声明。这是本清单中唯一有意留给响应侧的 immutable。
6. **gcr 网络不可达（调研环境）。** `gcr.io` 在此网络超时，无法实测其
   `artifacts-downloads` 形态；该路径保持 U。
7. **`release-YYYYMMDD.N`。** 实测 Ubuntu cloud 存在带 `.N` 后缀的重发目录，
   规则已纳入；若上游改用其他后缀，会变成漏报（false negative），不会误报。

## 验证方法

- 代码侧：`server/api/mirror-manifest.test.ts` 覆盖全部 11 条 immutable
  policy 的 match/rules 形状、mount-relative 路径正反例、APT 根与嵌套布局、
  OCI referrers 不误中、以及移动别名与未命名摘要算法不匹配。
- 上游侧（2026-11-27，curl 目录页抽样）：
  - `https://go.dev/dl/`：9022 个 `go*` 根文件全部命中；覆盖 `betaN`/`rcN`、
    `darwin-*-osx10.x`，以及 `go1.4-bootstrap-20170518.tar.gz` /
    `go1.4-bootstrap-20170531.tar.gz` 两个 bootstrap 种子。该列表中没有
    `js-*`、`wasip1-*` 或 wasm 发布文件，规则也不包含这些 OS 分支。
  - `https://nodejs.org/dist/`：`v<major>.<minor>.<patch>/` 与 `latest-*` 的
    区分成立。
  - `https://cdimage.debian.org/debian-cd/`：`13.8.0/` 与 `13.8.0-live/`
    并存，`current/`、`project/` 不匹配。
  - `https://cloud.debian.org/images/cloud/bookworm/`：`YYYYMMDD-NNNN/`
    目录实测存在。
  - `https://cloud-images.ubuntu.com/releases/noble/`：同时存在
    `release-YYYYMMDD/` 与 `release-YYYYMMDD.N/`，规则两者都覆盖。
  - `https://releases.ubuntu.com/`：数字版本与 codename 别名并存。
  - `https://pypi.org/simple/requests/`：文件 URL 形如
    `/packages/<2>/<2>/<64hex>/<filename>`；该 64hex 不是 body SHA-256，规则
    只按 `/packages/` 前缀声明，不捕获。
  - `https://registry.npmjs.org/lodash`：tarball 形如
    `/lodash/-/lodash-<ver>.tgz`。
- 未验证：`gcr.io`（超时）、PyPI 文件下载（`files.pythonhosted.org` 超时）、
  ubuntu-cd 完整目录树。

## 部署顺序

`rules.immutable` 是较早的严格 relay 会拒绝的未知键，因此仍须先升级 relay，
再发布包含本文 9 条新规则的 Worker manifest。该约束来自
[ADR 0015](../../docs/adr/0015-immutable-policy-rule.md)，与本次覆盖扩展无关
但同样适用。
