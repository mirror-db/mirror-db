export function IconURL(exp: string): string {
  let [source, icon, ext] = exp.split(":");
  if (source === "si") {
    return `https://cdn.jsdelivr.net/npm/simple-icons@v15/icons/${icon}.svg`;
  }
  if (source === "sh") {
    ext = ext || "svg";
    return `https://cdn.jsdelivr.net/gh/selfhst/icons/${ext}/${icon}.${ext}`;
  }
  if (source === "di") {
    ext = ext || "svg";
    return `https://cdn.jsdelivr.net/gh/homarr-labs/dashboard-icons/${ext}/${icon}.${ext}`;
  }
  throw new Error(`Unknown icon source: ${source}`);
}
