/**
 * Helm chart repository definitions. Each entry is mirrored at
 * `mirs.uk/charts/<name>/` — `helm repo add <name> https://mirs.uk/charts/<name>`.
 */

export interface HelmRepo {
  /** Repo name — also the path segment under `/charts/`. */
  name: string;
  /** Upstream Helm repo base URL (the dir containing `index.yaml`). */
  url: string;
  description?: string;
  homepage?: string;
  /** Example chart names, for the web UI / docs. */
  example_charts?: string[];
}

export const HelmReposList: HelmRepo[] = [
  {
    name: "keel",
    url: "https://charts.keel.sh",
    example_charts: ["keel"],
  },
  {
    name: "cert-manager",
    url: "https://charts.jetstack.io",
    example_charts: ["cert-manager"],
  },
  {
    name: "gitlab",
    url: "https://charts.gitlab.io",
    example_charts: ["gitlab-agent", "gitlab-runner"],
  },
  {
    name: "nvdp",
    url: "https://nvidia.github.io/k8s-device-plugin",
    example_charts: ["nvidia-device-plugin"],
  },
  {
    name: "nfd",
    url: "https://kubernetes-sigs.github.io/node-feature-discovery/charts",
    example_charts: ["node-feature-discovery"],
  },
  {
    name: "linolabx",
    url: "https://linolabx.github.io/charts",
    example_charts: ["network-exposer"],
  },
  {
    name: "openebs",
    url: "https://openebs.github.io/openebs",
    example_charts: ["openebs"],
  },
  {
    name: "rook",
    url: "https://charts.rook.io/release",
    example_charts: ["rook-ceph", "rook-ceph-cluster"],
  },
  {
    name: "ceph-csi-operator",
    url: "https://ceph.github.io/ceph-csi-operator",
    example_charts: ["ceph-csi-drivers", "ceph-csi-operator"],
  },
  {
    name: "mysql-operator",
    url: "https://mysql.github.io/mysql-operator",
    example_charts: ["mysql-operator", "mysql-innodbcluster"],
  },
  {
    name: "hami",
    url: "https://project-hami.github.io/HAMi",
    example_charts: ["hami"],
  },
  {
    name: "acmehub",
    url: "https://geektr-cloud.github.io/acme-hub",
    example_charts: ["acmehub-syncer"],
  },
];
