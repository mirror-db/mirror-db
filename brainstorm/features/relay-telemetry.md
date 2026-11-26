# Relay 遥测

## 目标

为 relay 增加遥测接口，用于观察缓存、上游回退、存储回收和 prefetch 的运行情况。

## 非目标

- 当前不包含外部指标存储、告警或展示系统。
- 不在指标与鉴权边界确认前假定具体接口格式。

## 交互

- `GET|HEAD /.mirror-relay/metrics` 由 OTel Prometheus exporter 提供标准 Prometheus 文本，供 Prometheus/VictoriaMetrics 抓取。
- 使用独立长期 Bearer token（`metrics.token` / `METRICS_TOKEN`），与 JWT 密钥分离；token 未配置时端点关闭，不复用 `secret.key`，不新增监听端口。
- 代码使用独立 OpenTelemetry MeterProvider，不设置全局 provider；首期不加入 Go/process collector。

## 实现

- 指标使用 OTel 名称和单位，由 exporter 生成 Prometheus 名称；只允许 mirror/result/reason/backend/coding 等有界属性，禁止 URL、path、digest、variant、host、客户端 IP、ETag 和错误文本。
- 镜像请求统计缓存结果、实际客户端字节和少量固定 bucket 延迟；上游字节按 proxy/cache/prefetch/manifest 分开。内部端点与 metrics 自身不混入镜像流量。
- 实际写出的部分响应即使客户端中断也计数；ResponseWriter 包装必须保留 Flush/Hijack/ReaderFrom/Unwrap。
- 请求指标为 `relay.requests{mirror,outcome}` 与 `relay.response.bytes{mirror,outcome}`；outcome 固定为 hit/predicted/revalidated_304/revalidated_200/miss_stored/miss_unstored/bypass/error。
- 上游指标为 `relay.upstream.bytes{mirror,source}`；缓存回退使用固定 reason，存储导出 objects/variants/bytes/capacity gauge 与 capacity eviction 次数/字节 counter。
- Prefetch 导出 leases/running/backoff gauge、runs/refusals counter 与运行时长 histogram；runs 的 result 固定为 success/not_modified/not_found/no_next/error。首期不展开 Range、转码和 S3 alias 内部指标。
- `relay.request.duration{mirror}` bucket 为 0.01/0.05/0.1/0.25/0.5/1/2.5/5/10/30/60/120 秒；`relay.prefetch.run.duration{kind}` 为 0.1/0.5/1/2.5/5/10/30/60 秒。请求时长不叠加 outcome。
- token 未配置时不注册路由；错误 token 返回 401 并常量时间比较，其他方法 405。Resource 仅 service.name=mirror-relay 与可获得的 service.version。
- Counter 仅进程内累计，重启归零；Gauge 在 scrape 时读取快照，不在热路径扫描任务或执行存储 I/O。采集失败不影响代理，不改变 S3 alias 错误语义。

## 验收

1. 配置 token 后 OTel Prometheus exporter 正确输出上述低基数指标，未配置时端点关闭，鉴权失败不泄露指标。
2. 缓存结果、实际上下游字节、存储占用与 capacity eviction、prefetch 状态及结果与真实行为一致，后台流量不计入客户端流量。
3. 流式/upgrade/Range/客户端中断不因包装失效；计数不包含 URL、digest 等无界属性，重启后 counter 归零。
4. 完整 Go race、vet、build、依赖 tidy 与端点/指标契约测试通过。

## 状态

设计已确认，待实现。
