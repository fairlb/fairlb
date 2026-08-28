export type BrandLocale = "en" | "zh";

export type LocalizedText = Record<BrandLocale, string>;

export type LocalizedLink = {
  label: LocalizedText;
  href: string;
};

export type BrandColors = {
  canvas: string;
  surface: string;
  ink: string;
  accent: string;
  healthy: string;
  degraded: string;
};

export type LocalFontSource = {
  path: string;
  weight: string;
  style: "normal" | "italic";
};

export type LocalFont = {
  family: string;
  sources: LocalFontSource[];
};

/**
 * The id of the JSON island a served page carries its profile in.
 *
 * It lives here rather than next to either user because both the browser entry
 * and the build plugin need it and neither may import the other: `build.ts`
 * pulls in `node:fs`.
 */
export const RUNTIME_PROFILE_ELEMENT_ID = "brand-profile";

export interface BrandProfileV1 {
  profileVersion: 1;
  identity: {
    name: string;
    shortName: string;
    surfaceNames: {
      marketing: LocalizedText;
      console: LocalizedText;
      operations: LocalizedText;
      communityAdmin: LocalizedText;
    };
    assets: {
      wordmarkSvg: string;
      markSvg: string;
      faviconSvg: string;
      socialMarkSvg: string;
    };
  };
  theme: {
    light: BrandColors;
    dark: BrandColors;
    fonts: {
      display: LocalFont;
      body: LocalFont;
      mono: LocalFont;
      chineseFallback: string[];
    };
  };
  operator: {
    legalName: string;
    supportEmail: string;
    salesEmail?: string;
  };
  links: {
    repository: string;
    deploymentDocs: string;
  };
  marketing: {
    tagline: LocalizedText;
    primaryCta: LocalizedLink;
    secondaryCta: LocalizedLink;
    offerings: {
      cloud: boolean;
      community: boolean;
      privateDeployment: false;
    };
    editorial: {
      blog: boolean;
      changelog: boolean;
      /**
       * Publish the developer guides bundled in `public/docs/`.
       *
       * A switch rather than a name substitution: those documents name the
       * `fairlb` binary, `FAIRLB_API_KEY`, the database role. Projecting the
       * display name onto their prose would leave every command still saying
       * `fairlb`, which is documentation that does not work.
       *
       * **That argument justifies not substituting, not withholding.** Read
       * through, every one of those occurrences is a machine interface — the
       * binary's own name, an environment variable it reads, an image or
       * repository address, the database role, or a provider key the reader
       * chooses in their own Codex/OpenCode config. ADR-0147 decision 6 already
       * rules that class stable across brands. A deployment under another name
       * runs the same binary with the same variables, so the guides are correct
       * for it verbatim; what it needs is one sentence saying so, which the
       * docs pages render (`docsIdentifierNote`). Publishing them is the
       * default. Turn this off only for a deployment that has written its own.
       */
      guides: boolean;
    };
    seo: {
      siteName: LocalizedText;
      defaultDescription: LocalizedText;
    };
  };
}

export type BrandSurface = keyof BrandProfileV1["identity"]["surfaceNames"];

export function interpolateBrandText(profile: BrandProfileV1, value: string): string {
  const tokens = {
    brand: profile.identity.name,
    shortBrand: profile.identity.shortName,
    operator: profile.operator.legalName,
    supportEmail: profile.operator.supportEmail,
    salesEmail: profile.operator.salesEmail ?? profile.operator.supportEmail,
  } as const;
  const rendered = value.replace(/\{([A-Za-z][A-Za-z0-9]*)\}/g, (_, key: string) => {
    if (!(key in tokens)) throw new Error(`brand profile: unknown content variable {${key}}`);
    return tokens[key as keyof typeof tokens];
  });
  if (/[{}]/.test(rendered)) {
    throw new Error(`brand profile: unresolved content variable in ${JSON.stringify(value)}`);
  }
  return rendered;
}

/**
 * The tracked FairLB profile is also the schema example for private builds.
 * Paths are relative to this module and are resolved only by the build loader.
 */
export const DEFAULT_BRAND_PROFILE_SOURCE = {
  profileVersion: 1,
  identity: {
    name: "FairLB",
    shortName: "FairLB",
    surfaceNames: {
      marketing: { en: "FairLB", zh: "FairLB" },
      console: { en: "FairLB Console", zh: "FairLB 控制台" },
      operations: { en: "FairLB Operations", zh: "FairLB 运营后台" },
      communityAdmin: { en: "FairLB Admin", zh: "FairLB 管理台" },
    },
    assets: {
      wordmarkSvg: "./mark.svg",
      markSvg: "./mark.svg",
      faviconSvg: "./mark.svg",
      socialMarkSvg: "./mark.svg",
    },
  },
  theme: {
    light: {
      canvas: "#F5F7FA",
      surface: "#FFFFFF",
      ink: "#141821",
      accent: "#2457E6",
      healthy: "#0B8F83",
      degraded: "#B86E00",
    },
    dark: {
      canvas: "#0B1018",
      surface: "#131A24",
      ink: "#EEF3F8",
      accent: "#7EA2FF",
      healthy: "#4FD1C5",
      degraded: "#F4B860",
    },
    fonts: {
      display: {
        family: "Archivo Variable",
        sources: [
          {
            path: "../node_modules/@fontsource-variable/archivo/files/archivo-latin-wght-normal.woff2",
            weight: "100 900",
            style: "normal",
          },
        ],
      },
      body: {
        family: "Source Sans 3 Variable",
        sources: [
          {
            path: "../node_modules/@fontsource-variable/source-sans-3/files/source-sans-3-latin-wght-normal.woff2",
            weight: "200 900",
            style: "normal",
          },
        ],
      },
      mono: {
        family: "IBM Plex Mono",
        sources: [
          {
            path: "../node_modules/@fontsource/ibm-plex-mono/files/ibm-plex-mono-latin-400-normal.woff2",
            weight: "400",
            style: "normal",
          },
          {
            path: "../node_modules/@fontsource/ibm-plex-mono/files/ibm-plex-mono-latin-500-normal.woff2",
            weight: "500",
            style: "normal",
          },
        ],
      },
      chineseFallback: ["PingFang SC", "Noto Sans SC", "system-ui", "sans-serif"],
    },
  },
  operator: {
    legalName: "FairLB",
    supportEmail: "support@fairlb.com",
    salesEmail: "sales@fairlb.com",
  },
  links: {
    repository: "https://github.com/fairlb/fairlb",
    deploymentDocs: "https://github.com/fairlb/fairlb/tree/main/docs",
  },
  marketing: {
    // The Chinese hero heading is `word-break: keep-all` (Base.astro), so it
    // only breaks at punctuation -- a Chinese tagline has to keep each run
    // between commas short or it overflows a 320px viewport. The narrow-geometry
    // test is what catches it; the English line wraps at spaces and does not
    // have this constraint.
    tagline: {
      en: "Native where there is a protocol. One API where there isn't.",
      zh: "有原生协议，就原样直通；没有的，我们自己定一套。",
    },
    primaryCta: {
      label: { en: "Start building", zh: "开始接入" },
      href: "/register",
    },
    secondaryCta: {
      label: { en: "Send your first request", zh: "发送第一个请求" },
      href: "/docs/#quickstart",
    },
    offerings: { cloud: true, community: true, privateDeployment: false },
    editorial: { blog: true, changelog: true, guides: true },
    seo: {
      siteName: { en: "FairLB", zh: "FairLB" },
      defaultDescription: {
        en: "Native OpenAI, Anthropic and Gemini APIs for text and images; one asynchronous job API for video. Automatic failover, and every charge attached to the record that produced it.",
        zh: "文本与图像走原生 OpenAI、Anthropic 与 Gemini API，视频走一套统一的异步任务 API。自动故障转移，每一笔费用都记在产生它的那条记录上。",
      },
    },
  },
} as const satisfies BrandProfileV1;

const defaultAsset = (name: string) => new URL(name, import.meta.url).href;

export const DEFAULT_BRAND_PROFILE: BrandProfileV1 = {
  ...DEFAULT_BRAND_PROFILE_SOURCE,
  identity: {
    ...DEFAULT_BRAND_PROFILE_SOURCE.identity,
    assets: {
      wordmarkSvg: defaultAsset("./mark.svg"),
      markSvg: defaultAsset("./mark.svg"),
      faviconSvg: defaultAsset("./mark.svg"),
      socialMarkSvg: defaultAsset("./mark.svg"),
    },
  },
  theme: {
    ...DEFAULT_BRAND_PROFILE_SOURCE.theme,
    fonts: {
      ...DEFAULT_BRAND_PROFILE_SOURCE.theme.fonts,
      display: { ...DEFAULT_BRAND_PROFILE_SOURCE.theme.fonts.display, sources: [] },
      body: { ...DEFAULT_BRAND_PROFILE_SOURCE.theme.fonts.body, sources: [] },
      mono: { ...DEFAULT_BRAND_PROFILE_SOURCE.theme.fonts.mono, sources: [] },
      chineseFallback: [...DEFAULT_BRAND_PROFILE_SOURCE.theme.fonts.chineseFallback],
    },
  },
};
