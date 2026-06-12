export type { WebListParser, WebListParserFactory } from "./types";
export { TableParser, createTableParser } from "./table";
export { UlParser, createUlParser } from "./ul";
export { PreParser, createPreParser } from "./pre";
export { BareUlParser, createBareUlParser } from "./bare-ul";

import type { WebListParserFactory } from "./types";
import { createTableParser } from "./table";
import { createUlParser } from "./ul";
import { createPreParser } from "./pre";
import { createBareUlParser } from "./bare-ul";

/**
 * Default parser set — all known formats. First with results wins.
 *
 * Order matters: Table and UL (class-qualified) are specific and unlikely to
 * false-positive; Pre fires on `<pre>` blocks; BareUl is the most generic
 * (classless `<ul>`) so it goes last.
 */
export const allParsers: WebListParserFactory[] = [
  createTableParser,
  createUlParser,
  createPreParser,
  createBareUlParser,
];
