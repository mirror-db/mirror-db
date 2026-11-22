/**
 * Web and special APT presets.
 *
 * These repositories are served by a file-tree mount rather than an `AptRepo`:
 * `docker-ce`, `microsoft`, `cuda`, `kubernetes` and `libnvidia-container` are
 * listing or passthrough mounts, so the repository root is a path several
 * segments below the mount — and NVIDIA puts the architecture in the URL.
 * Every preset therefore spells out the full path instead of deriving one from
 * the mount name.
 *
 * `pigsty` is the same shape: one mount carries two independent roots,
 * `infra/` (suite `generic`) and `pgsql/<codename>/` (each codename its own
 * repository), which `base + dists/<suite>` cannot address.
 *
 * Signing material is embedded inline, exactly as `/keys/apt/<name>/in-sources`
 * prints it, and no preset accepts an architecture upstream does not publish.
 */

import { aptPreset } from "../preset";
import type { AptPresetRegistry } from "../registry";
import { AptSourceError } from "../sources";
import type { AptPreset, AptRenderContext, AptSourceStanza } from "../types";

/** These repositories are not distribution archives: `amd64` is the default. */
const Amd64 = ["amd64"];

/** Trimmed origin, so mount paths below can start with a single slash. */
const trimOrigin = (origin: string) => origin.replace(/\/+$/, "");

/**
 * `download.docker.com/linux/<distro>` publishes one suite per codename, with
 * the `stable` component. There is deliberately no unversioned
 * `docker.sources` and no Raspbian: both would have to guess a suite.
 */
const DockerReleases = [
  { distro: "debian", version: "12", codename: "bookworm" },
  { distro: "debian", version: "13", codename: "trixie" },
  { distro: "ubuntu", version: "22.04", codename: "jammy" },
  { distro: "ubuntu", version: "24.04", codename: "noble" },
  { distro: "ubuntu", version: "26.04", codename: "resolute" },
  { distro: "ubuntu", version: "26.10", codename: "stonking" },
] as const;

/**
 * `packages.microsoft.com/repos/microsoft-<distro>-<codename>-prod` — the
 * stable trees. Other subrepositories (vscode, powershell, …) and EOL
 * distributions are not registered: this project mirrors the trees it can name
 * a suite for.
 */
const MicrosoftProdReleases = [
  { distro: "debian", version: "12", codename: "bookworm" },
  { distro: "debian", version: "13", codename: "trixie" },
  { distro: "ubuntu", version: "22.04", codename: "jammy" },
  { distro: "ubuntu", version: "24.04", codename: "noble" },
  { distro: "ubuntu", version: "26.04", codename: "resolute" },
] as const;

/** `packages.microsoft.com/repos/azure-cli`, one suite per codename. */
const AzureCliReleases = [
  { distro: "debian", version: "12", codename: "bookworm" },
  { distro: "ubuntu", version: "22.04", codename: "jammy" },
  { distro: "ubuntu", version: "24.04", codename: "noble" },
  { distro: "ubuntu", version: "26.04", codename: "resolute" },
] as const;

/** `repo.pigsty.io/apt/pgsql/<codename>` — each codename is its own root. */
const PigstyPgsqlReleases = [
  { distro: "debian", version: "12", codename: "bookworm" },
  { distro: "debian", version: "13", codename: "trixie" },
  { distro: "ubuntu", version: "22.04", codename: "jammy" },
  { distro: "ubuntu", version: "24.04", codename: "noble" },
  { distro: "ubuntu", version: "26.04", codename: "resolute" },
] as const;

/** `pkgs.k8s.io` publishes one flat repository per minor, and no rolling one. */
const KubernetesMinors = ["1.35", "1.36", "1.37"] as const;

/** The architectures `nvidia.github.io/libnvidia-container` publishes. */
const LibnvidiaArchitectures: readonly string[] = ["amd64", "arm64", "ppc64el"];

/**
 * One NVIDIA CUDA tree per (distribution, architecture). `dir` is the upstream
 * directory name.
 *
 * `arm64` is not uniform: Debian publishes an SBSA tree, Ubuntu 22.04 and 24.04
 * a native arm64 one, and Ubuntu 26.04 moved to SBSA again.
 *
 * `legacyKey` marks a tree whose signing certificate is an old SHA-1
 * self-signature. apt 3.x verifies with sqv and rejects it; that is a client
 * prerequisite, so the preset says so instead of weakening verification.
 */
const CudaReleases = [
  {
    distro: "debian",
    version: "12",
    codename: "bookworm",
    dir: "debian12",
    arm64: "sbsa",
    legacyKey: true,
  },
  {
    distro: "debian",
    version: "13",
    codename: "trixie",
    dir: "debian13",
    arm64: "sbsa",
    legacyKey: false,
  },
  {
    distro: "ubuntu",
    version: "22.04",
    codename: "jammy",
    dir: "ubuntu2204",
    arm64: "arm64",
    legacyKey: true,
  },
  {
    distro: "ubuntu",
    version: "24.04",
    codename: "noble",
    dir: "ubuntu2404",
    arm64: "arm64",
    legacyKey: true,
  },
  {
    distro: "ubuntu",
    version: "26.04",
    codename: "resolute",
    dir: "ubuntu2604",
    arm64: "sbsa",
    legacyKey: false,
  },
] as const;

const CudaLegacyKeyWarning = [
  "Old CUDA signing key: its self-signature is SHA-1, which apt 3.x rejects",
  "when verifying with sqv. No `trusted=yes` workaround is added here.",
];

export function registerDockerPresets(registry: AptPresetRegistry): void {
  for (const release of DockerReleases) {
    registry.register(
      [
        `docker-${release.distro}-${release.version}`,
        `docker-${release.distro}-${release.codename}`,
      ],
      aptPreset({
        mount: `docker-ce/linux/${release.distro}`,
        suites: [release.codename],
        components: ["stable"],
        architectures: Amd64,
        key: "docker-ce",
      }),
    );
  }
}

export function registerMicrosoftPresets(registry: AptPresetRegistry): void {
  for (const release of MicrosoftProdReleases) {
    registry.register(
      [
        `microsoft-${release.distro}-${release.version}`,
        `microsoft-${release.distro}-${release.codename}`,
      ],
      aptPreset({
        mount: `microsoft/microsoft-${release.distro}-${release.codename}-prod`,
        suites: [release.codename],
        components: ["main"],
        architectures: Amd64,
        key: "microsoft",
      }),
    );
  }

  // These two subrepositories publish a single `stable` suite: there is no
  // codename to choose, but the suite is still explicit.
  for (const name of ["code", "edge"]) {
    registry.register(
      `microsoft-${name}`,
      aptPreset({
        mount: `microsoft/${name}`,
        suites: ["stable"],
        components: ["main"],
        architectures: Amd64,
        key: "microsoft",
      }),
    );
  }

  for (const release of AzureCliReleases) {
    registry.register(
      [
        `microsoft-azure-cli-${release.distro}-${release.version}`,
        `microsoft-azure-cli-${release.distro}-${release.codename}`,
      ],
      aptPreset({
        mount: "microsoft/azure-cli",
        suites: [release.codename],
        components: ["main"],
        architectures: Amd64,
        key: "microsoft",
      }),
    );
  }
}

export function registerPigstyPresets(registry: AptPresetRegistry): void {
  registry.register(
    "pigsty-infra",
    aptPreset({
      mount: "pigsty/infra",
      suites: ["generic"],
      components: ["main"],
      architectures: Amd64,
      key: "pigsty",
    }),
  );

  for (const release of PigstyPgsqlReleases) {
    registry.register(
      [
        `pigsty-pgsql-${release.distro}-${release.version}`,
        `pigsty-pgsql-${release.distro}-${release.codename}`,
      ],
      aptPreset({
        mount: `pigsty/pgsql/${release.codename}`,
        suites: [release.codename],
        components: ["main"],
        architectures: Amd64,
        key: "pigsty",
      }),
    );
  }
}

export function registerKubernetesPresets(registry: AptPresetRegistry): void {
  for (const minor of KubernetesMinors) {
    registry.register(
      `kubernetes-${minor}`,
      aptPreset({
        mount: `kubernetes/core:/stable:/v${minor}/deb`,
        flat: true,
        architectures: Amd64,
        key: "kubernetes",
      }),
    );
  }
}

interface FlatArchPresetOptions {
  /** Mount-relative path below the origin, up to the architecture segment. */
  basePath: string;
  /** Upstream directory for an architecture, or undefined when unsupported. */
  archDir: (arch: string) => string | undefined;
  /** Accepted architectures, for the error message. */
  supported: readonly string[];
  key: string;
  comment?: string | string[];
}

/**
 * A flat repository whose URL carries the architecture.
 *
 * One stanza per requested architecture, because the path differs per arch. An
 * architecture upstream does not publish is rejected as a client error: the
 * alternative is a generated source pointing at a path that does not exist.
 */
function flatArchPreset(options: FlatArchPresetOptions): AptPreset {
  const preset = async ({ origin, overrides, inlineKey }: AptRenderContext) => {
    const suites = overrides.suites ?? ["/"];
    const signedBy = { sources: await inlineKey(options.key) };
    const root = trimOrigin(origin);

    const architectures = overrides.architectures ?? ["amd64"];
    return architectures.map((arch): AptSourceStanza => {
      const dir = options.archDir(arch);
      if (!dir) {
        throw new AptSourceError(
          `unsupported architecture: ${arch} (expected ${options.supported.join(", ")})`,
        );
      }
      return {
        comment: options.comment,
        uris: `${root}/${options.basePath}/${dir}/`,
        suites,
        architectures: [arch],
        signedBy,
      };
    });
  };

  return Object.assign(preset, { flat: true });
}

export function registerNvidiaPresets(registry: AptPresetRegistry): void {
  registry.register(
    "libnvidia-container",
    flatArchPreset({
      basePath: "libnvidia-container/stable/deb",
      archDir: (arch) => (LibnvidiaArchitectures.includes(arch) ? arch : undefined),
      supported: LibnvidiaArchitectures,
      key: "libnvidia-container",
    }),
  );

  for (const release of CudaReleases) {
    registry.register(
      [
        `cuda-${release.distro}-${release.version}`,
        `cuda-${release.distro}-${release.codename}`,
      ],
      flatArchPreset({
        basePath: `cuda/${release.dir}`,
        archDir: (arch) => {
          if (arch === "amd64") return "x86_64";
          if (arch === "arm64") return release.arm64;
          return undefined;
        },
        supported: ["amd64", "arm64"],
        key: "cuda",
        comment: release.legacyKey ? CudaLegacyKeyWarning : undefined,
      }),
    );
  }
}

/** Every preset backed by a web-list or passthrough mount. */
export function registerWebAptPresets(registry: AptPresetRegistry): void {
  registerDockerPresets(registry);
  registerMicrosoftPresets(registry);
  registerPigstyPresets(registry);
  registerKubernetesPresets(registry);
  registerNvidiaPresets(registry);
}
