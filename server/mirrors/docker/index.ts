import type { Mirror } from "@server/types";

import { handle } from "./docker";

/** Docker Hub mirror, served at the `dcr` subdomain. */
export default {
  name: "dcr",
  host: "dcr",
  policyTags: ["oci"],
  fetch: handle,
} satisfies Mirror;
