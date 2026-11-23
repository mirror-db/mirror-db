# Relay 请求缓存

## 目标

将本地命中、上游探测与重验、条件请求、流式转码、缓存交付、Range 响应和 MISS 写入串成统一请求链路，在流式交付的同时建立真实哈希存储。缓存元数据持久化后，仍新鲜的请求直接由本地元数据与 body 完成交付，不为每次命中支付一次上游探测成本。并发 MISS 不做请求合并，缓存故障只降级不阻断交付。

## 非目标

- 不为 `X-MDB-Relay: no-store` 或上游 `Cache-Control: no-store` 的响应建立任何缓存。
- 不在本功能中实现仓库元数据递归解析或自动 prefetch。
- 不在 relay 之间共享缓存元数据；共享存储只承载 body。
- 不为旧版 relay 保留语义兼容，也不引入协议版本号。
- 不改变内部端点（API、keys、scripts、speedtest、hash-content、relay 控制面）的现有行为。

## 作用范围

- 缓存开关属于 relay 的部署，不属于 manifest：`CACHE_DIR` 为空时不构造存储，所有请求按原样透传；非空即启用请求缓存。
- 启用后，**已解析到 mirror** 的 GET/HEAD 默认尝试缓存：先按地址键与 variant 查询本地元数据，**完整 `200` 的 GET 默认允许写入**。policy 不提供缓存开关；匹配到 `passthrough: true` 的请求是唯一的策略级例外，完全跳过缓存与响应控制处理。
- 只有**已解析到 mirror** 的请求进入缓存链路。`/api`、`/keys`、`/scripts`、`/speedtest`、`/hash-content`、`/.mirror-relay` 等内部端点一律旁路。
- 仅 GET 与 HEAD 进入链路；其余方法直接透传。二者共用同一 URL 与 variant 空间，但**只有完整 200 的 GET 建立条目**，HEAD 从不建立条目，见「HEAD 请求」。
- `relaytest` 是 Worker 上的可控测试上游，relay 对它自身的请求一律旁路，见「测试端点」。

## Worker 侧配合

- Worker 前端为所有 mirror 统一注入一层通用 middleware，只做与具体 mirror 无关的安全处理，不携带 per-mirror 的缓存取值。
- 该层把上游的 `Vary` 原样带到交付响应，relay 靠它划分 variant。Worker 保持 Cache API 自身的存储规则（带 `Vary` 不存），并停止用剥除 `Vary` 的方式绕过它：带 `Vary` 的响应由 Worker 缓存旁路、由 relay 接管；不引入隐藏头或旁路标记。
- 除 `Vary` 外，现有 sanitize 与缓存规则继续生效；若 middleware 或 mirror 的 `Cache-Control` 覆盖会重写该头，必须保留上游的 `no-transform`，否则 relay 无法遵守。当前阶段不做生产 per-mirror 缓存调优；每个 mirror 合适的响应决定（哪些响应该 `no-store`、哪些不该转码）留到 M2–M4 完成后再独立评估，将来按需以 `X-MDB-Relay` directive 表达，而不是写进 policy。
- 11 个 `createAptMirror` 挂载首批注入 `X-MDB-Relay: no-vary`（见「变体」）。这是上游对该资源的能力声明，不是缓存策略：它不影响 Worker 自身的缓存规则，也不决定 relay 是否缓存该路径。
- 请求来源不是 relay 时（以 `X-MDB-Relay-Host` 缺失判断），relay 与 Worker 的 mirror 层都剥除**入站请求**上的 `X-MDB-Relay`。该头只描述上游对响应的控制，请求里的副本没有语义、可能是客户端伪造；剥除只作用于请求，上游响应上的 `X-MDB-Relay` 仍照常解析。
- 验收仍用 `relaytest` 与临时 mirror/上游配置下发的测试 policy，不要求生产 mirror 携带测试专用配置。

## 交互

1. **本地命中**：该 URL 下存在与请求的命中比较字段逐字段相等的缓存元数据且仍新鲜时，直接用持久化的状态、响应头与内容哈希交付本地 body，**不发起上游请求**。命中判断只比较字段取值，不经过摘要或指纹。命中后先按「条件请求」求值 `If-*`，再按「Range」求值范围，最后交付。HEAD 命中交付同一套元数据但不发 body、不求值 Range，见「HEAD 请求」。
2. **哈希预判**（依赖后续里程碑的元数据索引）：从仓库元数据生成的资源—哈希映射直接定位本地 body，命中即交付，跳过上游探测。
3. **重验与探测**：在上游要求每次重验（`no-cache`）、元数据不新鲜或缺失时发生。有验证器时发条件 GET（`If-None-Match` / `If-Modified-Since`）；无验证器时完整重新获取。M2 不引入 HEAD 探测：不新增 policy 字段，也不自行用 HEAD 代替 GET 重验；客户端发来的 HEAD 请求按「HEAD 请求」处理。
4. **重验结果**：`304` 只更新元数据（新鲜度、响应头），不动 body；`200` 替换该 variant 的元数据与 body 引用；`404` / `410` 视为该 variant 确定缺失，删除条目。以上针对 GET 重验；HEAD 重验拿不到 body，`200` 只让 variant 失效而不替换（客户端 GET 因此继续走完整 MISS），见「HEAD 请求」。
5. **缓存交付**：命中后若本地 body 不存在或读取失败，回退上游；存储缺失按缓存 MISS 处理，不作为持久性错误。回退与故障判定的边界见「故障处理」。
6. **MISS**（仅完整 200 的 GET）：获取上游 body，写 `<CACHE_DIR>/.tmp`，同时向客户端流式交付并计算 sha1/sha256，完成后按真实哈希提交存储，并为该 URL 与命中比较字段集合写入缓存元数据。同一 variant 的并发 MISS 各自独立完成这条路径，不合并、不互相等待（见「并发」）。非 200 的响应、未完整传输完的 body 与 `206` 不建立条目（HEAD 见「HEAD 请求」）。
7. **禁止缓存**：`X-MDB-Relay: no-store`、`Cache-Control: no-store`、`Cache-Control: private` 或 `Vary: *`（未声明 `no-vary`）的响应直接交付，不写 body、不写元数据，也不进入 prefetch。`passthrough: true` 匹配的请求在链路入口就跳过缓存与响应控制，不读也不写。

## HEAD 请求

- HEAD 与 GET 共用同一 URL 与 variant 空间：命中判定、比较字段集合与记录完全一致。HEAD 不占用独立条目，也不写自己的命中比较字段，因此 HEAD 与 GET 不会互相产生两份记录。
- 只有**完整 200 的 GET** 建立缓存条目。HEAD 的任何响应都不写 body、不写元数据、不写已存响应头；其余方法同样不写。
- **新鲜命中**：不发起上游请求，用已存的 GET 元数据回答——状态码、已存响应头、按本地 body 大小重建的 `Content-Length`、按存储时刻重建的 `Age`，不带 body。前置条件照常由本地元数据回答（`304` / `412`）；HEAD 无 body，所以不求值 Range，也不产生 `206` / `416`。
- **编码协商与 GET 同规则**：同一套 `Accept-Encoding` qvalue/通配/identity 规则，同一套 `no-transform` 与摘要条目的禁止转码规则。需要转码时只改交付头（去掉 `Content-Length`、强 `ETag` 转弱等），不产生任何转码字节；编码偏好无法满足，或与禁止转码规则冲突时与 GET 一样回源，由上游回答。
- **冷 HEAD**（该 URL 与比较字段下无条目）：回源后原样交付，**不创建无 body 的条目**；之后的 GET 仍然是 MISS。
- **stale HEAD**（已有 body 的条目不再新鲜）：与 GET 的重验同样处理——`304` 只更新元数据；`200` / `404` / `410` 使该 variant 失效，下一次 GET 按 MISS 重建，**不凭 HEAD 响应建立新 body 或新记录**；其余状态不写缓存，条目保持原样。
- 缓存交付的 HEAD 与 GET 使用同一套响应头重建规则；差别只是 HEAD 不发送 body。

## 新鲜度与重验

- 新鲜度由上游 `s-maxage` / `max-age` / `Expires` 决定：共享缓存先看 `s-maxage`，再看 `max-age`，最后 `Expires`。上游都没有时，才使用 `X-MDB-Relay: ttl=<seconds>` 给出的兜底生命周期；缺省 `0` 表示不提供兜底，不是“永不过期”。`ttl` 不覆盖上游结论。
- `no-cache` 的响应照常写入缓存，但每次命中都先重验。
- `no-store`、`private`、`Vary: *`（未声明 `no-vary`）的响应不写入缓存。
- 客户端请求侧缓存控制同样是事实：`Cache-Control: no-store` 跳过查询与写入；`no-cache` 或 `max-age=0` 让新鲜条目先重验；`Pragma: no-cache` 仅在请求没有 `Cache-Control` 时按 `no-cache` 处理。
- `304` 重验把新的新鲜度和响应头并入元数据；若该 `304` 新声明 `no-store`、`private` 或 `Vary: *`（未声明 `no-vary`），条目被删除而不是刷新。
- 上游不带任何验证器（`ETag` / `Last-Modified`）时无法条件重验，过期后按完整重新获取处理。
- 新鲜度只决定是否先问上游，不决定 body 是否保留；body 回收仍由 LRU 负责。

## 变体

- 同一 URL 下可存在多个 variant，各自保存**命中比较字段**、**已存响应头**、状态码、body 哈希与新鲜度。两组字段用途不同，不共用同一清单：
  - **命中比较字段**只来自上游响应的 `Vary` 所列请求头，保存该请求头在本次请求中的规范化取值。字段名小写；值去掉首尾空白；同名多值按接收顺序合并；值本身不做大小写折叠；请求未带该头与带空值是两种不同取值，不互相等同。
  - **已存响应头**（`Content-Encoding`、`ETag`、`Last-Modified`、`Cache-Control`、`Expires`、`Content-Type`、摘要头等）只用于交付、条件重验与范围求值，不参与命中身份。
- variant identity 是这组规范化取值的集合，不是哈希：命中时逐字段比较，全部相等即同一 variant。字段集合本身即记录的一部分，因此调整或新增字段不需要重算任何指纹。
- 上游不发送 `Vary`、或 `Vary` 不含任何可比较请求头的 URL 只有一个 variant；`Vary: *` 不缓存。
- **`X-MDB-Relay: no-vary`**：上游声明该响应的 URL path 已足以唯一表示资源（例如 APT：同一 path 的字节不随请求头变化）时，relay 忽略该响应的 `Vary` 划分，不生成命中比较字段，同一 URL 只有一个 variant；`Vary` 仍作为已存响应头原样交付。`no-vary` **覆盖 `Vary: *`**：声明了 `no-vary` 的响应即使带 `Vary: *` 也只有一个 variant。覆盖范围**仅限 variant 划分**，不绕过其他保护：`no-store`、`private`、`no-transform`、摘要条目禁止转码与 `passthrough` 规则照常生效。
- 首批注入 `no-vary` 的是 11 个 `createAptMirror` 挂载：`debian`、`debian-ports`、`debian-security`、`ubuntu`、`ubuntu-ports`、`mise`、`syncthing-pkgs`、`hashicorp`、`opentofu`、`github-cli`、`gitlab-runner-deb`。这是上游对自身内容的能力声明，**不代表缓存策略**：其余 mirror 不因缺少它而失去默认缓存资格，只是带 `Vary` 的响应照常按 `Vary` 划分，`Vary: *` 照常不缓存。
- **`Accept-Encoding` 不参与匹配**：即使出现在 `Vary` 中，也不作为比较字段。请求的编码偏好与已存 variant 不一致时，该 variant 仍然命中，交付时按「流式转码」处理；不为编码差异新增 variant，转码结果也不写回缓存。
- **`Content-Encoding` 不建立 variant**：编码是已存 body 的属性，不是 variant identity 的一部分；同一 URL 与同一比较字段组合下只能有一条记录。
- 同一 URL 与同一比较字段组合的写入是**原子替换**：新回源结果整体替换旧记录，不出现两条记录或多次回源拼接出的混合状态。
- `ETag` 与 `Last-Modified` 是重验元数据，不参与 variant identity。
- HEAD 与 GET 落在同一 variant 空间：HEAD 按同一组比较字段查同一条记录，不新增记录、不建立 body；stale HEAD 的 `304` 只更新元数据（见「HEAD 请求」）。
- 上游 `Vary` 集合可能在两次回源之间变化，同一 URL 下因此可能并存比较字段集不同的多条记录。请求同时匹配多于一条记录（各记录按自己的字段集都判相等）即**歧义**：relay 不挑选其中一条，按 MISS 回源重验；只有恰好一条记录匹配时才按本地命中处理。歧义回源成功后，实现会删除该 URL 下与新记录身份不同的旧记录；这不改变命中判定，只让下一次请求回到唯一候选。

## 流式转码

- 转码发生在交付阶段：在存储 body 的读取流上先解码到 `identity`，再编码为目标编码，边转码边交付；不落地第二份内容，不为转码结果建立 variant，也不改写已存 variant 的编码与摘要。不做编码到编码的直接链式转换。
- 编码集合固定为 `identity`、`gzip`、`deflate`、`br`、`zstd`。实现使用标准库 `compress/gzip` 与 `compress/zlib` 处理 `gzip` / `deflate`，`github.com/andybalholm/brotli` 处理 `br`，`github.com/klauspost/compress/zstd` 处理 `zstd`。
- 目标编码由请求的 `Accept-Encoding` 完整协商：
  - 解析 qvalue（缺省 `1`）、通配 `*` 与 `identity`。未带 `Accept-Encoding` 时任何编码可接受，优先直接交付已存编码；`identity` 除非被 `identity;q=0` 或 `*;q=0` 排除，否则默认可接受。
  - 取 q 最高的编码。平手时先看显式与隐式：显式列出的编码优先于仅由缺省规则可接受的 `identity`；其余平手顺序为已存编码 → `zstd` → `br` → `gzip` → `deflate`。
  - 已存编码可接受时直接交付原字节，不转码；否则解码到 `identity` 再编码到目标编码。`identity` 是合法目标编码。
  - 没有可接受编码或目标编码不可用时**回源**：把原请求（含范围头）透传上游，响应直接交付且不写入缓存。
- 遵守 `Cache-Control: no-transform`：带该标记的条目永不转码；请求的编码偏好无法由已存编码满足时回源。
- 转码交付的响应头：
  - 移除描述已存字节的描述性头：`Content-Length`、`Content-MD5`、`Digest`、`Docker-Content-Digest`。
  - 强 `ETag` 降级为弱 `ETag`（加 `W/` 前缀）；本来就弱的保持弱。不新增客户端没见过的验证器。
  - 其余端到端头原样保留（含 `Last-Modified`、`Cache-Control`、`Content-Type`、`Vary`）；未转码交付的头集保持不变，只按「响应头存储」重建长度与范围头。
- **禁止转码**：OCI digest 路径，以及已存响应带内容摘要头（`Digest` / `Docker-Content-Digest` / `Content-MD5`）的条目。这些条目的字节身份就是摘要，转码会让摘要与响应头互相矛盾。此类条目在编码不匹配时**回源**，由上游决定响应，本地不做转码。

## 条件请求

- 本地命中先求值前置条件，再求值 Range，最后交付：
  - `If-None-Match` 命中当前 variant 的验证器 → `304`，只回验证器、新鲜度、`Vary` 等相关头，不交付 body。
  - `If-Modified-Since` 仅在请求未带 `If-None-Match` 时参与；资源未改变 → `304`。
  - `If-Match` 不匹配 → `412`。
  - `If-Unmodified-Since` 仅在请求未带 `If-Match` 时参与；资源已改变 → `412`。
- 比较强度：`If-None-Match` 用弱比较（`W/` 前缀不影响匹配）；`If-Match` 用强比较（弱验证器不匹配任何 `If-Match`）。日期比较只对存在 `Last-Modified` 的条目求值，没有则忽略该条件。
- `304` 与 `412` 都由本地元数据回答，不因此回源；前置条件通过后继续按命中或 Range 处理。
- 转码交付时 `ETag` 已是弱验证器，`If-None-Match` 仍按弱比较命中，`If-Match` 因弱验证器不匹配而返回 `412`。

## Range

- 缓存中已有完整 200 条目、且协商出的目标编码就是已存编码（无需转码）时，relay 自行回答单段与多段 Range，并从缓存内容构造 `206`。
- 多段 Range 最多 32 段；超过时忽略 Range 并返回完整 200，避免重复分段造成读取与带宽放大。
- 不可满足的范围返回 `416`。
- 带 `If-Range` 的请求只在验证器与当前 variant 一致时按缓存回答，否则先完整重验。`If-Range` 只接受强 `ETag` 或 `Last-Modified` 日期；弱 `ETag` 视为不匹配，退回完整响应。
- **需要转码才能满足编码偏好的命中上收到 Range 时回源**：不把范围套在转码流上，而是把原请求（含 `Range`）透传上游，响应直接交付且不写回缓存。
- Range 请求未命中缓存时透传上游。`206` 不写缓存。上游忽略 `Range`、返回完整 `200` 的 GET 响应时，可以照常建立条目，但必须写一行 warning 日志记录这次 Range MISS 实际拿到的是完整响应（客户端收到的仍是 `200`）。
- 求值顺序：前置条件（`304` / `412`）→ `If-Range` 校验 → 范围计算与 `206` 构造。

## 响应控制头

- 通用响应控制头统一为 `X-MDB-Relay`，取值形如 `Cache-Control`：逗号分隔的 `keyword` 或 `keyword=value`。
- 当前定义三个关键字：
  - `no-store`：不要将该响应写入 relay 缓存，body 与元数据都不写。APT 的可变 `dists/` 索引（不含 `by-hash/`）统一携带它，避免 Release 与 Packages 代次不一致。
  - `no-vary`：该响应的 URL path 足以唯一表示资源，不按 `Vary` 划分 variant；覆盖 `Vary: *`，但只作用于 variant 划分（见「变体」）。
  - `ttl=<seconds>`：上游既无 `s-maxage`/`max-age` 也无 `Expires` 时的兜底生命周期，非负整数秒，缺省 `0`；`0` 表示不提供兜底，不是“永不过期”，也不覆盖上游给出的新鲜度。
- 关键字只作用于携带它的那个响应。
- 非 `passthrough` 链路中，relay 只从上游响应里读取该头，交付客户端前移除。`passthrough: true` 的响应不进入响应控制处理：不解析、不移除，按上游原样交付。
- 入站请求上的同名头没有语义：请求未携带 `X-MDB-Relay-Host`（直接来源）时，relay 与 Worker 的 mirror 层都剥除它；响应上的 `X-MDB-Relay` 不受影响。
- 未知关键字忽略；标准 `Cache-Control: no-store` 与 `X-MDB-Relay: no-store` 同效。
- 响应决定只走响应头：`no-store`、`no-vary`、`ttl`、`no-transform`、实际新鲜度、实际编码与摘要都是 relay 拿到响应后才能判断的事实，不进入 mirrors manifest 的 policy。`no-transform` 直接按标准 `Cache-Control: no-transform` 识别；将来需要新增响应侧控制时按需加入 `X-MDB-Relay` 关键字。
- 旧名 `X-MDB-Relay-No-Store` 不再使用。

## 响应头存储

- 只保存端到端头（end-to-end），不保存 hop-by-hop 头（`Connection` 及其列出的 token、`Keep-Alive`、`Transfer-Encoding`、`Upgrade`、`Proxy-*`、`TE`、`Trailer`）。
- 不保存 `Set-Cookie`、`WWW-Authenticate`、relay 控制头 `X-MDB-Relay`，也不保存原始的 `Content-Length`。
- 保留验证器（`ETag`、`Last-Modified`）、内容编码（`Content-Encoding`）、新鲜度（`Cache-Control`、`Expires`）、`Digest`、`Content-Type`，以及上游确实给出的表述协商头（如 `Vary`）。这些是**已存响应头**，只用于交付、条件重验与范围求值；`Vary` 另外派生命中比较字段集合，但两组字段分开保存，不共用同一清单。以上均为**存储值**，即上游原样；转码交付时按「流式转码」改写或移除，不影响存储。
- 交付时重建长度与范围头：未转码交付按本地 body 大小重建 `Content-Length`；转码交付不发送 `Content-Length`（流式结果长度未知）。Range 由 `Content-Range` / `Accept-Ranges` 配合本地内容重新构造。
- `Age` 不存储也不回放，按存储时刻到交付时刻重建。
- 已存头集与原始上游头的差异只在必要时记录，不承诺逐字节保留上游响应。

## 并发

- M2 不做 singleflight。同一 URL 与 variant 的并发 MISS 各自回源、各自流式交付，不合并请求、不互相等待、不共享中间状态。
- 存储写入按真实哈希幂等：多个回源者得到相同 body 时只保留一份对象，重复写入既不报错也不产生第二份。
- variant 元数据以原子替换落盘：并发 MISS 结束后该 variant 的元数据整体是某一次回源的结果，不出现多次回源拼接出的混合状态。

## 故障处理

- 响应开始前：缓存读取或元数据解析失败时记录日志并回退上游，客户端只看到正常的上游响应，不看到缓存故障。
- 确定缺失（本地 body 不存在、共享存储明确无此对象、元数据指向不存在的 body）按 MISS 处理，并修复或删除该元数据；瞬态错误（网络、权限、I/O）不删除元数据。
- 写入失败只影响缓存本身：上游响应照常交给客户端，不因写 body 或写元数据失败改变状态码或中断流。
- 响应开始后不重试：body 传输中断即按时中断，缓存侧不换源续传，也不尝试接续同一个响应。

## 测试端点

- `relaytest/<casename>` 是 **Worker 侧的受控测试上游**，不是 relay 的缓存入口。它按用例名返回确定的状态码、响应头与 body（`ETag`、`Cache-Control`、`Vary`、Range 响应等），使每条缓存分支都能被确定性地观测。
- relay 对 `/relaytest` 自身的请求与内部端点一样一律旁路：该路径既不查缓存也不写缓存。
- 自动化测试不让 relay 识别用例名，而是准备临时 mirror/上游配置，把目标路径指向对应 case 的 URL，再以普通客户端身份请求 mirror 路径，从外部观测命中、重验、MISS 与旁路行为。
- 这 8 个 case 都支持 HEAD：HEAD 返回与同路径 GET 相同的状态码与响应头，不带 body。
- 固定 case 清单（名称即路径段，行为按上一轮定义）：
  - `fresh` — 长 `max-age` 的 200 + `ETag`：第二次请求不发上游，直接命中。
  - `revalidate` — `no-cache` + 验证器：每次命中条件请求，`304` 只更新元数据；改内容后 `200` 替换 body。
  - `ttl` — 不给 `Cache-Control` / `Expires`：靠 `X-MDB-Relay: ttl` 兜底，TTL 内命中、TTL 后重验。
  - `vary` — 按 `Vary` 字段产生多个 variant：不同取值互不串味，同一取值稳定命中；`Vary` 含 `Accept-Encoding` 时不因编码新增 variant。
  - `encoding` — 只存 `identity`：覆盖显式编码、qvalue 平手、通配 `*`、`identity;q=0`、缺省 `Accept-Encoding`、无可接受编码回源与 `Cache-Control: no-transform` 回源；本地转码交付且不发上游；同一命中上的 Range 回源。
  - `range` — 完整 200 缓存：单/多段 `206`、`416`、`If-Range` 本地回答；未命中时透传且不写 `206`。
  - `no-store` — `X-MDB-Relay: no-store`、`Cache-Control: no-store`、`private`、`Vary: *` 都不产生缓存写入。
  - `digest` — OCI digest 路径 / 带 `Docker-Content-Digest` 等摘要头的条目：编码不匹配回源而非转码，摘要头原样保留。
- 临时 mirror/上游配置的构造、下发与清理由测试自行负责，属于实现细节。

## 实现

- bbolt 为单个 URL 保存多组 variant 记录：命中比较字段（字段名 + 规范化取值，逐字段匹配，不存 variant 指纹哈希）、已存响应头、body 的 SHA-256 与实际大小、状态码、写入时刻与新鲜度边界。匹配字段与交付/重验响应头分开保存；同一 URL 与同一比较字段组合的写入整体替换该记录。键模型由实现决定。
- 每条记录都由一次完整 200 的 GET 产生，因此不存在“有元数据、无 body”的记录；HEAD 只读记录，不新增、不改写、不刷新新鲜度（`304` 的元数据更新除外）。
- 转码是交付期行为，不进入元数据：元数据只描述已存字节的编码与摘要。codec 固定为标准库 `gzip` / `zlib`、`andybalholm/brotli` 与 `klauspost/compress/zstd`，解码到 `identity` 后再编码目标。
- 缓存元数据只保存在**本机 bbolt**，不写入共享 RustFS；共享存储只承载 body 与摘要指针。
- body 被 LRU 驱逐、显式删除或 `relay prune` 清理时，同步删除引用它的缓存元数据。
- 他端删除共享 body 后，本端按 MISS 回源，回源成功后重建元数据。
- Release 等索引中的 MD5 被忽略，不作为内容身份依据。
- 缓存默认开启：`CACHE_DIR` 非空、请求解析到 mirror、方法为 GET/HEAD 且未被 `passthrough` 匹配时进入缓存链路；policy 中没有缓存字段。
- MISS 临时文件写 `<CACHE_DIR>/.tmp`，与最终本地对象同文件系统以支持原子 rename；不设独立 TMP_DIR。

## 验收

1. 四类结果可独立判定：新鲜本地命中、需重验的本地命中、存储缺失回退、完整 MISS。
2. 新鲜命中不产生任何上游请求。
3. `no-cache` 命中每次都重验，`304` 只更新元数据、不重写 body。
4. 上游无验证器时能完整重新获取；`304` 之外的确定缺失会删除条目。
5. `X-MDB-Relay: no-store` 与 `Cache-Control: no-store` 都不产生任何 relay 缓存写入；`private` 与 `Vary: *` 同样不写。
6. 同一 URL 的不同 variant 只按上游 `Vary` 所列请求头的规范化取值区分，`Accept-Encoding` 不参与匹配、`Content-Encoding` 不建立 variant；同一 URL 与同一比较字段组合写入时整体替换；编码偏好按完整 `Accept-Encoding` qvalue/通配/identity 协商，平手按已存编码 → `zstd` → `br` → `gzip` → `deflate`，显式编码优先隐式 `identity`，解码到 `identity` 再编码目标；转码不新增 variant、不写回缓存。
7. MISS 只需一次上游 body 传输即可同时完成客户端交付、双哈希计算和存储提交。
8. Range、`416` 与 `If-Range` 按完整缓存正确回答；协商目标编码等于已存编码才本地范围，需转码的 Range 回源；Range 未命中时透传且不写 `206` 缓存。
9. `CACHE_DIR` 为空时任何请求都不查不写缓存；非空时未被 `passthrough` 匹配的已解析 mirror GET/HEAD 默认进入缓存链路，`passthrough: true` 匹配的路径、内部端点与 `/relaytest` 既不查询也不写入缓存，也不做响应控制与转码。
10. body 被驱逐或被他端删除后，命中降级为 MISS 并能自愈。
11. 同一 variant 的并发 MISS 各自回源且都能完整交付，最终只留一份 body 与一份自洽的 variant 元数据。
12. 响应开始前的缓存故障回退上游并留日志；写失败不改变上游响应；响应开始后不重试。
13. 存储的响应头不含 hop-by-hop、`Set-Cookie`、`WWW-Authenticate`、`X-MDB-Relay` 与原始 `Content-Length`，未转码交付按实际内容重建长度，并重建 Range 与 `Age`。
14. 本地命中按 `If-None-Match` / `If-Modified-Since` / `If-Match` / `If-Unmodified-Since` 回答 `304` / `412`，全程不触发上游请求；`If-None-Match` 弱比较、`If-Match` 强比较，`If-Range` 只认强验证器或日期。
15. 转码交付移除 `Content-Length`、`Content-MD5`、`Digest`、`Docker-Content-Digest`，强 `ETag` 转弱；未转码交付的头集保持不变；OCI digest 路径、带摘要头的条目、`Cache-Control: no-transform` 或无可接受编码时回源而非转码。
16. Worker 的通用 middleware 把 `Vary` 原样交付，带 `Vary` 的响应不由 Worker 缓存保存而由 relay 接管，全程没有隐藏头；当前没有任何 per-mirror 缓存调优配置进入生产。
17. GET 与 HEAD 共用同一 URL 与 variant 空间：新鲜 HEAD 零上游请求、按 GET 元数据回答且不发 body、不求值 Range；冷 HEAD 回源且不创建无 body 条目；已有 body 的 stale HEAD 按条件重验，`304` 只更新元数据，`200` / `404` / `410` 使该 variant 失效且不凭 HEAD 建立新 body，其余状态不写；只有完整 200 的 GET 建立条目，非 GET/HEAD 不写。
18. 同一 URL 下字段集不同的多条记录同时匹配时按歧义回源重验，不猜选其中一条；恰好一条匹配才本地命中。
19. `X-MDB-Relay: no-vary` 的响应不按 `Vary` 划分 variant，同一 URL 只有一个 variant；覆盖 `Vary: *`，但仅限 variant 划分，不绕过 `no-store`、`no-transform`、摘要禁止转码与 `passthrough`；`Vary` 仍原样交付。
20. HEAD 的编码协商、`no-transform` 与摘要禁止转码规则与 GET 相同；需要转码时只改交付头、不带 body，冲突回源，且不求值 Range；8 个 `relaytest` case 都能以相同状态码与响应头回答 HEAD。
21. Range 未命中且上游忽略 `Range` 返回完整 200 时允许建立缓存条目，并写一行 warning 标明这次 MISS 拿到的是完整响应；`206` 不写缓存。
22. 上游既无 `max-age` 也无 `Expires` 时 `X-MDB-Relay: ttl` 生效，缺省 `0` 不兜底；上游给出新鲜度时 `ttl` 被忽略。
23. 请求未携带 `X-MDB-Relay-Host` 时，relay 与 Worker 都剥除入站请求上的 `X-MDB-Relay`；响应上的 `X-MDB-Relay` 不受影响。

## 状态

已完成（M2，2026-11-23；未部署验证）。实现要点：`X-MDB-Relay` 控制头定义 `no-store`、`no-vary` 与 `ttl=<seconds>`，只读上游、交付前移除（`passthrough` 响应不解析也不移除）、未知关键字忽略；请求未携带 `X-MDB-Relay-Host` 时 relay 与 Worker 都剥除入站请求上的 `X-MDB-Relay`，响应控制不受影响；响应决定只走响应头（`X-MDB-Relay` 或标准 `Cache-Control`），不进入 policy；`CACHE_DIR` 非空即启用缓存，已解析 mirror 的 GET/HEAD 默认尝试缓存，完整 200 GET 默认允许写，`passthrough: true` 是唯一的策略级跳过；仅 GET/HEAD 且内部端点与 `/relaytest` 旁路；新鲜度来自上游 `s-maxage` / `max-age` / `Expires`，三者皆无时由 `X-MDB-Relay: ttl` 兜底（缺省 0）；客户端 `Cache-Control: no-store` 跳过查询与写入，`no-cache` / `max-age=0` 强制重验；`no-cache` 存但每次重验；`no-store` / `private` / `Vary: *` 不存，`no-vary` 覆盖 `Vary: *` 且仅限 variant 划分，首批 11 个 `createAptMirror` 挂载注入 `no-vary`；过期后条件重验，`304` 更新元数据、`200` 替换、确定缺失删除；无验证器完整重取；variant identity 只是上游 `Vary` 所列请求头的规范化取值（`X-MDB-Relay: no-vary` 时忽略 `Vary`、单一 variant，`Vary: *` 亦然），`Accept-Encoding` 不参与匹配、`Content-Encoding` 不建立 variant，交付/重验响应头单独保存；同一 URL 与同一比较字段组合原子替换；编码偏好按完整 `Accept-Encoding` qvalue/通配/identity 协商，平手按已存编码 → `zstd` → `br` → `gzip` → `deflate`，显式编码优先隐式 `identity`，统一解码到 `identity` 再编码目标，库为标准库 `gzip` / `zlib` + `andybalholm/brotli` + `klauspost/compress/zstd`；转码不改写 variant、不写回缓存，仅改交付头（去掉 `Content-Length`、`Content-MD5`、`Digest`、`Docker-Content-Digest`，强 `ETag` 转弱）；OCI digest 路径、带内容摘要头的条目、`Cache-Control: no-transform` 与无可接受编码均回源；需转码的 Range 回源，编码一致才本地 Range；本地命中按 `If-*` 回答 `304` / `412`，`If-None-Match` 弱比较、`If-Match` 强比较，`If-Range` 只认强 `ETag` 或日期；不做 singleflight，并发 MISS 各自回源、Put 幂等、variant 元数据原子替换；响应开始前缓存故障回退上游，写失败不影响交付，开始后不重试；存储端到端头并排除 hop-by-hop 等，交付重建长度、Range 与 Age；Worker 统一通用 middleware 保留 `Vary` 交付、不改 Cache API 存储规则（带 `Vary` 的响应由 Worker 缓存旁路、relay 接管），不加隐藏头，当前不做 per-mirror 生产调优；`relaytest` 是 Worker 侧受控测试上游，测试通过临时 mirror/上游配置指向 case；元数据只在本机 bbolt，body 回收同步清元数据；完整 200 缓存支持单/多 Range、416、If-Range；Range 未命中透传且不写 206；`relaytest` 固定 8 个 case：`fresh` / `revalidate` / `ttl` / `vary` / `encoding` / `range` / `no-store` / `digest`；GET 与 HEAD 共用同一 URL 与 variant 空间，只有完整 200 的 GET 建立条目，HEAD 从不建立条目（新鲜命中原样由 GET 元数据回答且不发上游，冷 HEAD 回源且不留条目，已有 body 的 stale HEAD 按条件重验——`304` 只更新元数据，`200` / `404` / `410` 使 variant 失效但不凭 HEAD 建新 body，其余状态不写）；其余方法不写缓存；HEAD 的编码协商、`no-transform` 与摘要禁止转码规则与 GET 相同，需转码只改交付头不带 body、冲突回源，HEAD 不求值 Range，8 个 `relaytest` case 均支持 HEAD（同状态/头无 body）；同一 URL 下字段集不同的多条记录同时匹配视为歧义并回源重验；Range MISS 时上游忽略 Range 返回完整 200 可写缓存并记一行 warning，`206` 不写；M2 不新增 HEAD 探测与 prefetch 的 policy 字段，prefetch 归 M3。

后续：prefetch 与遥测归 M3/M4；policy 边界与字段见[Relay 策略下发](relay-policy.md)。实现细节（bbolt 键模型、`relaytest` 临时配置的构造与清理）不在本功能语义内。2026-11-23 已通过一次性 Docker RustFS 1.0.6 真实 S3 联测（未加入 CI）；部署验证尚未进行。
