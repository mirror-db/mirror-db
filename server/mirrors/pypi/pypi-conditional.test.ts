import { afterEach, describe, expect, it, vi } from "vitest";

import { pypi } from "./index";

/**
 * Conditional revalidation of `/pypi/simple/`, exercised against the real
 * handler and the real fetch helpers: only the network boundary is stubbed.
 *
 * The Worker's read-through cache answers from its own copy and ignores the
 * client's validator, so a conditional request must bypass it and reach
 * pypi.org. A relay rebuilding its lease after a restart depends on getting a
 * real `304` back instead of a full 48 MB index.
 */

const fetchMock = vi.fn();
vi.stubGlobal("fetch", fetchMock);

afterEach(() => {
  fetchMock.mockReset();
});

const conditional = (etag: string) =>
  new Request("https://mirs.uk/pypi/simple/", {
    headers: { "if-none-match": etag },
  });

describe("pypi simple index — real conditional passthrough", () => {
  it("forwards the inbound validator upstream and hands back the 304", async () => {
    fetchMock.mockImplementation(async (request: Request) => {
      expect(request.url).toBe("https://pypi.org/simple/");
      expect(request.headers.get("if-none-match")).toBe('"index-1"');
      expect(request.headers.get("accept-encoding")).toBe("identity");
      return new Response(null, { status: 304, headers: { etag: '"index-1"' } });
    });

    const res = await pypi.fetch(conditional('"index-1"'));

    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(res.status).toBe(304);
    expect(res.headers.get("etag")).toBe('"index-1"');
    expect(res.headers.get("x-mdb-relay")).toBe("prefetch");
    expect(await res.text()).toBe("");
  });

  it("does not fabricate a 304 — an upstream error is passed through", async () => {
    fetchMock.mockImplementation(async () => new Response("bad", { status: 502 }));

    const res = await pypi.fetch(conditional('"index-1"'));

    expect(res.status).toBe(502);
    expect(res.headers.get("x-mdb-relay")).toBeNull();
  });
});
