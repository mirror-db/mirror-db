import type { Mirror } from "@server/types";

import { handle } from "./docker";

/** Docker Hub mirror, served at the `dcr` subdomain. */
export default {
  name: "dcr",
  host: "dcr",
  fetch: handle,
} satisfies Mirror;
