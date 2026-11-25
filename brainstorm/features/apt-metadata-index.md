# APT 元数据索引

## 目标

在 CF mirrors 递归解析 APT 元数据，为不支持目录列出的仓库提供目录能力；relay 使用同类元数据建立资源哈希预判。

## 非目标

- 不递归下载 `.deb` 等内容 body，不构建完整 APT 仓库镜像。
- 不把 Release/InRelease 当作包体哈希来源。

## 交互

- 对托管在 S3 等不支持 listing 的 APT 仓库，CF mirror 根据元数据引用合成可浏览或可遍历的目录；已有原生 autoindex 的 mirror 保持原行为。
- Worker 统一解析 APT 元数据并通过固定端点 `/<mount>/.mdb/apt-index.json` 输出规范化索引，relay 消费同一结果建立资源路径到预期哈希的映射；不维护 TS/Go 两套解析器，也不为此扩展 manifest。
- 首批覆盖 `syncthing-pkgs`、`hashicorp`、`mise`、`opentofu`、`github-cli`；`pigsty` 的多布局 passthrough 不在本期改造。
- 元数据更新后，哈希预判切换到新引用；旧内容交由哈希存储 LRU 回收。
- 对索引声明的不可变包文件，URL 缓存 MISS 时可按预期 SHA-256 或 SHA-1 查找已有 body 并直接交付；使用中的索引必须新鲜，body 缺失则普通回源。可变 Release/Packages 不得凭哈希预判绕过回源。

## 实现

- 从 Releases/InRelease 递归解析其引用的元数据，并继续解析 Packages/Sources，得到包文件路径、大小与 SHA-256/SHA-1。
- Release/InRelease 列出的全部文件都进入目录和元数据哈希表，但后台只继续下载 Packages/Sources；Translation、Contents、dep11 等只登记，不主动下载。
- Release/InRelease 提供 Packages 等元数据的校验和，不直接提供 `pool/` 中 `.deb` 的哈希。
- 目录可展示元数据和 Packages/Sources 引用的包文件，但递归仅限仓库定义的元数据引用，不扩展为通用 HTML 链接抓取。
- 目录按需合成，不启动全仓库遍历任务；引用包文件不等于下载或预热包体。
- relay 的规范化索引与映射只存内存，重启后等待真实请求重新发现，不做冷启动扫描；Worker 每次从同一代 Release 固定引用，优先 by-hash，否则校验大小和哈希，任何所需 Packages/Sources 失败都不发布新代。Worker 与 relay 都原子替换快照。
- 新鲜索引是其覆盖路径的权威来源；SHA-256 优先、SHA-1 回退。旧 variant 哈希不符不得命中，索引刷新失败或 body 缺失时普通回源；哈希直接交付不另建长期 URL variant，freshness 不超过索引。
- Worker 更新失败可继续提供旧目录；relay 不得用 stale 索引直接交付，旧 body 仍只由 LRU 回收。
- Release 继续复用 `apt-parser`；Packages/Sources 使用按行流式 stanza 解析。优先 gzip、其次 plain，本期不支持仅提供 xz/bz2/zstd 的索引。
- 单个 Packages/Sources 解压后最多 8 MiB；单 mirror 每代最多解析 64 MiB、保留 5 万个去重文件。超限、路径逃逸或同路径摘要冲突拒绝整代更新；空索引是合法结果。
- 固定端点返回一个 `schema: 1` JSON：mirror、生成/到期时间、各 suite 的 Release 身份、Release 元数据文件及去重包文件的路径/大小/SHA-1/SHA-256；不含无关包字段或绝对上游 URL。首期不做分页、R2、Cron 或 NDJSON。
- 目录在 HTML、`?format=json` 与只读 WebDAV（PROPFIND Depth 0/1）三种模式下都读同一代索引；未知目录返回 404/502，不渲染为空目录。
- 稳定内容 ETag：校验值只覆盖 schema/mirror/suites/files，`generated_at` / `expires_at` 留在响应体中；上游元数据未变时重建不改 ETag，`If-None-Match` 可稳定得到 `304`。
- 代内已确认的签名同辈（`InRelease` / `Release` / `Release.gpg`）可以继续透传上游，即使该代只验证并列出其中一个，避免隐藏 apt 需要的回退名。
- Release/InRelease 加入 `X-MDB-Relay: prefetch`；`Packages`/`Sources` 与 `pool/` 包体不加。
- 包体请求仍按源方法透传：HEAD 到上游也是 HEAD，回读缓存产生 GET body 时在交付前丢弃；`Range` 仍不向 APT `pool/` 上游转发。

## 验收

1. 不支持原生 listing 的目标 APT 仓库可由 CF mirror 提供元数据目录。
2. relay 能从同一组元数据建立资源—哈希映射，并区分元数据哈希与包体哈希。
3. 递归解析不会仅因引用关系自动下载包体。
4. 元数据更新后，新请求使用更新后的哈希预判。

## 状态

已完成（M3，2026-11-23；未部署验证）。五个 snapshot 挂载为 `syncthing-pkgs`、`hashicorp`、`mise`、`opentofu`、`github-cli`：Worker 流式解析 Release → Packages/Sources，合成三模式目录并发布固定 schema 1 索引；解析上限为单索引 8 MiB、每代 64 MiB（按实际内容去重后计）、5 万个去重文件，索引组并发 4，仅支持 gzip/plain，永不下载包体。2026-11-23 对五个真实上游完成一次构建与稳定 ETag 验证（`hashicorp` 一代去重后解析 15 个索引、约 5.92 MiB）；该验证未加入 CI，也未做部署验证。
