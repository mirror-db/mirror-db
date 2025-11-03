import pLimit from "p-limit";
import urlJoin from "url-join";

export interface CollectionStats {
  files: number;
  size: number;
  deduplicatedSize: number;
  deduplicatedFiles: number;
}

export class CollectionCounter {
  private files: number = 0;
  private size: number = 0;
  private map: Record<string, number> = {};

  constructor(items?: [string, number][]) {
    if (!items) return;
    if (!Array.isArray(items)) {
      throw new Error("Items must be an array");
    }
    this.bulk(items);
  }

  add(item: [string, number]) {
    this.files += 1;
    this.size += item[1];
    this.map[item[0]] = (this.map[item[0]] || 0) + item[1];
  }
  bulk(items: [string, number][]) {
    for (const item of items) this.add(item);
  }
  merge(other: CollectionCounter) {
    this.files += other.files;
    this.size += other.size;
    for (const key in other.map) {
      this.add([key, other.map[key] as number]);
    }
  }
  export(): CollectionStats {
    return {
      files: this.files,
      size: this.size,
      deduplicatedSize: Object.values(this.map).reduce(
        (acc, size) => acc + size,
        0
      ),
      deduplicatedFiles: Object.keys(this.map).length,
    };
  }
}

export const cachedFetchFactory = (cacheName: string, concurrency: number) => {
  const limit = pLimit(concurrency);

  const _fetch = async (...parts: string[]) => {
    const url = urlJoin(...parts);

    const cache = await caches.open(cacheName);
    let response = await cache.match(url, { ignoreMethod: true });
    if (response) return new Response(response.body, response);

    response = await fetch(url, { headers: { "User-Agent": "curl/7.54.1" } });
    if (response.ok) await cache.put(url, response.clone());
    return response;
  };

  return (...parts: string[]) => limit(() => _fetch(...parts));
};

export const subfetch = cachedFetchFactory("subrequests", 32);
