# 元数据 Prefetch

## 目标

根据 mirror 在响应中注入的 `X-MDB-Relay: prefetch` 指令，自动刷新该 relay 最近实际使用的元数据，使 Releases、PyPI 主目录等资源保持更新，并为目录合成和哈希预判提供新数据。

## 非目标

- 不因递归元数据解析预热全部包体。
- 不对未标记的路径默认启用全仓库 prefetch。

## 交互

- 每个 mirror 自行决定哪些响应携带 `prefetch`；不增加 manifest policy 字段。
- relay 只在实际请求发现该指令后，于本机内存登记 prefetch，初始保留期为 3 天；从未访问过的资源不启动任务，重启后也不恢复未再次发现的任务。
- GET/HEAD 请求把对应任务的保留期重新延长到 3 天；保留期内资源到达 freshness expiry 时后台发条件 GET。请求遇到 stale 时仍走 M2 同步重验并续期，不额外制造重复 prefetch。连续 3 天无请求后删除任务；没有任何 freshness 的资源不自动循环刷新。
- `no-store` 始终优先于 `prefetch`；同时出现时不建立普通请求缓存条目，并记录 warning。APT 只更新独立规范化索引及哈希预判，不建立可变 Release/Packages 的客户端缓存 variant；普通可缓存资源可同时更新请求缓存。
- 普通内容仍在真实请求 MISS 时按需获取。

## 实现

- prefetch 仅递归元数据，是否预热引用内容不属于本功能。
- `CACHE_DIR` 为空时完全禁用 prefetch。
- 默认后台并发为 4，可配置；同一资源不重复运行。任务按 URL + variant identity 区分，后台 GET 重放 variant 请求头并固定 `Accept-Encoding: identity`；HEAD 可登记/续期，后台刷新永远使用 GET。到期加入少量 jitter；失败按 1、5、15、30 分钟退避，30 分钟封顶，旧可用数据不因失败删除。
- 单对象受 `CACHE_MAX_SIZE` 限制；默认最多 4096 个内存任务，可配置，满时拒绝新增并记录 warning，不驱逐活跃任务。
- 只给 Release/InRelease 注入 APT `prefetch`；同一 mirror 的这些请求共同登记并续期一个固定索引任务，后台目标为 `/<mount>/.mdb/apt-index.json`，不为每个 Packages 建任务。APT 的 `no-store` + `prefetch` 冲突在登记或状态变化时记录 warning，避免逐请求刷屏。
- APT 索引有 `Valid-Until` 时取所有 suite 的最早到期时间；全部缺失时成功生成后默认新鲜 1 小时，`Date` 不参与 TTL。索引端点提供对应 Cache-Control 与整代 ETag；relay 到期条件 GET，304 续期、200 原子换代。
- PyPI 主目录的自动更新不意味着递归抓取所有包页面，除非后续另有明确规则。只有 `/pypi/simple/` 根索引的 200/304 带 `prefetch`；带 `If-None-Match` / `If-Modified-Since` 的请求真实到达上游以取得 `304`，无条件请求仍走 Worker 读穿缓存。
- 并发与任务上限的配置键为 `prefetch.concurrency`（默认 4）与 `prefetch.max_tasks`（默认 4096），对应环境变量 `PREFETCH_CONCURRENCY` / `PREFETCH_MAX_TASKS`；非正值回落到默认值。
- relay 关闭时停止派发并取消在途请求；lease 与 APT 索引都只在内存，重启后等待真实请求重新发现。

## 验收

1. 被标记路径能无需客户端请求而自动更新，未标记路径不启动 prefetch。
2. APT 递归只获取元数据引用，不自动获取包体。
3. 更新结果可被 APT 目录合成和 relay 哈希预判使用。
4. 调度、失败与资源占用边界有明确配置，默认值即生效。

## 状态

已完成（M3，2026-11-23；未部署验证）。`prefetch` 是响应侧 `X-MDB-Relay` 关键字：五个 snapshot APT 挂载的 `Release`/`InRelease` 200/304 与 PyPI `/simple/` 根索引 200/304 携带它。relay 只在本机内存登记 lease（3 天），GET/HEAD 续期；到期后按 freshness 条件 GET（`304` 续期、`200` 替换、`404`/`410` 删除），无 freshness 不循环刷新，重启不恢复；`no-store` 优先并于状态变化时记一次 warning；失败按 1/5/15/30 分钟退避，到期加 jitter；默认 4 并发、4096 任务且可配置；`CACHE_DIR` 为空时完全禁用。APT 每个 mirror 共用一个聚合索引任务，目标固定为 `/<mount>/.mdb/apt-index.json`；PyPI 条件请求真实到上游，无条件请求维持 Worker 读穿缓存。
