import { describe, expect, it } from "vitest";

import { aptPreset } from "./preset";
import { registerAptPresets } from "./presets";
import { AptPresetRegistry } from "./registry";
import { AptQueryError } from "./params";
import { AptSourceError, renderAptSource } from "./sources";
import type { AptKeyLoader, AptPresetEntry } from "./types";

const ORIGIN = "https://mirs.uk";

/** Inline key value, shaped exactly as the key proxy's `inSources()` emits it. */
const fakeKey: AptKeyLoader = async (name) =>
  [
    "-----BEGIN PGP PUBLIC KEY BLOCK-----",
    " .",
    ` ${name} key`,
    " -----END PGP PUBLIC KEY BLOCK-----",
  ].join("\n");

const registry = registerAptPresets(new AptPresetRegistry());

const entry = (name: string): AptPresetEntry => {
  const found = registry.get(name);
  if (!found) throw new Error(`preset not registered: ${name}`);
  return found;
};

const render = (name: string, search = "", origin = ORIGIN) =>
  renderAptSource(entry(name), {
    origin,
    query: new URLSearchParams(search),
    inlineKey: fakeKey,
  });

const inlineField = (key: string) =>
  [
    "Signed-By: -----BEGIN PGP PUBLIC KEY BLOCK-----",
    " .",
    ` ${key} key`,
    " -----END PGP PUBLIC KEY BLOCK-----",
  ].join("\n");

/** Number of deb822 stanzas in a rendered file. */
const stanzaCount = (file: string) => (file.match(/^Types: deb$/gm) ?? []).length;

describe("Debian presets", () => {
  /** Archive stanza followed by the release's one fixed security stanza. */
  const debianFile = (
    archiveSuites: string,
    securitySuite: string,
    components = "main contrib non-free non-free-firmware",
  ) =>
    [
      "Types: deb",
      `URIs: ${ORIGIN}/debian/`,
      `Suites: ${archiveSuites}`,
      `Components: ${components}`,
      "Signed-By: /usr/share/keyrings/debian-archive-keyring.gpg",
      "",
      "Types: deb",
      `URIs: ${ORIGIN}/debian-security/`,
      `Suites: ${securitySuite}`,
      `Components: ${components}`,
      "Signed-By: /usr/share/keyrings/debian-archive-keyring.gpg",
      "",
    ].join("\n");

  it("renders bookworm with updates, backports, and the installed keyring", async () => {
    expect(await render("debian-bookworm")).toBe(
      debianFile(
        "bookworm bookworm-updates bookworm-backports",
        "bookworm-security",
      ),
    );
  });

  it("renders trixie the same way", async () => {
    expect(await render("debian-trixie")).toBe(
      debianFile("trixie trixie-updates trixie-backports", "trixie-security"),
    );
  });

  it("renders forky with only its own archive suite", async () => {
    // A brand-new stable has no -updates/-backports suite yet.
    expect(await render("debian-forky")).toBe(
      debianFile("forky", "forky-security"),
    );
  });

  it("does not emit Architectures, leaving native selection to apt", async () => {
    expect(await render("debian-12")).not.toContain("Architectures:");
  });

  it("appends exactly one security stanza per release", async () => {
    for (const [name, suite] of [
      ["debian-bookworm", "bookworm-security"],
      ["debian-trixie", "trixie-security"],
      ["debian-forky", "forky-security"],
    ] as const) {
      const file = await render(name);
      expect(stanzaCount(file)).toBe(2);
      expect(file).toContain(`URIs: ${ORIGIN}/debian-security/\n`);
      expect(file).toContain(`Suites: ${suite}\n`);
      expect(file).not.toContain(`${suite}-updates`);
      expect(file).not.toContain(`${suite}-backports`);
    }
  });

  it("leaves the security suite fixed when `suites` overrides the archive", async () => {
    const file = await render("debian-12", "suites=bookworm,bookworm-updates");
    expect(file).toContain("Suites: bookworm bookworm-updates\n");
    // The security stanza is never a function of the client's suite choice.
    expect(file).toContain("Suites: bookworm-security\n");
    expect(stanzaCount(file)).toBe(2);
  });

  it("applies component and architecture overrides to both stanzas", async () => {
    const file = await render("debian-13", "components=main&arch=arm64");
    expect(file.match(/Components: main\n/g)).toHaveLength(2);
    expect(file.match(/Architectures: arm64\n/g)).toHaveLength(2);
    expect(file).toContain(`URIs: ${ORIGIN}/debian/\n`);
    expect(file).toContain(`URIs: ${ORIGIN}/debian-security/\n`);
    expect(file).toContain("Suites: trixie-security\n");
  });
});

describe("Ubuntu presets", () => {
  const releases: [version: string, codename: string][] = [
    ["22.04", "jammy"],
    ["24.04", "noble"],
    ["26.04", "resolute"],
    ["26.10", "stonking"],
  ];

  for (const [version, codename] of releases) {
    it(`renders ${version}/${codename} with all four suites`, async () => {
      expect(await render(`ubuntu-${codename}`)).toBe(
        [
          "Types: deb",
          `URIs: ${ORIGIN}/ubuntu/`,
          `Suites: ${codename} ${codename}-updates ${codename}-backports ${codename}-security`,
          "Components: main restricted universe multiverse",
          "Signed-By: /usr/share/keyrings/ubuntu-archive-keyring.gpg",
          "",
        ].join("\n"),
      );
    });
  }

  it("registers every version and codename as the same content", async () => {
    for (const [version, codename] of releases) {
      expect(await render(`ubuntu-${version}`)).toBe(await render(`ubuntu-${codename}`));
    }
  });
});

describe("third-party presets", () => {
  it("renders mise with amd64 and its inline key", async () => {
    expect(await render("mise")).toBe(
      [
        "Types: deb",
        `URIs: ${ORIGIN}/mise/`,
        "Suites: stable",
        "Components: main",
        "Architectures: amd64",
        inlineField("mise"),
        "",
      ].join("\n"),
    );
  });

  it("renders syncthing's concrete component instead of a guessed one", async () => {
    const file = await render("syncthing-pkgs");
    expect(file).toContain(`URIs: ${ORIGIN}/syncthing-pkgs/\n`);
    expect(file).toContain("Suites: syncthing\n");
    expect(file).toContain("Components: stable-v2\n");
    expect(file).toContain(inlineField("syncthing-pkgs"));
  });

  it("renders opentofu against the `any` suite with its own key", async () => {
    const file = await render("opentofu");
    expect(file).toContain(`URIs: ${ORIGIN}/opentofu/\n`);
    expect(file).toContain("Suites: any\n");
    expect(file).toContain(inlineField("opentofu"));
  });

  it("renders the GitHub CLI repository", async () => {
    const file = await render("github-cli");
    expect(file).toContain(`URIs: ${ORIGIN}/github-cli/\n`);
    expect(file).toContain("Suites: stable\n");
    expect(file).toContain("Components: main\n");
    expect(file).toContain(inlineField("github-cli"));
  });
});

describe("HashiCorp presets", () => {
  const releases: [name: string, suite: string][] = [
    ["hashicorp-debian-12", "bookworm"],
    ["hashicorp-debian-bookworm", "bookworm"],
    ["hashicorp-debian-13", "trixie"],
    ["hashicorp-debian-trixie", "trixie"],
    ["hashicorp-ubuntu-22.04", "jammy"],
    ["hashicorp-ubuntu-jammy", "jammy"],
    ["hashicorp-ubuntu-24.04", "noble"],
    ["hashicorp-ubuntu-noble", "noble"],
    ["hashicorp-ubuntu-26.04", "resolute"],
    ["hashicorp-ubuntu-resolute", "resolute"],
  ];

  for (const [name, suite] of releases) {
    it(`${name} is one codename suite with main and amd64`, async () => {
      const file = await render(name);
      expect(file).toContain(`URIs: ${ORIGIN}/hashicorp/\n`);
      expect(file).toContain(`Suites: ${suite}\n`);
      expect(file).toContain("Components: main\n");
      expect(file).toContain("Architectures: amd64\n");
      expect(file).toContain(inlineField("hashicorp"));
    });
  }

  it("registers version and codename aliases for the same content", async () => {
    expect(await render("hashicorp-debian-12")).toBe(
      await render("hashicorp-debian-bookworm"),
    );
    expect(await render("hashicorp-ubuntu-22.04")).toBe(
      await render("hashicorp-ubuntu-jammy"),
    );
  });
});

describe("GitLab Runner presets", () => {
  const releases: [version: string, codename: string][] = [
    ["12", "bookworm"],
    ["13", "trixie"],
    ["14", "forky"],
  ];

  for (const [version, codename] of releases) {
    it(`renders debian ${version}/${codename} from its own mount`, async () => {
      const file = await render(`gitlab-runner-debian-${version}`);
      expect(file).toContain(`URIs: ${ORIGIN}/gitlab-runner-deb/\n`);
      expect(file).toContain(`Suites: ${codename}\n`);
      expect(file).toContain("Components: main\n");
      expect(file).toContain("Architectures: amd64\n");
      expect(file).toContain(inlineField("gitlab-runner-deb"));
      expect(file).toBe(await render(`gitlab-runner-debian-${codename}`));
    });
  }
});

describe("query overrides", () => {
  it("replaces suites, components, and architectures", async () => {
    const file = await render(
      "debian-12",
      "suites=bookworm,bookworm-updates&components=main&arch=arm64,amd64",
    );
    expect(file).toContain("Suites: bookworm bookworm-updates\n");
    expect(file).toContain("Components: main\n");
    expect(file).toContain("Architectures: arm64 amd64\n");
  });

  it("omits Components when the override is explicitly empty", async () => {
    expect(await render("mise", "components=")).not.toContain("Components:");
  });

  it("keeps Distribution defaults when the client says nothing", async () => {
    expect(await render("debian-12")).toContain("Components: main contrib");
  });
});

describe("protocol parameter", () => {
  /** Every `URIs:` field value in a rendered file. */
  const uris = (file: string) =>
    [...file.matchAll(/^URIs: (.+)$/gm)].map((match) => match[1]);

  it("defaults to HTTPS even when the request origin was HTTP", async () => {
    const file = await render("debian-12", "", "http://mirs.uk");
    expect(file).toContain("URIs: https://mirs.uk/debian/\n");
    expect(file).toContain("URIs: https://mirs.uk/debian-security/\n");
    expect(file).not.toContain("URIs: http://");
  });

  it("keeps an explicit HTTPS the same way", async () => {
    expect(await render("debian-12", "proto=https", "http://mirs.uk")).toBe(
      await render("debian-12", "", "http://mirs.uk"),
    );
  });

  it("switches every stanza URI to HTTP on request", async () => {
    // Distribution, third-party, and architecture-mapped custom paths all use
    // the same origin, so one parameter covers them all.
    for (const name of [
      "debian-12",
      "mise",
      "cuda-debian-13",
      "libnvidia-container",
    ]) {
      const file = await render(name, "protocol=http");
      const values = uris(file);
      expect(values.length).toBeGreaterThan(0);
      expect(file).not.toContain("URIs: https://");
      for (const value of values) expect(value).toMatch(/^http:\/\//);
    }
  });

  it("keeps the host, port, and relay host while changing the scheme", async () => {
    const file = await render(
      "debian-12",
      "proto=http",
      "https://relay.example.com:8443",
    );
    expect(file).toContain("URIs: http://relay.example.com:8443/debian/\n");
    expect(file).toContain(
      "URIs: http://relay.example.com:8443/debian-security/\n",
    );
  });

  it("accepts both aliases when they agree", async () => {
    expect(await render("debian-12", "protocol=http&proto=http")).toBe(
      await render("debian-12", "proto=http"),
    );
  });

  it("rejects conflicting protocol values", async () => {
    await expect(
      render("debian-12", "protocol=http&proto=https"),
    ).rejects.toThrow(/conflicting protocol/);
    await expect(
      render("debian-12", "protocol=http&protocol=https"),
    ).rejects.toThrow(/conflicting protocol/);
  });

  it("rejects empty, unknown, and comma-separated values", async () => {
    for (const search of [
      "protocol=",
      "proto=ftp",
      "protocol=http,https",
    ]) {
      await expect(render("debian-12", search)).rejects.toThrow(AptQueryError);
    }
  });
});

describe("catalog surface", () => {
  it("never registers an unversioned Debian or Ubuntu default", () => {
    for (const name of ["debian", "debian.sources", "ubuntu", "ubuntu.sources"]) {
      expect(registry.has(name)).toBe(false);
    }
  });

  it("folds Debian security into the release preset instead of a second path", () => {
    for (const name of [
      "debian-security-12",
      "debian-security-bookworm",
      "debian-security-13",
      "debian-security-trixie",
      "debian-security-14",
      "debian-security-forky",
    ]) {
      expect(registry.has(name)).toBe(false);
    }
  });

  it("lists every registered path, aliases included, stable-sorted", () => {
    const names = registry.entries().map((e) => e.name);
    expect(names).toEqual([...names].sort());
    expect(names).toContain("debian-12");
    expect(names).toContain("debian-bookworm");
    expect(names).toContain("ubuntu-22.04");
    expect(names).toContain("ubuntu-jammy");
    expect(names).toContain("gitlab-runner-debian-forky");
    expect(new Set(names).size).toBe(names.length);
  });

  it("registers every expected web/special path", () => {
    for (const name of [
      "docker-debian-12",
      "docker-debian-bookworm",
      "docker-debian-trixie",
      "docker-ubuntu-noble",
      "docker-ubuntu-26.10",
      "docker-ubuntu-stonking",
      "microsoft-debian-bookworm",
      "microsoft-debian-trixie",
      "microsoft-ubuntu-resolute",
      "microsoft-code",
      "microsoft-edge",
      "microsoft-azure-cli-debian-bookworm",
      "microsoft-azure-cli-ubuntu-resolute",
      "pigsty-infra",
      "pigsty-pgsql-debian-trixie",
      "pigsty-pgsql-ubuntu-noble",
      "kubernetes-1.35",
      "kubernetes-1.37",
      "libnvidia-container",
      "cuda-debian-12",
      "cuda-ubuntu-26.04",
    ]) {
      expect(registry.has(name)).toBe(true);
    }
  });

  it("never registers a path that would have to guess a suite", () => {
    for (const name of [
      "docker",
      "docker.sources",
      "docker-raspbian",
      "microsoft",
      "microsoft-stonking",
      "microsoft-ubuntu-26.10",
      "microsoft-ubuntu-20.04",
      "microsoft-vscode",
      "microsoft-powershell",
      "microsoft-azure-cli-ubuntu-26.10",
      "kubernetes",
      "kubernetes-latest",
      "pigsty",
      "libnvidia-container-stable",
      "cuda",
      "cuda-ubuntu-20.04",
    ]) {
      expect(registry.has(name)).toBe(false);
    }
  });

  it("rejects duplicate and unsafe names", () => {
    const fresh = new AptPresetRegistry();
    const preset = aptPreset({ mount: "x", suites: ["stable"] });
    fresh.register("x", preset);
    expect(() => fresh.register("x", preset)).toThrow(/duplicate/);
    for (const bad of ["../x", "X", "a/b", ""]) {
      expect(() => fresh.register(bad, preset)).toThrow();
    }
  });
});

describe("flat presets", () => {
  const flatRegistry = new AptPresetRegistry().register(
    "cuda",
    aptPreset({
      mount: "cuda",
      flat: true,
      keyring: "/usr/share/keyrings/cuda.gpg",
    }),
  );
  const flat = flatRegistry.get("cuda")!;
  const renderFlat = (search = "") =>
    renderAptSource(flat, {
      origin: ORIGIN,
      query: new URLSearchParams(search),
      inlineKey: fakeKey,
    });

  it("uses Suites: / and omits Components", async () => {
    expect(await renderFlat()).toBe(
      [
        "Types: deb",
        `URIs: ${ORIGIN}/cuda/`,
        "Suites: /",
        "Signed-By: /usr/share/keyrings/cuda.gpg",
        "",
      ].join("\n"),
    );
  });

  it("accepts an explicit `/` suite", async () => {
    expect(await renderFlat("suites=/")).toContain("Suites: /\n");
  });

  it("rejects any other suite", async () => {
    await expect(renderFlat("suites=stable")).rejects.toThrow(AptSourceError);
  });

  it("rejects components, but allows the explicit empty value", async () => {
    await expect(renderFlat("components=main")).rejects.toThrow(AptSourceError);
    expect(await renderFlat("components=")).not.toContain("Components:");
  });

  it("surfaces a bad query as an AptQueryError", async () => {
    await expect(renderFlat("bogus=1")).rejects.toThrow(AptQueryError);
  });
});

describe("Docker presets", () => {
  const releases: [name: string, mount: string, suite: string][] = [
    ["docker-debian-12", "docker-ce/linux/debian", "bookworm"],
    ["docker-debian-13", "docker-ce/linux/debian", "trixie"],
    ["docker-ubuntu-22.04", "docker-ce/linux/ubuntu", "jammy"],
    ["docker-ubuntu-24.04", "docker-ce/linux/ubuntu", "noble"],
    ["docker-ubuntu-26.04", "docker-ce/linux/ubuntu", "resolute"],
    ["docker-ubuntu-26.10", "docker-ce/linux/ubuntu", "stonking"],
  ];

  for (const [name, mount, suite] of releases) {
    it(`${name} is one codename suite with stable and amd64`, async () => {
      expect(await render(name)).toBe(
        [
          "Types: deb",
          `URIs: ${ORIGIN}/${mount}/`,
          `Suites: ${suite}`,
          "Components: stable",
          "Architectures: amd64",
          inlineField("docker-ce"),
          "",
        ].join("\n"),
      );
    });
  }

  it("registers the codename as the same content as its version", async () => {
    const aliases: [version: string, codename: string][] = [
      ["debian-12", "debian-bookworm"],
      ["debian-13", "debian-trixie"],
      ["ubuntu-22.04", "ubuntu-jammy"],
      ["ubuntu-24.04", "ubuntu-noble"],
      ["ubuntu-26.04", "ubuntu-resolute"],
      ["ubuntu-26.10", "ubuntu-stonking"],
    ];
    for (const [version, codename] of aliases) {
      expect(await render(`docker-${version}`)).toBe(
        await render(`docker-${codename}`),
      );
    }
  });
});

describe("Microsoft presets", () => {
  const prod: [name: string, mount: string, suite: string][] = [
    [
      "microsoft-debian-bookworm",
      "microsoft/microsoft-debian-bookworm-prod",
      "bookworm",
    ],
    [
      "microsoft-debian-trixie",
      "microsoft/microsoft-debian-trixie-prod",
      "trixie",
    ],
    ["microsoft-ubuntu-jammy", "microsoft/microsoft-ubuntu-jammy-prod", "jammy"],
    ["microsoft-ubuntu-noble", "microsoft/microsoft-ubuntu-noble-prod", "noble"],
    [
      "microsoft-ubuntu-resolute",
      "microsoft/microsoft-ubuntu-resolute-prod",
      "resolute",
    ],
  ];

  for (const [name, mount, suite] of prod) {
    it(`${name} renders the prod tree from its own suite`, async () => {
      expect(await render(name)).toBe(
        [
          "Types: deb",
          `URIs: ${ORIGIN}/${mount}/`,
          `Suites: ${suite}`,
          "Components: main",
          "Architectures: amd64",
          inlineField("microsoft"),
          "",
        ].join("\n"),
      );
    });
  }

  for (const name of ["code", "edge"]) {
    it(`microsoft-${name} uses its single stable suite`, async () => {
      expect(await render(`microsoft-${name}`)).toBe(
        [
          "Types: deb",
          `URIs: ${ORIGIN}/microsoft/${name}/`,
          "Suites: stable",
          "Components: main",
          "Architectures: amd64",
          inlineField("microsoft"),
          "",
        ].join("\n"),
      );
    });
  }

  const azureCli: [name: string, suite: string][] = [
    ["microsoft-azure-cli-debian-bookworm", "bookworm"],
    ["microsoft-azure-cli-ubuntu-jammy", "jammy"],
    ["microsoft-azure-cli-ubuntu-noble", "noble"],
    ["microsoft-azure-cli-ubuntu-resolute", "resolute"],
  ];

  for (const [name, suite] of azureCli) {
    it(`${name} uses the codename suite on the azure-cli mount`, async () => {
      const file = await render(name);
      expect(file).toContain(`URIs: ${ORIGIN}/microsoft/azure-cli/\n`);
      expect(file).toContain(`Suites: ${suite}\n`);
      expect(file).toContain("Components: main\n");
      expect(file).toContain("Architectures: amd64\n");
      expect(file).toContain(inlineField("microsoft"));
    });
  }

  it("registers the codename as the same content as its version", async () => {
    expect(await render("microsoft-debian-12")).toBe(
      await render("microsoft-debian-bookworm"),
    );
    expect(await render("microsoft-debian-13")).toBe(
      await render("microsoft-debian-trixie"),
    );
    expect(await render("microsoft-ubuntu-26.04")).toBe(
      await render("microsoft-ubuntu-resolute"),
    );
    expect(await render("microsoft-azure-cli-ubuntu-24.04")).toBe(
      await render("microsoft-azure-cli-ubuntu-noble"),
    );
  });
});

describe("Pigsty presets", () => {
  it("renders the infra repository with its generic suite", async () => {
    expect(await render("pigsty-infra")).toBe(
      [
        "Types: deb",
        `URIs: ${ORIGIN}/pigsty/infra/`,
        "Suites: generic",
        "Components: main",
        "Architectures: amd64",
        inlineField("pigsty"),
        "",
      ].join("\n"),
    );
  });

  const pgsql: [name: string, codename: string][] = [
    ["pigsty-pgsql-debian-12", "bookworm"],
    ["pigsty-pgsql-debian-bookworm", "bookworm"],
    ["pigsty-pgsql-debian-13", "trixie"],
    ["pigsty-pgsql-debian-trixie", "trixie"],
    ["pigsty-pgsql-ubuntu-22.04", "jammy"],
    ["pigsty-pgsql-ubuntu-jammy", "jammy"],
    ["pigsty-pgsql-ubuntu-24.04", "noble"],
    ["pigsty-pgsql-ubuntu-noble", "noble"],
    ["pigsty-pgsql-ubuntu-26.04", "resolute"],
    ["pigsty-pgsql-ubuntu-resolute", "resolute"],
  ];

  for (const [name, codename] of pgsql) {
    it(`${name} points at its own pgsql root`, async () => {
      const file = await render(name);
      expect(file).toContain(`URIs: ${ORIGIN}/pigsty/pgsql/${codename}/\n`);
      expect(file).toContain(`Suites: ${codename}\n`);
      expect(file).toContain("Components: main\n");
      expect(file).toContain("Architectures: amd64\n");
      expect(file).toContain(inlineField("pigsty"));
    });
  }

  it("registers version and codename aliases for the same content", async () => {
    expect(await render("pigsty-pgsql-debian-12")).toBe(
      await render("pigsty-pgsql-debian-bookworm"),
    );
    expect(await render("pigsty-pgsql-ubuntu-26.04")).toBe(
      await render("pigsty-pgsql-ubuntu-resolute"),
    );
  });
});

describe("Kubernetes presets", () => {
  for (const minor of ["1.35", "1.36", "1.37"]) {
    it(`kubernetes-${minor} is the flat per-minor repository`, async () => {
      expect(entry(`kubernetes-${minor}`).flat).toBe(true);
      expect(await render(`kubernetes-${minor}`)).toBe(
        [
          "Types: deb",
          `URIs: ${ORIGIN}/kubernetes/core:/stable:/v${minor}/deb/`,
          "Suites: /",
          "Architectures: amd64",
          inlineField("kubernetes"),
          "",
        ].join("\n"),
      );
    });
  }
});

describe("libnvidia-container preset", () => {
  it("is flat and defaults to amd64", async () => {
    expect(entry("libnvidia-container").flat).toBe(true);
    expect(await render("libnvidia-container")).toBe(
      [
        "Types: deb",
        `URIs: ${ORIGIN}/libnvidia-container/stable/deb/amd64/`,
        "Suites: /",
        "Architectures: amd64",
        inlineField("libnvidia-container"),
        "",
      ].join("\n"),
    );
  });

  it("emits one stanza per architecture, in URI order", async () => {
    const file = await render(
      "libnvidia-container",
      "arch=amd64,arm64,ppc64el",
    );
    expect(stanzaCount(file)).toBe(3);
    for (const arch of ["amd64", "arm64", "ppc64el"]) {
      expect(file).toContain(
        `URIs: ${ORIGIN}/libnvidia-container/stable/deb/${arch}/\n`,
      );
      expect(file).toContain(`Architectures: ${arch}\n`);
    }
    expect(file).not.toContain("Components:");
  });

  it("rejects an architecture upstream does not publish", async () => {
    await expect(
      render("libnvidia-container", "arch=s390x"),
    ).rejects.toThrow(AptSourceError);
    await expect(
      render("libnvidia-container", "arch=s390x"),
    ).rejects.toThrow(/amd64, arm64, ppc64el/);
  });
});

describe("CUDA presets", () => {
  const releases: [
    name: string,
    dir: string,
    arm64Dir: string,
    legacyKey: boolean,
  ][] = [
    ["cuda-debian-12", "debian12", "sbsa", true],
    ["cuda-debian-13", "debian13", "sbsa", false],
    ["cuda-ubuntu-22.04", "ubuntu2204", "arm64", true],
    ["cuda-ubuntu-24.04", "ubuntu2404", "arm64", true],
    ["cuda-ubuntu-26.04", "ubuntu2604", "sbsa", false],
  ];

  for (const [name, dir, arm64Dir, legacyKey] of releases) {
    it(`${name} is flat and maps amd64 to x86_64`, async () => {
      expect(entry(name).flat).toBe(true);
      const file = await render(name);
      expect(file).toContain(`URIs: ${ORIGIN}/cuda/${dir}/x86_64/\n`);
      expect(file).toContain("Suites: /\n");
      expect(file).toContain("Architectures: amd64\n");
      expect(file).not.toContain("Components:");
      expect(file).toContain(inlineField("cuda"));
    });

    it(`${name} maps arm64 to ${arm64Dir}`, async () => {
      const file = await render(name, "arch=arm64");
      expect(file).toContain(`URIs: ${ORIGIN}/cuda/${dir}/${arm64Dir}/\n`);
      expect(file).toContain("Architectures: arm64\n");
    });

    it(`${name} ${legacyKey ? "warns about" : "does not warn about"} the old key`, async () => {
      const file = await render(name);
      if (legacyKey) {
        expect(file).toContain("SHA-1");
        expect(file).toContain("sqv");
        // The warning explains the problem; it must never weaken verification.
        expect(file).not.toContain("Trusted:");
        expect(file).not.toContain("[trusted=");
      } else {
        expect(file).not.toContain("SHA-1");
        expect(file).not.toContain("#");
      }
    });
  }

  it("emits one stanza per architecture with the right directories", async () => {
    const file = await render("cuda-debian-13", "arch=amd64,arm64");
    expect(stanzaCount(file)).toBe(2);
    expect(file).toContain(`URIs: ${ORIGIN}/cuda/debian13/x86_64/\n`);
    expect(file).toContain(`URIs: ${ORIGIN}/cuda/debian13/sbsa/\n`);
    expect(file).toContain("Architectures: arm64\n");
  });

  it("rejects an architecture upstream does not publish", async () => {
    await expect(render("cuda-ubuntu-22.04", "arch=ppc64el")).rejects.toThrow(
      AptSourceError,
    );
    await expect(render("cuda-ubuntu-22.04", "arch=ppc64el")).rejects.toThrow(
      /amd64, arm64/,
    );
  });

  it("registers version and codename aliases for the same content", async () => {
    expect(await render("cuda-debian-12")).toBe(await render("cuda-debian-bookworm"));
    expect(await render("cuda-debian-13")).toBe(await render("cuda-debian-trixie"));
    expect(await render("cuda-ubuntu-22.04")).toBe(
      await render("cuda-ubuntu-jammy"),
    );
    expect(await render("cuda-ubuntu-24.04")).toBe(
      await render("cuda-ubuntu-noble"),
    );
    expect(await render("cuda-ubuntu-26.04")).toBe(
      await render("cuda-ubuntu-resolute"),
    );
  });
});
