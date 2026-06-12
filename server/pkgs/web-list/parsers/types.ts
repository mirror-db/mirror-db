import type { WebListEntry } from "../parse";

/**
 * A self-contained parser that registers HTMLRewriter handlers and produces
 * structured directory entries after the stream completes.
 *
 * Multiple parsers can be installed on the same HTMLRewriter simultaneously;
 * their element selectors must be disjoint or carefully layered so they don't
 * interfere (e.g. table vs. pre vs. ul.directorycontents).
 */
export interface WebListParser {
  /** Register element/text handlers on the shared rewriter. */
  install(rewriter: HTMLRewriter): void;
  /** Return parsed entries after `.transform().arrayBuffer()` finishes. */
  result(): WebListEntry[];
}

/** Factory that creates a fresh parser instance (stateful, one-shot). */
export type WebListParserFactory = () => WebListParser;
