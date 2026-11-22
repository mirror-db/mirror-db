# ROADMAP

## M1：独立哈希存储

- **状态**：已完成（2026-11-22；含 S3 分片与离线 prune，真实 RustFS 联测留待部署）
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

- **状态**：未开始
- **目标**：将预判、探测、命中、MISS 和 no-store 接入 relay，并通过 mirrors 接口下发可组合的缓存策略。
- **进入条件**：M1 完成；请求缓存与 policy 合并、协议兼容边界确认。
- **包含的 features**：[Relay 请求缓存](features/relay-request-cache.md)、[缓存策略下发](features/cache-policy.md)
- **不包含**：仓库元数据递归解析、自动 prefetch、遥测接口。
- **完成条件**：
  1. HEAD/GET 探测、identity 对照、缓存交付和流式 MISS 写入形成完整链路。
  2. 存储缺失能回退上游，`X-MDB-Relay-No-Store` 不产生缓存写入。
  3. policy 按字段合并、last match win 与 `exit` 语义生效，新旧语义兼容方式明确。
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
