# ROADMAP

## M1：独立哈希存储

- **状态**：已完成（2026-11-22；含 S3 分片与离线 prune；2026-11-23 已通过 Docker RustFS 1.0.6 一次性联测，未部署验证）
- **目标**：建立本地目录与 S3 通用的内容寻址存储、容量驱动 LRU 和哈希内容访问能力。
- **进入条件**：已满足；哈希存储边界、社区组件、容量与目录规则均已确认。
- **包含的 features**：[哈希存储](features/hash-storage.md)
- **不包含**：relay 请求缓存链路、仓库元数据解析、prefetch、遥测。
- **完成条件**：
  1. 本地目录与 S3 后端均可按真实哈希读写内容。
  2. 容量上限和 LRU 回收可用，同一 body 可通过 SHA-1 与 SHA-256 两个索引访问。
  3. 共享 S3 对象缺失可识别，CACHE_DIR 规则与 `/hash-content` 接口可用。
- **依赖**：无。

## M2：Relay 请求缓存与策略闭环

- **状态**：已完成（2026-11-23；代码与测试落地，已通过一次性 Docker RustFS 1.0.6 联测，未部署验证）
- **目标**：将新鲜度、重验、变体、流式转码、条件请求、Range、命中、MISS、no-store、no-vary 与 no-transform 接入 relay；缓存由 relay 的 `CACHE_DIR` 启用，mirrors 接口只下发回源前策略（transport、passthrough），响应侧由 `X-MDB-Relay` 与标准 HTTP 头控制。
- **进入条件**：M1 已完成，其余条件均已满足：新鲜度来源、variant 匹配字段与交付/重验响应头的分离、编码协商（qvalue/通配/identity 与平手顺序）、Range 与条件请求矩阵、GET/HEAD 共用条目而只有完整 200 GET 写入的规则、并发与故障边界、元数据归属、缓存默认开启（`CACHE_DIR` 非空时已解析 mirror 的 GET/HEAD 默认尝试缓存，`passthrough: true` 为唯一策略级跳过）、policy 边界（只下发回源前信息，不含 HEAD 探测与 prefetch）与字段位置（`rules` 顶层、`match` 不变，字段合并 / last match / `exit`）、Worker 通用 middleware（保留 `Vary`、Cache API 存储规则不变、不加隐藏头）、`no-vary` 覆盖 `Vary: *` 与首批 11 个 APT 挂载、直接来源请求剥除入站 `X-MDB-Relay`、多 variant 歧义回源、HEAD 编码协商、8 个 relaytest case 的 HEAD 行为与 Range MISS 完整 200 写缓存规则均已确认；prefetch 归 M3。
- **包含的 features**：[Relay 请求缓存](features/relay-request-cache.md)、[Relay 策略下发](features/relay-policy.md)
- **不包含**：仓库元数据递归解析、自动 prefetch、遥测接口、跨 relay 共享缓存元数据、并发 MISS 的 singleflight 合并、各 mirror 的 directive 取值调优（M2–M4 后另立研究）。
- **完成条件**：
  1. Variant 只按上游 `Vary` 所列请求头的规范化取值区分，`Accept-Encoding` 不参与匹配、`Content-Encoding` 不建立 variant；交付与重验响应头单独保存，同一 URL 与同一比较字段组合原子替换。编码偏好按完整 `Accept-Encoding` qvalue/通配/identity 协商，平手按已存编码 → `zstd` → `br` → `gzip` → `deflate`，显式编码优先隐式 `identity`，始终解码到 `identity` 再编码目标；转码不新增 variant、不写回，OCI digest 与带摘要头的条目、`no-transform`、无可接受编码和需转码的 Range 都回源。客户端 GET/HEAD、条件重验、缓存交付和流式 MISS 写入形成完整链路；GET 与 HEAD 共用同一 URL 与 variant 空间，只有完整 200 的 GET 建立条目，HEAD 从不建立条目（新鲜 HEAD 用 GET 元数据零上游回答，冷 HEAD 回源且不留条目，已有 body 的 stale HEAD 条件重验——`304` 只更新元数据、`200` / `404` / `410` 使 variant 失效但不凭 HEAD 建立新 body），其余状态与非 GET/HEAD 不写。同一 URL 下字段集不同的多条记录同时匹配时视为歧义、回源重验，只有恰好一条匹配才本地命中；`X-MDB-Relay: no-vary` 的响应忽略 `Vary`、只保留单一 variant，并覆盖 `Vary: *`（仅限 variant 划分，不绕过其他保护）；首批 11 个 `createAptMirror` 挂载注入 `no-vary`；HEAD 的编码协商、`no-transform` 与摘要禁止转码规则与 GET 相同，需转码只调交付头不带 body、冲突回源且不求值 Range。
  2. 新鲜度按上游 `s-maxage` / `max-age` / `Expires` 判定，均无时由 `X-MDB-Relay: ttl` 兜底（缺省 `0` 不兜底）；`304` 只更新元数据，无验证器时完整重取。
  3. `CACHE_DIR` 非空时缓存默认开启，已解析 mirror 的 GET/HEAD 默认尝试缓存，完整 200 GET 默认允许写，`passthrough: true` 为唯一策略级跳过；存储缺失能回退上游；`X-MDB-Relay: no-store` 与 `Cache-Control: no-store` 不产生缓存写入，缓存元数据只在本机 bbolt；并发 MISS 各自回源，Put 幂等且同 URL + 比较字段的 variant 元数据原子替换。
  4. 完整 200 缓存能回答单/多 Range、`416` 与 `If-Range`，协商目标编码等于已存编码才本地范围、需转码的 Range 回源；本地命中按 `If-None-Match` / `If-Modified-Since` / `If-Match` / `If-Unmodified-Since` 回答 `304` / `412`；Range 未命中时透传；上游忽略 Range 返回完整 200 可写缓存并记一行 warning，`206` 不写缓存。
  5. policy 只下发 relay 发起上游请求前必须知道的信息；`traffic_policy` / `passthrough` / `exit` 同为 `rules` 顶层字段，`match` 不变，`traffic_policy` / `passthrough` 按字段合并、last match win，显式 `false` 覆盖、`null` 非法，`exit` 不参与合并、应用当前规则后停止；`passthrough: true` 匹配后响应不拆包、完全跳过缓存与响应控制；`no-store` / `no-vary` / `ttl` / `no-transform` 等响应决定只走响应头（`X-MDB-Relay` 或标准 `Cache-Control`），不进入 policy；M2 不新增 HEAD 探测与 prefetch 的 policy 字段，prefetch 归 M3；不含版本协商与旧 relay 兼容。
  6. 缓存读写故障在响应开始前回退上游并留日志、写入失败不影响交付、开始后不重试；存储端到端头并在交付时重建长度、Range 与 `Age`，转码交付按规则改写摘要与验证器头；Worker 注入一版安全、通用的 middleware，统一保留 `Vary` 交付、不改 Cache API 存储规则（带 `Vary` 的响应由 Worker 缓存旁路、relay 接管）、不加隐藏头，当前不做 per-mirror 生产调优，首批 11 个 `createAptMirror` 挂载注入 `no-vary`；请求来源不是 relay（无 `X-MDB-Relay-Host`）时 relay 与 Worker 都剥除入站请求上的 `X-MDB-Relay`；`relaytest` 以 `fresh` / `revalidate` / `ttl` / `vary` / `encoding` / `range` / `no-store` / `digest` 八个固定 case 驱动上述行为的自动化验证（每个 case 都支持 HEAD，返回与 GET 相同的状态码和响应头且不带 body），临时测试配置由测试构造并清理。
- **依赖**：M1。

## M3：元数据递归与自动更新

- **状态**：未开始
- **目标**：为不可列出的 APT 仓库提供元数据目录和 relay 哈希预判，并按策略自动更新指定元数据。
- **进入条件**：M2 完成；CF/relay 解析一致性方案及 prefetch 调度、规模和失败边界确认。
- **包含的 features**：[APT 元数据索引](features/apt-metadata-index.md)、[元数据 Prefetch](features/metadata-prefetch.md)
- **不包含**：包体递归预热、完整仓库镜像、通用网页链接抓取。
- **完成条件**：
  1. CF mirror 可从递归 APT 元数据合成目录，relay 可据此建立哈希预判。
  2. 被 policy 标记的 Releases、PyPI 主目录等路径可自动保持更新。
  3. 递归范围仅限元数据，不因引用关系自动下载包体。
- **依赖**：M2。

## M4：运行遥测

- **状态**：未开始
- **目标**：提供 relay 遥测接口，使缓存命中、上游回退、容量回收和 prefetch 运行状态可观察。
- **进入条件**：M3 完成；遥测接口路径、鉴权、格式和指标清单确认。
- **包含的 features**：[Relay 遥测](features/relay-telemetry.md)
- **不包含**：外部指标存储、告警与展示系统。
- **完成条件**：
  1. 遥测接口按确认的鉴权与格式提供指标。
  2. 缓存、存储回退、LRU 和 prefetch 的关键状态及故障可通过接口观察。
- **依赖**：M3。
