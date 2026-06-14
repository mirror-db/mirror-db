const MAX_MB = 1024;
const CHUNK_SIZE = 65536; // 64KB

export function handleSpeedtest(request: Request, mb: string): Response {
  const n = parseInt(mb, 10);
  if (!Number.isFinite(n) || n <= 0 || n > MAX_MB) {
    return new Response(`Usage: /speedtest/<1-${MAX_MB}> (MB)`, { status: 400 });
  }

  const totalBytes = n * 1024 * 1024;
  let sent = 0;

  const stream = new ReadableStream({
    pull(controller: ReadableStreamDefaultController) {
      const remaining = totalBytes - sent;
      if (remaining <= 0) {
        controller.close();
        return;
      }
      const chunk = new Uint8Array(Math.min(CHUNK_SIZE, remaining));
      crypto.getRandomValues(chunk);
      controller.enqueue(chunk);
      sent += chunk.byteLength;
    },
  });

  return new Response(stream, {
    headers: {
      "Content-Type": "application/octet-stream",
      "Content-Length": String(totalBytes),
      "Cache-Control": "no-store",
    },
  });
}
