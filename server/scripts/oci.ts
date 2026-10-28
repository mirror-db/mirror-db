import { Hono } from "hono";
import { z } from "zod";
import { zValidator } from "@hono/zod-validator";
import { sendScript } from "./utils";
import { relayInfo } from "@server/relay";

const OciMap: [string, string][] = [
  ["docker.io", "dcr"],
  ["ghcr.io", "ghcr"],
  ["gcr.io", "gcr"],
  ["k8s.gcr.io", "k8s"],
  ["registry.k8s.io", "k8s"],
  ["quay.io", "quay"],
  ["mcr.microsoft.com", "mcr"],
  ["nvcr.io", "nvcr"],
  ["container-registry.oracle.com", "ocr"],
];

interface OciSetupOptions {
  sudo?: boolean;
  server: string;
}

const dockerOciSetup = ({ sudo, server }: OciSetupOptions) => {
  const prefix = sudo ? "sudo " : "";

  return `#!/bin/bash
set -euo pipefail

${prefix}install -d -m 0755 /etc/docker
${prefix}install -m 0644 /dev/stdin /etc/docker/daemon.json <<EOF
{"registry-mirrors": ["https://dcr.${server}"]}
EOF
`;
};

const k3sOciSetup = ({ sudo, server }: OciSetupOptions) => {
  const prefix = sudo ? "sudo " : "";
  return `#!/bin/bash
set -euo pipefail

${prefix}install -d -m 0755 /etc/rancher/k3s
${prefix}install -m 0644 /dev/stdin /etc/rancher/k3s/registries.yaml <<EOF
mirrors:
${OciMap.map(([host, sub]) => `  ${host}: {"endpoint":["https://${sub}.${server}"]}`).join("\n")}
EOF
`;
};

const containerdOciSetup = ({ sudo, server }: OciSetupOptions) => {
  const prefix = sudo ? "sudo " : "";
  return `#!/bin/bash
set -euo pipefail

${prefix}install -d -m 0755 /etc/containerd/certs.d

test -f /etc/containerd/config.toml && ! grep -Fxq '[plugins."io.containerd.cri.v1.images".registry]' /etc/containerd/config.toml && ${prefix}tee -a /etc/containerd/config.toml >/dev/null <<EOF
[plugins."io.containerd.cri.v1.images".registry]
  config_path = "/etc/containerd/certs.d"
EOF

write_host() {
  ${prefix}install -d -m 0755 "/etc/containerd/certs.d/$1"
  ${prefix}install -m 0644 /dev/stdin "/etc/containerd/certs.d/$1/hosts.toml" <<EOF
server = "https://$1"

[host."https://$2.${server}"]
  capabilities = ["pull", "resolve"]
EOF
}

${OciMap.map(([host, sub]) => `write_host ${host} ${sub}`).join("\n")}

#${prefix}systemctl restart containerd
`;
};

export const regOciRoute = (h: Hono) =>
  h.get(
    "/oci/:type",
    zValidator("param", z.object({ type: z.enum(["docker.sh", "k3s.sh", "containerd.sh"]) })),
    zValidator(
      "query",
      z.object({
        sudo: z.enum(["true", "false"]).optional(),
        server: z.string().optional(),
      }),
    ),
    async (c) => {
      const { type } = c.req.valid("param");
      let { sudo, server } = c.req.valid("query");
      const { baseDomain } = relayInfo(c);
      const opts = {
        sudo: sudo === "true",
        server: server || baseDomain,
      };

      if (type === "docker.sh") return sendScript(c, dockerOciSetup(opts));
      if (type === "k3s.sh") return sendScript(c, k3sOciSetup(opts));
      if (type === "containerd.sh") return sendScript(c, containerdOciSetup(opts));
    },
  );
