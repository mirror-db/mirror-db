# Worker 侧待办（relay 审计发现，本轮未改动）

来源：`tools/relay/` 全面 review。前提：整个 mirror 项目开放，**除证书外一切资源不设防**。
relay 利用 `X-MDB-Relay-Host` 把「用户实际访问的域名」传给 Worker，
Worker 生成的 URL 必须指回该域名，否则用户会离开 relay 直连 Worker。

## P0 — 绝对 URL 指向 Worker 而非 relay

### 1. `server/mirrors/npm/index.ts:25` — tarball URL 改写绕开 relay

```ts
const origin = new URL(req.url).origin;   // ← Worker 自己的域名
```

`request.url` 在 CF 侧永远是 `https://<worker>/npm/...`，relay 域名只存在于
`x-mdb-relay-host`。结果：npm/yarn/pnpm 拿到的 tarball 地址是
`https://mirs.uk/npm/...`，用户包管理器**直连 Worker 下载**，完全绕过 relay
（以及 relay 的抗 DPI 通道）。

改用 `relayInfo(req).origin`。注意 `rewriteBody` 的 callback 拿到的是
**原始 request**，`relayInfo` 正好接受 `Request`，可直接调用。

### 2. `server/pkgs/apt/proxy.ts:82` — 目录补斜杠 301 跳回 Worker

```ts
const url = new URL(request.url);          // ← origin 是 Worker
url.pathname += "/";
return Response.redirect(url.toString(), 301);
```

APT 客户端访问 `/<repo>/dists` 被 301 到 `https://mirs.uk/...`，此后所有请求
离开 relay。改用 `relayInfo(request).origin` 重建 URL。

顺带：这里是 **301 永久重定向**，客户端会长期缓存。若 relay 域名后续变更，
用户端会持续跳到旧地址。建议与 #4 一起统一改 308。

### 3. 其余已确认「正确」的位置（无需改动，仅登记）

- `server/mirrors/charts/index.ts:40` — 已用 `relayInfo(request).origin`，正确。
- `server/scripts/oci.ts:92` — 已用 `relayInfo(c).baseDomain`，正确。
- `server/mirrors/pypi/index.ts` — Simple 页面改写成**相对路径**，与 origin 无关，正确。
- `server/mirrors/web-mirror.ts:48` / `apt-mirror.ts:43` — 内部 `new Request` 只用
  `url.origin` 作为同源占位、随后被 `WebListProxy` 按相对路径重新拼接，不产生对外
  URL，正确。但**前提是它们不丢 `x-mdb-relay-host`**：`new Request(url, request)` 会
  复制全部头，当前正确 —— 后续任何人把 inner request 改成 `new Request(url, {...})`
  就会静默丢失 relay 信息，建议加注释固化。

## P1 — relay 相关契约

### 4. HTTP → HTTPS 重定向用 301，方法不保真

relay 侧 `httpRedirectHandler` 已改为 **308**（本次改动）。Worker 若有同类
重定向，应对齐 308；`301` 只保留给 `GET /`。

### 5. `X-MDB-Relay-Host` 的信任与多级链

契约（本次固化在 relay 侧）：

- relay **原样透传**客户端带来的 `x-mdb-relay-host`，仅当缺失时补上自己的
  base domain → 多级链路（A→B→Worker）天然保留**最靠近用户的那一级**。
- relay 对**自己发起**的上游请求（`/api/mirrors`、`/speedtest?passthrough`,
  `/cert-pack`）**不设**该头 —— 这些是机器对机器请求，转发的 relay 域名对它们
  没有意义且可能诱导 Worker 生成用户不可达的 URL。

Worker 侧需要保证：**只在用户可见的响应里**消费 `x-mdb-relay-host`，不要把它
当成身份凭据（开放项目，任何人都可以伪造，这是设计允许的）。

### 6. `relayInfo()` 对空值/畸形值的兜底

`relay.ts:10` 直接 `relayHost || env.BASE_DOMAIN`。任何人可发
`X-MDB-Relay-Host: evil.com/../x` 或 `x:1234`。由于输出只进入 URL 字符串，
风险是生成畸形链接而非执行，但建议：

- 校验为裸 host（不含 `/`、不含 `:`、匹配 `[a-z0-9.-]+`），不合法则回落 `BASE_DOMAIN`；
- 输出前统一 `strings.TrimSuffix` 去尾部点。

## P2 — 其他审计记录

### 7. `sanitizeResponse` 剥 `www-authenticate` 之后，Worker 内的 OCI 路径已自洽，
但 relay 侧曾发现 `Set-Cookie` 会在 relay→客户端方向原样透传。开放项目下
不可利用（Worker 不设 cookie），仅记录：**若将来 Worker 引入任何 cookie，
必须同时改 relay 的 `copyHeaders`**，否则 `Domain=mirs.uk` 的 cookie 会种到
relay 域名下。

### 8. `/api/mirrors` 无缓存头

relay 每 10 分钟拉一次这个接口（带 ±30s 抖动）。建议 Worker 侧加
`Cache-Control: public, max-age=60`，让多级 relay 与 CF 边缘共享缓存，
减少 Worker 请求数。

### 9. `mirrors` 清单缺少 `keepHTTP` 之外的传输能力描述

relay 需要知道某个镜像是否需要特殊处理（如只支持 HTTPS、或需要 range 透传）。
当前只用 `keepHTTP`，够用。若未来新增能力位，relay 侧 `server.Mirror` 结构体
需同步扩展（relay 现在会忽略未知字段，向后兼容）。
