# 哈希存储

## 目标

提供独立的内容寻址存储组件，以真实 sha1/sha256 保存不可变 body，支持本地目录和可共享的 S3 后端，并按容量触发 LRU 回收。

## 非目标

- 不保证共享 S3 中的对象持续存在，不维护跨 relay 引用计数。
- 不通过 LRU 或 cache-control 主动过期保证内容新鲜度。
- 不对 `.tmp` 临时文件或 bbolt 索引套用哈希存储容量。

## 交互

- 调用方按算法和哈希读取内容；对象被其他 relay 删除或无法读取时，按缓存 MISS 回退上游。
- 所有 relay 域名公开提供只读 GET/HEAD `/hash-content/sha1/<hash>` 和 `/hash-content/sha256/<hash>`；不支持通过该接口写入。
- 对象按真实内容哈希全局寻址，不按 mirror 分键；同一 body 只存一份，同时建立 SHA-1 与 SHA-256 两个索引。
- 不保证对象在响应传输途中存活，不引入本地或跨实例引用计数。

## 实现

- `CACHE_DIR` 始终是本地工作目录，也是缓存启用开关；为空则不构造存储、不注册 `/hash-content`，非空则启用。
- MISS 先写 `<CACHE_DIR>/.tmp`，同时计算真实 SHA-1 与 SHA-256 后提交哈希存储，临时文件与最终本地对象同文件系统以支持原子 rename。
- 不再设置独立的 TMP_DIR；bbolt 索引固定为 `<CACHE_DIR>/.index.db`。
- 启用缓存时必须配置 `CACHE_CAPACITY`（例如 `128GiB`）表示可用容量；`CACHE_MAX_SIZE` 是缓存的总字节预算和 LRU 上限，可留空，默认取 `CACHE_CAPACITY` 的 10%，显式配置不得超过 `CACHE_CAPACITY`。容量值支持带二进制单位的字节数。
- 单个 body 大于 `CACHE_MAX_SIZE` 时跳过缓存，不驱逐其他对象。
- 未配置 `S3_BUCKET` 时 body 只存本地；配置后 body 只存 RustFS，本地仅保留 `.tmp` 和 bbolt。S3 另配置 `S3_ENDPOINT`、可选 `S3_PREFIX` 和 `S3_REGION`（默认 `us-east-1`），凭据走 AWS 标准环境变量或 SDK 凭据链。
- 仅对哈希存储的逻辑内容字节做容量管理，超限触发严格 LRU；下载中的临时文件不计入该容量。
- 共享 S3 只提供机会型复用：各 relay 按自身容量独立维护 LRU，任何实例均可删除共享对象，其他实例不得假定对象完整或仍然存在。
- 不使用 cache-control 主动过期；被动回收统一由 LRU 负责。
- 使用 `google/go-lru` 维护按字节加权的严格 LRU，relay 的 Go 工具链同步升级到 1.26；禁用该库的 runtime-pressure 淘汰。
- 使用 bbolt 持久保存各实例自己的容量记账、双摘要别名和 LRU 顺序；启动时校验并协调索引与后端实际对象。
- RustFS 后端采用其官方推荐的 AWS SDK for Go v2，使用 path-style 寻址。
- LRU 主键统一为 SHA-256，每条记录包含 body 大小及两个摘要；SHA-1 仅作无权重别名，避免同一 body 被重复计算容量。
- 本地 body 以 SHA-256 分片路径保存；S3 body 与 SHA-1/SHA-256 指针对象都按前两字节、再两字节切分路径，避免底层巨型目录。指针对象使其他 relay 能通过任一算法发现同一 body，别名不计内容容量。
- 本地启动时后台协调索引与文件；S3 不全量列举共享 bucket，已有索引在读时校验，其他实例写入的对象在成功读取后收养进本实例 LRU。

## Prune

- 提供独立离线命令 `relay prune`，复用 relay 配置打开 CACHE_DIR；bbolt 文件锁阻止它与运行中的 relay 同时使用同一目录。
- prune 扫描后端并删除本机 bbolt 未索引的 body 和别名，同时清理失效索引与 pending。共享 S3 通过 ListObjectsV2 分页枚举当前 prefix，不改变正常读取/启动 Reconcile 的“不 LIST”规则。
- **共享 bucket 上这是破坏性缓存清理**：其他 relay 写入但本机未索引的对象也会删除，包括在线节点的对象；只能在独占 bucket 或确认其他节点不再需要这些缓存时运行。删除后仍可回源，不是原始数据丢失。
- 命令输出扫描、删除与失败计数；存在失败时返回非零。

## 代码边界

- 新增扁平包 `tools/relay/hashstore`，内部仅以后端小接口隔离本地与 RustFS 实现。
- relay 配置负责解析上述变量并在启动时 fail fast；server 只在缓存启用时注册公开只读 handler。
- M1 提供存储写入 API 和 handler 测试，但不接入 reverse-proxy MISS 链路；该链路属于 M2。
- Go `go.mod`、CI 与 Dockerfile 同步升级到 1.26。

## 验收

1. 本地目录和 S3 均可按真实 sha1/sha256 写入、读取和删除内容。
2. 达到配置容量后按 LRU 回收，同一 body 只存一份且两个哈希索引均可读取。
3. S3 对象缺失或读取失败可被调用方识别并转为上游回退，不作为持久性错误。
4. CACHE_DIR、`.tmp`、bbolt 索引与容量配置规则符合上述边界。
5. `/hash-content` 能按算法与哈希返回对应内容。
6. S3 body 与别名均采用分片路径；离线 `relay prune` 能清理本机未索引对象并正确报告失败。

## 状态

已完成实现与单元/竞态验收，包含 S3 分片路径和离线 prune；真实 RustFS 服务联测留待部署。
