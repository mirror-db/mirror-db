import { Context } from "hono";

export const sendScript = (c: Context, script: string) => {
  const isBrowser = /Mozilla|AppleWebKit|Chrome|Safari|Firefox/.test(c.req.header("user-agent") || "");

  if (isBrowser) {
    return c.html(`<pre><code>${script.replace(/</g, "&lt;").replace(/>/g, "&gt;")}</code></pre>`, 200, {
      "Content-Type": "text/html",
    });
  }

  return c.text(script, 200, { "Content-Type": "text/x-shellscript" });
};
