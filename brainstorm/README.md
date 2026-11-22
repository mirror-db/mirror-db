# Relay 本地缓存与元数据索引

## 项目目标与范围

为 relay 增加支持本地目录与 S3 的内容寻址缓存、容量驱动的 LRU 回收和仓库元数据哈希预判；CF mirrors 为不支持目录列出的 APT 仓库合成目录，并通过 mirrors 接口下发探测与 prefetch 策略。

递归 prefetch 仅覆盖元数据，不自动下载包体或构建完整仓库镜像。

## 当前状态

M1 已完成：本地/RustFS 内容寻址存储、严格字节 LRU、bbolt 持久索引和公开只读 `/hash-content` 均已实现并通过竞态测试。M2 尚未开始。

已确认：共享 S3 对象可被独立驱逐，缺失时回退上游；缓存不采用 cache-control 主动过期；`X-MDB-Relay-No-Store` 表示不要写入 relay 缓存；policy 按字段合并、last match win，并支持 `exit`。

M1 复用 `google/go-lru`、bbolt、AWS SDK for Go v2 与 `docker/go-units`；统一使用 CACHE_DIR，不再设置 TMP_DIR。

后续待明确：prefetch 调度参数、遥测指标与鉴权，以及 policy 协议兼容方式。

## 文档索引

- [ROADMAP](ROADMAP.md)：分阶段成果、进入条件与完成条件。
- [哈希存储](features/hash-storage.md)：本地/S3 内容寻址存储、容量与哈希访问接口。
- [Relay 请求缓存](features/relay-request-cache.md)：预判、探测、命中、MISS 与 no-store 流程。
- [缓存策略下发](features/cache-policy.md)：HEAD 能力、prefetch 配置及 policy 合并语义。
- [APT 元数据索引](features/apt-metadata-index.md)：CF 目录合成与 relay 哈希预判。
- [元数据 Prefetch](features/metadata-prefetch.md)：按策略自动更新 Releases、PyPI 主目录等元数据。
- [Relay 遥测](features/relay-telemetry.md)：遥测接口的待定义边界。
