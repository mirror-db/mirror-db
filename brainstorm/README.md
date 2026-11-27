# Relay 本地缓存与元数据索引

## 项目目标与范围

为 relay 增加支持本地目录与 S3 的内容寻址缓存、容量驱动的 LRU 回收和仓库元数据哈希预判；CF mirrors 为不支持目录列出的 APT 仓库合成目录。缓存由 relay 自己的 `CACHE_DIR` 启用，mirrors 接口只下发回源前策略（transport、passthrough），响应侧决定由 `X-MDB-Relay` 与标准 HTTP 头控制；元数据 prefetch 属 M3（已完成），由响应 `X-MDB-Relay: prefetch` 指令触发，不进入 policy。

递归 prefetch 仅覆盖元数据，不自动下载包体或构建完整仓库镜像。

## 当前状态

M1 已完成：本地/RustFS 内容寻址存储、严格字节 LRU、bbolt 持久索引和公开只读 `/hash-content` 均已实现并通过竞态测试。M2 已完成：把请求缓存接入 relay（本地命中、条件重验、HEAD、Range、流式转码、MISS 写入）、policy 改为按字段合并并支持 `passthrough`/`exit`、Worker 侧注入 `X-MDB-Relay` 控制头与 `/relaytest` 测试上游。2026-11-23 已用一次性 Docker RustFS 1.0.6 容器通过真实 S3 联测；未加入 CI，尚未部署验证。

M3 已完成（2026-11-23；Worker 与 relay 代码、测试落地，未部署验证）：Worker 独占解析 APT 元数据并以固定 `/<mount>/.mdb/apt-index.json` schema 1 发布整代索引，供目录合成与 relay 双哈希预判；prefetch 为响应侧 `X-MDB-Relay: prefetch` 指令，relay 按真实请求建立本机内存 lease 并在 freshness 到期时条件刷新。同日对五个真实上游（`syncthing-pkgs`、`hashicorp`、`mise`、`opentofu`、`github-cli`）完成一次快照构建与稳定 ETag 验证（`hashicorp` 一代去重后解析 15 个索引、约 5.92 MiB），未加入 CI。决策见 [ADR 0013](../docs/adr/0013-worker-owned-apt-metadata-index.md) 与 [ADR 0014](../docs/adr/0014-demand-driven-prefetch-leases.md)。

不可变对象分段缓存已实现（2026-11-27；未部署验证）：`rules.immutable` 命中的 GET 由进程内 `go-tiered-cache` 装配并合并为一次上游，完整后经 `hashstore.Adopt` 发布，普通非 immutable 链路不变；片段只在内存与 `.tmp`，重启不恢复。决策见 [ADR 0016](../docs/adr/0016-tiered-active-cache-for-immutable-objects.md)，范围见[不可变对象分段缓存](features/relay-active-cache.md)。M4 仍未开始。

已确认：共享 S3 对象可被独立驱逐，缺失时回退上游；请求缓存的新鲜度由上游 `s-maxage` / `max-age` / `Expires` 决定，`X-MDB-Relay: ttl` 只在无上游新鲜度时兜底（这一区分不改动 M1 哈希存储本身，body 回收仍只由 LRU 负责）；`no-cache` 存但每次重验，`no-store` / `private` / `Vary: *` 不存（声明 `no-vary` 时 `Vary: *` 也只有一个 variant）；过期后条件重验，`304` 更新元数据、`200` 替换、确定缺失删除；`X-MDB-Relay` 控制头认 `no-store`、`no-vary`、`prefetch` 与 `ttl` 且交付前移除（`passthrough` 响应除外）；请求来源不是 relay（无 `X-MDB-Relay-Host`）时 relay 与 Worker 都剥除入站请求上的 `X-MDB-Relay`；响应决定只走响应头（`X-MDB-Relay` 或标准 `Cache-Control`），不进入 policy。

缓存链路已实现：缓存元数据只在本机 bbolt，不进共享 RustFS；variant identity 只是上游 `Vary` 所列请求头的规范化取值（`X-MDB-Relay: no-vary` 时忽略 `Vary`、单一 variant），`Accept-Encoding` 不参与匹配、`Content-Encoding` 不建立 variant，交付/重验响应头单独保存，同一 URL 与同一比较字段组合原子替换；编码偏好按完整 `Accept-Encoding` qvalue/通配/identity 协商（平手按已存编码 → `zstd` → `br` → `gzip` → `deflate`，显式编码优先隐式 `identity`），一律解码到 `identity` 再编码目标，库用标准库 gzip/zlib + andybalholm/brotli + klauspost/compress/zstd；转码不写回、不新增 variant，转码交付去掉 `Content-Length` / `Content-MD5` / `Digest` / `Docker-Content-Digest` 并把强 `ETag` 转弱；OCI digest 路径、带摘要头的条目、`no-transform`、无可接受编码和需转码的 Range 都回源；本地命中先按 `If-*` 回答 `304` / `412` 再求值 Range；普通 variant 不做 singleflight，并发 MISS 各自回源（不可变 GET 的合并在分段缓存，见上），Put 幂等、variant 元数据原子替换；缓存读写故障只降级不阻断交付，响应开始后不重试；存储端到端头并排除 hop-by-hop 等，交付重建长度、Range 与 Age；`relaytest` 是 Worker 侧受控测试上游，relay 自身旁路，固定 `fresh` / `revalidate` / `ttl` / `vary` / `encoding` / `range` / `no-store` / `digest` 八个 case。同一 URL 下字段集不同的多条记录同时匹配视为歧义、回源重验；Range MISS 时上游忽略 Range 返回完整 200 可写缓存并记一行 warning，`206` 不写。

GET 与 HEAD 共用同一 URL 与 variant 空间，只有完整 200 的 GET 建立条目：新鲜 HEAD 不发起上游、用 GET 元数据回答且不发 body；冷 HEAD 回源且不留无 body 条目；已有 body 的 stale HEAD 按条件重验，`304` 只更新元数据，`200` / `404` / `410` 使该 variant 失效但不凭 HEAD 建立新 body。HEAD 与 GET 用同一套编码协商与禁止转码规则，需转码只调交付头、不带 body，冲突回源，且不求值 Range；`relaytest` 的 8 个 case 都支持 HEAD。其余状态与非 GET/HEAD 一律不写缓存。

policy 边界已实现：manifest policy 只下发 relay 发起上游请求前就必须知道的信息（`traffic_policy`、`passthrough`、`exit`）；响应后才能决定的事项（`no-store`、`no-vary`、`prefetch`、`ttl`、`no-transform`、实际新鲜度、编码与摘要）只走响应头（`X-MDB-Relay` 或标准 `Cache-Control`），绝不进入 policy。缓存开关不由 policy 决定：`CACHE_DIR` 非空即启用，已解析 mirror 的 GET/HEAD 默认尝试缓存，完整 200 GET 默认允许写；匹配 `passthrough: true` 的请求完全跳过缓存与响应控制处理。`traffic_policy` / `passthrough` / `exit` 为 `rules` 顶层字段，`match` 不变；`traffic_policy` / `passthrough` 按字段合并、last match win，`passthrough` 默认 `false`、显式 `false` 覆盖、`null` 非法，`exit` 只结束求值、不参与合并。M2 不新增 HEAD 探测与 prefetch 的 policy 字段；prefetch 已在 M3 作为响应指令落地，M4 仍未开始。

Worker 侧已注入一版安全、通用的 middleware：统一把 `Vary` 交付给客户端与 relay，保持 Cache API 存储规则（带 `Vary` 的响应旁路 Worker 缓存、由 relay 接管），不加隐藏头。首批 11 个 `createAptMirror` 挂载注入 `X-MDB-Relay: no-vary`（`debian`、`debian-ports`、`debian-security`、`ubuntu`、`ubuntu-ports`、`mise`、`syncthing-pkgs`、`hashicorp`、`opentofu`、`github-cli`、`gitlab-runner-deb`），覆盖 `Vary: *` 但仅限 variant 划分，不代表缓存策略。请求来源不是 relay（无 `X-MDB-Relay-Host`）时 Worker 与 relay 都剥除入站请求上的 `X-MDB-Relay`。当前不做 per-mirror 生产缓存调优；M2–M4 完成后再独立研究每个 mirror 的合适取值，将来按需新增 directive。

M1 复用 `google/go-lru`、bbolt、AWS SDK for Go v2 与 `docker/go-units`；统一使用 CACHE_DIR，不再设置 TMP_DIR。

后续待明确：遥测指标与鉴权，以及 M2–M4 后各 mirror 的 directive 取值研究；部署验证尚未进行。真实 RustFS 1.0.6 的一次性 Docker 联测已通过，未加入 CI。实现细节（bbolt 键模型、`relaytest` 临时配置的构造与清理）由实现决定。

## 文档索引

- [ROADMAP](ROADMAP.md)：分阶段成果、进入条件与完成条件。
- [哈希存储](features/hash-storage.md)：本地/S3 内容寻址存储、容量与哈希访问接口。
- [Relay 请求缓存](features/relay-request-cache.md)：新鲜度与重验、变体匹配与流式转码、条件请求与 Range、命中/MISS、并发与故障降级、Worker 通用 middleware。
- [不可变对象分段缓存](features/relay-active-cache.md)：immutable 路径的进程内装配、singleflight、发布与回收、容量与磁盘水位边界。
- [Relay 策略下发](features/relay-policy.md)：policy 边界（只下发回源前信息）、`traffic_policy` / `passthrough` / `exit` 字段与默认值、合并语义、破坏式下发。
- [静态 immutable 覆盖](features/relay-immutable-coverage.md)：40 个 mirror 的 immutable 分类、mount-relative RE2、外部路径对照、响应头/body hash 研究结论与未知项。
- [APT 元数据索引](features/apt-metadata-index.md)：CF 目录合成与 relay 哈希预判。
- [元数据 Prefetch](features/metadata-prefetch.md)：按响应 `X-MDB-Relay: prefetch` 指令、由真实请求驱动的元数据自动更新。
- [Relay 遥测](features/relay-telemetry.md)：遥测接口的待定义边界。
