# 不可变对象分段缓存

## 目标

对 manifest `rules.immutable` 命中的路径，把并发读同一对象的请求合并成一次上游装配：被动缓存按 byte range 拉取缺失区间，完整后交给哈希存储，使冷 Range、并发 MISS 和跨 mirror 的同摘要请求不必各自完整回源。

## 非目标

- 不改变普通（非 immutable）请求的 `streamAndStore` 链路，也不对它们加 singleflight。
- 不新增第二套完整对象存储：完整 body 仍只进 hashstore，LRU、容量和 S3 后端不变。
- 不做跨 relay 的片段共享或持久化，不在重启后恢复未完成装配。
- 不引入新的 manifest 字段；路径是否不可变只来自 `rules.immutable`（ADR 0015）。
- 不处理遥测；本轮不增加指标或 exporter。
- 不把片段当作可长期命中的 URL variant。

## 交互

- 只在已解析 mirror、`CACHE_DIR` 启用、`passthrough` 为假时进入；内部端点与 `/relaytest` 仍旁路。
- 入口条件：merged policy 命中 `immutable`、客户端未强制重验（`no-cache` / `max-age=0`）、客户端接受 `identity`、方法为 `GET`。`HEAD` 只能复用已按摘要寻址的 body，不建立条目；带 `If-*` 的首个请求不 seed，交给上游裁决。
- 零 variant 时顺序为：APT 哈希预判 → 按摘要的本地命中 → 本轮 active 装配（仅 GET）→ 普通 MISS。stale variant 在 OCI digest 或 immutable 路径上先尝试本地命中，再走原重验路径。
- 无命中且装配失败时，已取得的上游响应按普通链路交付，不二次回源。

## 实现

- 依赖 `github.com/geektr-cloud/go-tiered-cache v0.0.1`；每个不可变对象一个 `PassiveCache`，由 `activecache.Manager` 按 key 管理。key 为捕获到的 `sha256:<hex>` / `sha1:<hex>`，无摘要时为 URL key（仅去重同一路径的并发请求）。
- 首个 `Acquire` 成为 creator，唯一联系上游；后续读者等待 `Init`，失败则回落普通链路。这就是不可变对象的 singleflight。
- 片段写入 `<CACHE_DIR>/.tmp` 的 scratch 文件；完整后经 `hashstore.Adopt` 发布。`Adopt` 只接受该目录下大小相符的完整普通文件，一次读取重算 SHA-1 与 SHA-256；policy 捕获的摘要是期望值，不符则不发布且不碰已有 body。
- `entryStore` 同时实现 `tiered.Store` 与 `CacheHandoff`：正常路径 `FinalizeFromCache` 原地 adopt，回退路径 `Finalize` 复制后 adopt。
- 发布后 entry 立即离开 active map，scratch 名被 Reclaim；已有读者用发布前打开的本地 fd 继续 drain，字节预算保留到最后一个引用释放。LRU/驱逐仍属 hashstore。
- 完整交付或覆盖整个对象的 Range 交付后，等待 `Adopt`（上限 60s）再写普通 URL variant，让后续无摘要请求也能本地命中；发布失败或大小不符则不写。
- Seed 使用 `Accept-Encoding: identity`，multipart Range 只转发第一段；`206` 必须带具体总长度。以下情况回退普通链路：未知/零长度、超过 `CACHE_MAX_SIZE`、`200` 回应 Range、`206` 无总长、写禁止条件（`no-store`/`private`/非 `Accept-Encoding` 的 `Vary`/非 identity 编码等）、容量或磁盘水位拒绝。
- 选择 `PassiveCache` 而非 `SequentialCache`：后者要求从 0 顺序读完整前缀且只有一个上游，无法服务任意 offset 的 layer 续传、multipart Range 或 `416`。
- 限额：`CACHE_ACTIVE_MAX_ENTRIES`（默认 64）、`CACHE_ACTIVE_CAPACITY`（默认 `cache.max_size`，不可更大）、`CACHE_ACTIVE_IDLE_TIMEOUT`（默认 10m）。已知大小在 `Init` 预留；未知大小单独准入并按写入增长封顶；`StalledTime` 超时的空转 entry 被回收。已准入的 entry 不因容量或水位中途失败。
- `CACHE_DISK_WATER_MARK` 统一约束本地写入：`Put`、`Adopt` 与 active 准入在建临时文件前检查，大 body 写入期间复检；低于水位时不可变请求按普通链路回源且不缓存，读/删/prune 不受限。
- 重启不恢复片段：active map 只在内存，启动时清空 `.tmp`。完整 hash 对象不受影响。
- 关闭顺序：`Server.Close` 先停 active manager，再关 hashstore。

## 验收

1. 同一不可变对象的并发 GET 只产生一次上游装配；不同 Range 的读者共享已取字节，冷 Range 不先下载完整对象。
2. 完整 body 只以真实摘要进 hashstore 一次；摘要不符、超预算或低水位都不产生条目，且不影响客户端拿到响应。
3. 普通非 immutable 请求行为与 M2/M3 完全一致。
4. 重启后无残留片段；关闭时无泄漏 goroutine 或未释放的 fd。

## 状态

已实现（2026-11-27；代码与测试落地，未部署验证）。依赖 `go-tiered-cache v0.0.1`；发布顺序 Relay 先、Worker manifest 后，避免较早的严格 Relay 因未知 `rules` 键丢弃整份 policy。语义见 [ADR 0016](../../docs/adr/0016-tiered-active-cache-for-immutable-objects.md)，字段定义见 [ADR 0015](../../docs/adr/0015-immutable-policy-rule.md)。
