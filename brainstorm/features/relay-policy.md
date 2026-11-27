# Relay 策略下发

## 目标

扩展 mirrors 接口的 policy，让 relay 在发起上游请求之前拿到它需要知道的信息：transport 偏好与是否完全透传，并以可合并的规则组合生效。

## 非目标

- 不决定缓存开关或缓存生命周期。缓存由 relay 自己的 `CACHE_DIR` 与上游响应头决定，见[Relay 请求缓存](relay-request-cache.md)。
- 不使用 policy 表达响应侧决定：`no-store`、`no-vary`、`no-transform`、实际新鲜度、实际编码或摘要等只在 relay 拿到响应后才知道的事实，一律走响应头（`X-MDB-Relay` 或标准 `Cache-Control`），不进入 manifest。
- 不用 policy 覆盖上游新鲜度。
- 不在本功能中执行具体的元数据解析或 prefetch 调度。
- 不让 policy 覆盖内部端点的旁路规则。
- 不为旧版 relay 保留兼容语义，也不引入协议版本号。

## 边界

- manifest policy 只下发 **relay 发起上游请求之前就必须知道的信息**：请求落到哪个 mirror（manifest 既有能力）、用哪条 transport、该路径是否完全透传，以及求值控制。
- 发起请求之后才能决定的事情**不进 policy**：响应是否可存（`no-store` / `private` / `Vary: *`）、是否允许转码（`no-transform`）、实际新鲜度、实际编码与摘要、资源是否缺失。它们由 relay 读取响应并自行判断，需要上游表态时按响应头（`X-MDB-Relay` 或标准 `Cache-Control`）处理。
- 因此 policy 中不存在 `cache`、`cache_ttl`、`no-store`、`no-vary`、`no-transform` 或任何等价字段。响应侧控制只作为 `X-MDB-Relay` 关键字加入，不会反向变成 policy 字段。

## 交互

- 规则项外层形状不变：`match` 保持原样，可合并字段与 `exit` 同为 `rules` 对象的顶层字段：

  ```json
  {
    "name": "...",
    "description": "...",
    "match": { "tag": "apt", "path_prefix": "/pool/" },
    "rules": {
      "traffic_policy": "speed",
      "passthrough": false,
      "exit": true
    }
  }
  ```

  - `traffic_policy`（既有字段）：`latency` 或 `speed`。
  - `passthrough`（bool，默认 `false`）：为 `true` 时该请求按原样代理，响应不拆包——不查缓存、不写缓存，不解析也不执行响应控制头，不做转码，响应字节与端到端头按上游原样交付（`X-MDB-Relay` 既不解析也不移除）。它是回源前就能确定的事实，因此属于 policy，而不是响应 directive。
  - `exit`（bool，求值控制，不是配置值）：应用当前规则后结束求值，自身不进入求值结果。
- 多条规则按顺序求值：匹配规则把显式的 `traffic_policy` / `passthrough` 合并进当前配置，后匹配覆盖先匹配的值；匹配且带 `exit` 时，在合并后立即停止求值，`exit` 自身不进入配置。未匹配任何规则的请求保持默认值。
- 合并只做**顶层浅合并**：字段整体替换，不做嵌套结构递归合并。未出现的字段继承已有值；显式的 `false` 参与覆盖；`null` 非法，视为配置错误而不是“取消设置”。`exit` 不是合并值，只在该规则命中并完成合并后生效。
- 默认配置为 `traffic_policy=latency`、`passthrough=false`，无 `exit`；未匹配任何规则时使用它。

## 实现

- 复用现有 mirror、tag、path 匹配能力与现有 transport policy；`match` 结构不变。
- 当前语义为按字段合并、last match win；`exit` 是求值控制字段，合并结果中不出现。
- `rules` 的 `traffic_policy` 与 `passthrough` 参与字段合并：后匹配可改写 transport、把某条路径改为完全透传或取消透传；规则可以省略这些字段，省略即继承。`exit` 是规则本地的求值控制，不参与合并，也不进入求值结果。
- 字段校验在 manifest 加载时完成：`traffic_policy` 出现时必须是 `latency` 或 `speed`，`passthrough` 必须是 bool，`exit` 必须是 bool；出现 `null` 或类型不符时整份 manifest 视为非法，按现有非法 manifest 处理（保留上一份可用快照），不带着部分字段继续求值。
- policy 只对**已解析到 mirror** 的请求生效；`/api`、`/keys`、`/scripts`、`/speedtest`、`/hash-content`、`/.mirror-relay` 等内部端点以及 `/relaytest` 由 relay 无条件旁路，policy 不参与也不会被这些请求匹配。
- 缓存开关不在 policy 内：`CACHE_DIR` 为空时 relay 不构造存储；非空时已解析 mirror 的 GET/HEAD 默认进入缓存链路。`passthrough: true` 是唯一的策略级跳过方式，匹配后完全跳过缓存与响应控制处理。
- policy 不定义、也不读取任何响应侧字段：`no-store`、`no-vary`、`no-transform`、`ttl`、实际新鲜度、实际编码或摘要都不在 schema 内，只能通过响应头（`X-MDB-Relay` 或标准 `Cache-Control`）表达。
- 下发方式为破坏式更新：manifest 与 policy 直接按当前语义覆盖，不携带版本号，不同时支持旧的 first-match 解释。旧 relay 读到新 manifest 会得到不同结果，这是已知且接受的结果。

## 验收

1. 后匹配字段覆盖先匹配字段，未指定的 `traffic_policy` / `passthrough` 保持继承；显式 `false` 能关闭先匹配的 `passthrough`，后匹配也能改写 `traffic_policy`。
2. `exit` 仅在规则匹配时生效，阻止后续规则参与合并，且自身不出现在求值结果中。
3. 顶层浅合并生效：不存在的可合并字段继承，不被写成零值或默认值。
4. `null`、非 bool 的 `passthrough` / `exit` 或未知的 `traffic_policy` 使 manifest 非法，relay 保留上一份可用快照。
5. transport 与 `passthrough` 可在同一求值流程中组合。
6. `passthrough: true` 匹配的路径不查缓存、不写缓存、不解析响应控制头、不做转码，响应原样交付。
7. `CACHE_DIR` 启用时，未被 `passthrough` 匹配的已解析 mirror 请求默认进入缓存链路；policy 中不存在 `cache` / `cache_ttl` 字段。
8. 内部端点与 `/relaytest` 即使被规则匹配到也不进入缓存链路。
9. manifest 只表示当前语义，不含版本协商；新 relay 不需要识别旧语义，也不保证旧 relay 读出新结果。
10. `traffic_policy`、`passthrough`、`exit` 是 `rules` 的全部顶层字段，`match` 形状不变；policy 中不存在响应侧字段，这类决定只走响应头（`X-MDB-Relay` 或标准 `Cache-Control`）。

## 状态

已完成（M2，2026-11-23；未部署验证）。实现要点：manifest policy 只下发 relay 发起上游请求之前必须知道的信息；缓存开关与兜底生命周期不由 policy 决定（`CACHE_DIR` 启用后已解析 mirror 的 GET/HEAD 默认尝试缓存，兜底生命周期由 `X-MDB-Relay: ttl` 表达）；`traffic_policy`、`passthrough`（bool，默认 `false`）与 `exit`（bool 求值控制）同为 `rules` 顶层字段，`match` 不变；`passthrough` 匹配后响应不拆包、完全跳过缓存与响应控制处理，仅作用于已解析 mirror 的请求，内部端点与 `/relaytest` 旁路；按字段合并、last match win、`exit` 应用当前规则后停止且不进入结果；顶层浅合并，缺失继承，显式 `false` 覆盖，`null` 非法；响应侧决定只走响应头；不做协议兼容，破坏式覆盖下发。

后续：prefetch 已确定为响应侧 directive（`X-MDB-Relay: prefetch`），由 relay 按真实请求在本机建立按需 lease 并在 freshness 到期时刷新，不作为 policy 字段；M2 的 policy 边界与已实现语义不变，prefetch 已在 M3 落地（见 [ADR 0014](../../docs/adr/0014-demand-driven-prefetch-leases.md)）。实现细节（bbolt 键模型、测试临时配置）由实现决定，不作为用户待决。

后续扩展（2026-11-26 下发，2026-11-27 Relay 已消费，未部署验证）：`rules.immutable` 以字符串正则声明路径本身不可变，正则跑在 mount-relative path 上，可有名为 `sha256` 或 `sha1` 的命名组；有则捕获的 hex 供摘要提取，无则仅表达路径不可变，schema 不强制要求命名组。它与 `traffic_policy` / `passthrough` 同为按字段合并的请求前事实，不是响应侧 freshness。初始范围是 OCI digest blob/manifest 与 APT `by-hash/` 的 SHA256/SHA1 对象；两条规则排在 transport 规则之前，digest blob 仍会经 `oci-blobs` 继承 `speed`，digest manifest 保持默认 latency。Relay 合并后只对 mount-relative path 求值一次存活的表达式：命中的命名组必须是唯一且为 hex，才作为内容摘要；否则只声明不可变。因较早的严格 Relay 把未知 `rules` 键视为整份 manifest 非法，部署顺序仍是先升级 Relay、再发布 Worker manifest；语义见 [ADR 0015](../../docs/adr/0015-immutable-policy-rule.md)，Relay 侧用法见 [ADR 0016](../../docs/adr/0016-tiered-active-cache-for-immutable-objects.md)。

覆盖扩展（2026-11-27，未部署验证）：`rules.immutable` 从 2 条内容寻址规则扩为 11 条。APT `by-hash` 的模式加可选嵌套仓库根 `^/(?:.+/)?dists/…`，覆盖 `microsoft` 的 `code/dists/…`；另加 9 条按 mirror 精确限定的发布契约规则（PyPI `/packages/`、npm tarball、Node.js 版本目录、Go 发布文件、Kubernetes release 目录、Debian CD、Debian cloud、Ubuntu cloud、Ubuntu releases），均只声明不可变、不捕获摘要，并保持排在 transport 规则之前。逐 mirror 分类、外部路径与 mount-relative 的对应、响应头/body hash 的研究结论及未知项见 [relay-immutable-coverage.md](relay-immutable-coverage.md)。响应头与 body 内 checksum 不作为 policy 来源。
