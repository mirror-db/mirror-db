import { handle } from "./docker";

/** Host descriptor: served at the `dcr` subdomain. */
export default {
  host: "dcr",
  fetch: handle,
};
