/**
 * The 404 page for a path that is not a mirror mount.
 *
 * Historically an unknown first segment fell through to the SPA and answered
 * `200 text/html` — a typo in a package manager config looked like success, and
 * a probe could not tell a wrong mount from a working one. Now a miss is a real
 * `404`; a browser additionally gets a short page that names what is wrong and
 * points at the home page.
 */

const REDIRECT_SECONDS = 5;

/** Escapes text for HTML text/attribute context. */
const esc = (s: string): string =>
  s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;");

const isBrowser = (request: Request): boolean =>
  (request.headers.get("accept") ?? "").includes("text/html") ||
  /Mozilla|Chrome|Safari|Firefox|Edg\//i.test(request.headers.get("user-agent") ?? "");

export interface NotFoundOptions {
  /** The path that was not found, e.g. `/cargo/foo`. */
  pathname: string;
  /** Mirror mounts that do exist, e.g. `["npm", "pypi"]`. */
  mounts: string[];
}

export function notFoundResponse(request: Request, opts: NotFoundOptions): Response {
  const mount = opts.pathname.replace(/^\/+/, "").split("/")[0] ?? "";
  const mounts = opts.mounts.map((m) => `/${m}/`);

  const headers = {
    "cache-control": "no-store",
    "x-content-type-options": "nosniff",
  };

  if (!isBrowser(request)) {
    const body = [
      `404 Not Found: /${mount} is not a mirror on this service.`,
      "",
      "Available mirrors:",
      ...mounts.map((m) => `  ${m}`),
      "",
      "See /api/mirrors for the machine-readable list.",
      "",
    ].join("\n");
    return new Response(body, {
      status: 404,
      headers: { ...headers, "content-type": "text/plain; charset=utf-8" },
    });
  }

  const list = mounts.map((m) => `<li><a href="${esc(m)}">${esc(m)}</a></li>`).join("");
  const html = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta http-equiv="refresh" content="${REDIRECT_SECONDS}; url=/">
<title>404 — /${esc(mount)} is not a mirror</title>
<style>
body{margin:0;min-height:100vh;display:grid;place-items:center;background:#0b0b0c;color:#e6e6e6;
font:14px/1.6 ui-sans-serif,system-ui,sans-serif}
main{max-width:34rem;padding:2rem}
h1{margin:0 0 .25rem;font-size:1.05rem;font-weight:600}
code{background:#1b1b1d;padding:.1rem .35rem;border-radius:4px}
ul{padding-left:1.1rem;columns:2}
a{color:#8ab4f8}
p{color:#9a9a9a;margin:.6rem 0}
</style>
</head>
<body>
<main>
  <h1>404 — <code>/${esc(mount)}</code> is not a mirror</h1>
  <p>Nothing is served at this path on this host. Returning to the home page in
  <span id="s">${REDIRECT_SECONDS}</span>s…</p>
  <p><strong>Available mirrors</strong></p>
  <ul>${list}</ul>
  <p>Machine-readable list: <a href="/api/mirrors">/api/mirrors</a></p>
</main>
<script>
(function () {
  var n = ${REDIRECT_SECONDS}, el = document.getElementById("s");
  var t = setInterval(function () {
    n -= 1;
    if (el) el.textContent = String(n);
    if (n <= 0) { clearInterval(t); location.replace("/"); }
  }, 1000);
})();
</script>
</body>
</html>`;

  return new Response(html, {
    status: 404,
    headers: { ...headers, "content-type": "text/html; charset=utf-8" },
  });
}
