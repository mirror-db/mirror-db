/**
 * deb822 rendering for APT source stanzas.
 *
 * Field order is fixed (`Types`, `URIs`, `Suites`, `Components`,
 * `Architectures`, `Signed-By`), empty optional fields are omitted, stanzas are
 * separated by one blank line, and the file ends with exactly one newline.
 */

import type { AptSourceStanza } from "./types";

const asList = (value: string | string[]): string[] =>
  (Array.isArray(value) ? value : [value]).map((v) => v.trim()).filter((v) => v !== "");

const fieldList = (value: string | string[]): string => asList(value).join(" ");

const commentLines = (comment: string | string[] | undefined): string[] =>
  (comment === undefined ? [] : Array.isArray(comment) ? comment : [comment])
    .flatMap((line) => line.replace(/\r\n/g, "\n").split("\n"))
    .map((line) => (line === "" ? "#" : `# ${line}`));

/**
 * Render one or more stanzas as a complete `.sources` file.
 *
 * An inline key arrives already formatted as a deb822 value (its first line
 * follows `Signed-By: ` directly, continuation lines keep one space of indent,
 * and a blank line is `.` — deb822 has no empty continuation line).
 */
export function renderAptSources(stanzas: AptSourceStanza[]): string {
  if (!stanzas.length) throw new Error("no apt stanzas to render");
  return `${stanzas.map(renderStanza).join("\n\n")}\n`;
}

function renderStanza(stanza: AptSourceStanza): string {
  const lines = commentLines(stanza.comment);

  const uris = asList(stanza.uris);
  if (!uris.length) throw new Error("apt stanza has no URIs");
  const suites = asList(stanza.suites);
  if (!suites.length) throw new Error("apt stanza has no Suites");

  lines.push(`Types: ${fieldList(stanza.types ?? "deb")}`);
  lines.push(`URIs: ${uris.join(" ")}`);
  lines.push(`Suites: ${suites.join(" ")}`);

  const components = stanza.components ? asList(stanza.components) : [];
  if (components.length) lines.push(`Components: ${components.join(" ")}`);

  const architectures = stanza.architectures ? asList(stanza.architectures) : [];
  if (architectures.length) lines.push(`Architectures: ${architectures.join(" ")}`);

  if (stanza.signedBy !== undefined) lines.push(renderSignedBy(stanza.signedBy));

  return lines.join("\n");
}

function renderSignedBy(signedBy: string | { sources: string }): string {
  if (typeof signedBy === "string") return `Signed-By: ${signedBy}`;
  return `Signed-By: ${signedBy.sources}`;
}
