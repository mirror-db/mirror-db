/**
 * Streaming stanza parser for APT index files (`Packages`, `Sources`).
 *
 * `apt-parser`'s `Packages` class materialises every stanza up front, which is
 * fine for a few hundred packages but not for an index that decompresses to
 * several megabytes. This parser consumes text in arbitrary chunks, emits one
 * `Map` per stanza and drops it immediately, so memory stays proportional to a
 * single stanza rather than the whole file.
 *
 * Format (Debian policy §5.1): RFC822-style `Field: value` lines; a line that
 * starts with a space or tab continues the previous field; a blank line ends the
 * stanza. Field names are case-insensitive, so keys are lower-cased.
 */

/** One parsed stanza: lower-cased field name → unfolded value. */
export type Stanza = ReadonlyMap<string, string>;

export class StanzaStreamParser {
  private stanza = new Map<string, string>();
  private lastField: string | null = null;
  /** Incomplete trailing line carried between chunks. */
  private buffer = "";

  constructor(private readonly onStanza: (stanza: Stanza) => void) {}

  /** Feed an arbitrary text chunk. Safe to call with split lines. */
  push(chunk: string): void {
    this.buffer += chunk;
    let start = 0;
    for (let i = 0; i < this.buffer.length; i++) {
      if (this.buffer.charCodeAt(i) !== 10 /* \n */) continue;
      this.line(this.trimCR(this.buffer.slice(start, i)));
      start = i + 1;
    }
    this.buffer = this.buffer.slice(start);
  }

  /** Flush the trailing line and the final stanza. */
  end(): void {
    if (this.buffer.length) {
      this.line(this.trimCR(this.buffer));
      this.buffer = "";
    }
    this.flush();
  }

  private trimCR(line: string): string {
    return line.endsWith("\r") ? line.slice(0, -1) : line;
  }

  private line(line: string): void {
    if (line === "") {
      this.flush();
      return;
    }

    const first = line.charCodeAt(0);
    if (first === 32 /* space */ || first === 9 /* tab */) {
      // Continuation of the previous field (folded Description, multi-line
      // Files/Checksums blocks). A continuation with no field to attach to is
      // malformed; skip it rather than aborting a whole generation.
      if (this.lastField === null) return;
      const current = this.stanza.get(this.lastField) ?? "";
      const folded = current === "" ? line.slice(1) : `${current}\n${line.slice(1)}`;
      this.stanza.set(this.lastField, folded);
      return;
    }

    const colon = line.indexOf(":");
    if (colon <= 0) return; // Not a field line; ignore.
    const key = line.slice(0, colon).trim().toLowerCase();
    if (!key) return;
    this.stanza.set(key, line.slice(colon + 1).trim());
    this.lastField = key;
  }

  private flush(): void {
    if (this.stanza.size > 0) this.onStanza(this.stanza);
    this.stanza = new Map();
    this.lastField = null;
  }
}
