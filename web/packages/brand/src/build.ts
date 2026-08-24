import { readFileSync, statSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, extname, resolve } from "node:path";
import type { BrandColors, BrandProfileV1, BrandSurface, LocalFont } from "./profile";
// @ts-expect-error Vite's Node ESM config runner requires the real TypeScript extension here.
import { DEFAULT_BRAND_PROFILE_SOURCE } from "./profile.ts";

type Asset = { fileName: string; source: Uint8Array };

export type LoadedBrandProfile = {
  profile: BrandProfileV1;
  css: string;
  assets: Asset[];
};

const profileRoot = dirname(fileURLToPath(import.meta.url));
const HEX = /^#[0-9a-f]{6}$/i;
const EMAIL = /^[^\s@]+@[^\s@]+\.[^\s@]+$/;

const keys = <T extends object>(value: T) => Object.keys(value) as (keyof T)[];

function object(value: unknown, label: string): Record<string, unknown> {
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    throw new Error(`brand profile: ${label} must be an object`);
  }
  return value as Record<string, unknown>;
}

function exact(value: unknown, label: string, allowed: readonly string[]): Record<string, unknown> {
  const result = object(value, label);
  const unknown = Object.keys(result).filter((key) => !allowed.includes(key));
  if (unknown.length)
    throw new Error(`brand profile: ${label} has unknown fields: ${unknown.join(", ")}`);
  const missing = allowed.filter((key) => !(key in result));
  if (missing.length) throw new Error(`brand profile: ${label} is missing: ${missing.join(", ")}`);
  return result;
}

function nonempty(value: unknown, label: string): string {
  if (typeof value !== "string" || !value.trim()) {
    throw new Error(`brand profile: ${label} must be a non-empty string`);
  }
  return value.trim();
}

function bool(value: unknown, label: string): boolean {
  if (typeof value !== "boolean") throw new Error(`brand profile: ${label} must be boolean`);
  return value;
}

function localized(value: unknown, label: string) {
  const item = exact(value, label, ["en", "zh"]);
  return { en: nonempty(item.en, `${label}.en`), zh: nonempty(item.zh, `${label}.zh`) };
}

function url(value: unknown, label: string, allowRelative = false): string {
  const candidate = nonempty(value, label);
  if (allowRelative && candidate.startsWith("/")) return candidate;
  let parsed: URL;
  try {
    parsed = new URL(candidate);
  } catch {
    throw new Error(
      `brand profile: ${label} must be an absolute http(s) URL${allowRelative ? " or a root-relative path" : ""}`,
    );
  }
  if (!new Set(["http:", "https:"]).has(parsed.protocol)) {
    throw new Error(`brand profile: ${label} must use http(s)`);
  }
  return parsed.href;
}

function hex(value: unknown, label: string): string {
  const candidate = nonempty(value, label);
  if (!HEX.test(candidate)) throw new Error(`brand profile: ${label} must be #RRGGBB`);
  return candidate.toUpperCase();
}

function channel(value: string): number {
  const n = Number.parseInt(value, 16) / 255;
  return n <= 0.04045 ? n / 12.92 : ((n + 0.055) / 1.055) ** 2.4;
}

function luminance(color: string): number {
  return (
    0.2126 * channel(color.slice(1, 3)) +
    0.7152 * channel(color.slice(3, 5)) +
    0.0722 * channel(color.slice(5, 7))
  );
}

export function contrast(a: string, b: string): number {
  const [lighter, darker] = [luminance(a), luminance(b)].sort((x, y) => y - x);
  return (lighter! + 0.05) / (darker! + 0.05);
}

function colors(value: unknown, label: string): BrandColors {
  const item = exact(value, label, ["canvas", "surface", "ink", "accent", "healthy", "degraded"]);
  const result = Object.fromEntries(
    keys(item).map((key) => [key, hex(item[key], `${label}.${String(key)}`)]),
  ) as BrandColors;
  for (const background of [result.canvas, result.surface]) {
    if (contrast(result.ink, background) < 4.5) {
      throw new Error(
        `brand profile: ${label}.ink must meet AA contrast against canvas and surface`,
      );
    }
  }
  if (contrast(result.accent, result.canvas) < 4.5) {
    throw new Error(`brand profile: ${label}.accent must meet AA contrast against canvas`);
  }
  for (const semantic of [result.healthy, result.degraded]) {
    if (contrast(semantic, result.canvas) < 3) {
      throw new Error(`brand profile: ${label} semantic colors must reach 3:1 against canvas`);
    }
  }
  return result;
}

function localFile(path: unknown, base: string, label: string, extension: string): string {
  const candidate = nonempty(path, label);
  if (/^(?:https?:)?\/\//i.test(candidate))
    throw new Error(`brand profile: ${label} must be local`);
  const absolute = resolve(base, candidate);
  if (extname(absolute).toLowerCase() !== extension || !statSync(absolute).isFile()) {
    throw new Error(`brand profile: ${label} must point to an existing ${extension} file`);
  }
  return absolute;
}

function svgAsset(path: unknown, base: string, label: string, fileName: string): Asset {
  const absolute = localFile(path, base, label, ".svg");
  const source = readFileSync(absolute);
  if (!/^\s*<svg\b/i.test(source.toString("utf8"))) {
    throw new Error(`brand profile: ${label} is not an SVG document`);
  }
  return { fileName, source };
}

function font(
  value: unknown,
  base: string,
  label: string,
  slug: string,
): { runtime: LocalFont; assets: Asset[] } {
  const item = exact(value, label, ["family", "sources"]);
  if (!Array.isArray(item.sources) || !item.sources.length) {
    throw new Error(`brand profile: ${label}.sources must be a non-empty array`);
  }
  const assets: Asset[] = [];
  const sources = item.sources.map((raw, index) => {
    const source = exact(raw, `${label}.sources[${index}]`, ["path", "weight", "style"]);
    const absolute = localFile(source.path, base, `${label}.sources[${index}].path`, ".woff2");
    const bytes = readFileSync(absolute);
    if (bytes.subarray(0, 4).toString("ascii") !== "wOF2") {
      throw new Error(`brand profile: ${label}.sources[${index}] is not a WOFF2 file`);
    }
    const rawStyle = nonempty(source.style, `${label}.sources[${index}].style`);
    if (rawStyle !== "normal" && rawStyle !== "italic") {
      throw new Error(`brand profile: ${label}.sources[${index}].style must be normal or italic`);
    }
    const style: "normal" | "italic" = rawStyle;
    const fileName = `brand/fonts/${slug}-${index}.woff2`;
    assets.push({ fileName, source: bytes });
    return {
      path: `/${fileName}`,
      weight: nonempty(source.weight, `${label}.sources[${index}].weight`),
      style,
    };
  });
  return { runtime: { family: nonempty(item.family, `${label}.family`), sources }, assets };
}

function cssString(value: string): string {
  return JSON.stringify(value);
}

function foreground(background: string): string {
  return contrast("#FFFFFF", background) >= contrast("#000000", background) ? "#FFFFFF" : "#000000";
}

function profileCss(profile: BrandProfileV1): string {
  const faces = (["display", "body", "mono"] as const).flatMap((role) =>
    profile.theme.fonts[role].sources.map(
      (source) =>
        `@font-face{font-family:${cssString(profile.theme.fonts[role].family)};font-style:${source.style};font-display:swap;font-weight:${source.weight};src:url(${cssString(source.path)}) format("woff2")}`,
    ),
  );
  const theme = (name: "light" | "dark") => {
    const value = profile.theme[name];
    return [
      `--flb-profile-${name}-canvas:${value.canvas}`,
      `--flb-profile-${name}-surface:${value.surface}`,
      `--flb-profile-${name}-ink:${value.ink}`,
      `--flb-profile-${name}-route:${value.accent}`,
      `--flb-profile-${name}-route-hover:color-mix(in srgb,${value.accent} 84%,${value.ink})`,
      `--flb-profile-${name}-on-route:${foreground(value.accent)}`,
      `--flb-profile-${name}-healthy:${value.healthy}`,
      `--flb-profile-${name}-degraded:${value.degraded}`,
    ].join(";");
  };
  const fallback = profile.theme.fonts.chineseFallback.map(cssString).join(",");
  return `${faces.join("\n")}\n:root{${theme("light")};${theme("dark")};--flb-profile-font-display:${cssString(profile.theme.fonts.display.family)},${fallback};--flb-profile-font-ui:${cssString(profile.theme.fonts.body.family)},${fallback};--flb-profile-font-mono:${cssString(profile.theme.fonts.mono.family)},ui-monospace,monospace}\n`;
}

function validate(raw: unknown, base: string): LoadedBrandProfile {
  const root = exact(raw, "root", [
    "profileVersion",
    "identity",
    "theme",
    "operator",
    "links",
    "marketing",
  ]);
  if (root.profileVersion !== 1) throw new Error("brand profile: profileVersion must be 1");

  const identity = exact(root.identity, "identity", [
    "name",
    "shortName",
    "surfaceNames",
    "assets",
  ]);
  const surfaceNames = exact(identity.surfaceNames, "identity.surfaceNames", [
    "marketing",
    "console",
    "operations",
    "communityAdmin",
  ]);
  const sourceAssets = exact(identity.assets, "identity.assets", [
    "wordmarkSvg",
    "markSvg",
    "faviconSvg",
    "socialMarkSvg",
  ]);
  const assets = [
    svgAsset(sourceAssets.wordmarkSvg, base, "identity.assets.wordmarkSvg", "brand/wordmark.svg"),
    svgAsset(sourceAssets.markSvg, base, "identity.assets.markSvg", "brand/mark.svg"),
    svgAsset(sourceAssets.faviconSvg, base, "identity.assets.faviconSvg", "brand/favicon.svg"),
    svgAsset(
      sourceAssets.socialMarkSvg,
      base,
      "identity.assets.socialMarkSvg",
      "brand/social-mark.svg",
    ),
  ];

  const theme = exact(root.theme, "theme", ["light", "dark", "fonts"]);
  const fonts = exact(theme.fonts, "theme.fonts", ["display", "body", "mono", "chineseFallback"]);
  if (!Array.isArray(fonts.chineseFallback) || !fonts.chineseFallback.length) {
    throw new Error("brand profile: theme.fonts.chineseFallback must be a non-empty array");
  }
  const display = font(fonts.display, base, "theme.fonts.display", "display");
  const body = font(fonts.body, base, "theme.fonts.body", "body");
  const mono = font(fonts.mono, base, "theme.fonts.mono", "mono");
  assets.push(...display.assets, ...body.assets, ...mono.assets);

  const operator = object(root.operator, "operator");
  const operatorAllowed = ["legalName", "supportEmail", "salesEmail"];
  const operatorUnknown = Object.keys(operator).filter((key) => !operatorAllowed.includes(key));
  if (operatorUnknown.length)
    throw new Error(`brand profile: operator has unknown fields: ${operatorUnknown.join(", ")}`);
  const supportEmail = nonempty(operator.supportEmail, "operator.supportEmail");
  if (!EMAIL.test(supportEmail))
    throw new Error("brand profile: operator.supportEmail must be an email");
  const salesEmail =
    operator.salesEmail === undefined
      ? undefined
      : nonempty(operator.salesEmail, "operator.salesEmail");
  if (salesEmail && !EMAIL.test(salesEmail))
    throw new Error("brand profile: operator.salesEmail must be an email");

  const links = exact(root.links, "links", ["repository", "deploymentDocs"]);
  const marketing = exact(root.marketing, "marketing", [
    "tagline",
    "primaryCta",
    "secondaryCta",
    "offerings",
    "editorial",
    "seo",
  ]);
  const primaryCta = exact(marketing.primaryCta, "marketing.primaryCta", ["label", "href"]);
  const secondaryCta = exact(marketing.secondaryCta, "marketing.secondaryCta", ["label", "href"]);
  const offerings = exact(marketing.offerings, "marketing.offerings", [
    "cloud",
    "community",
    "privateDeployment",
  ]);
  if (offerings.privateDeployment !== false) {
    throw new Error(
      "brand profile: marketing.offerings.privateDeployment must remain false until the product exists",
    );
  }
  const editorial = exact(marketing.editorial, "marketing.editorial", [
    "blog",
    "changelog",
    "guides",
  ]);
  const seo = exact(marketing.seo, "marketing.seo", ["siteName", "defaultDescription"]);

  const profile: BrandProfileV1 = {
    profileVersion: 1,
    identity: {
      name: nonempty(identity.name, "identity.name"),
      shortName: nonempty(identity.shortName, "identity.shortName"),
      surfaceNames: {
        marketing: localized(surfaceNames.marketing, "identity.surfaceNames.marketing"),
        console: localized(surfaceNames.console, "identity.surfaceNames.console"),
        operations: localized(surfaceNames.operations, "identity.surfaceNames.operations"),
        communityAdmin: localized(
          surfaceNames.communityAdmin,
          "identity.surfaceNames.communityAdmin",
        ),
      },
      assets: {
        wordmarkSvg: "/brand/wordmark.svg",
        markSvg: "/brand/mark.svg",
        faviconSvg: "/brand/favicon.svg",
        socialMarkSvg: "/brand/social-mark.svg",
      },
    },
    theme: {
      light: colors(theme.light, "theme.light"),
      dark: colors(theme.dark, "theme.dark"),
      fonts: {
        display: display.runtime,
        body: body.runtime,
        mono: mono.runtime,
        chineseFallback: fonts.chineseFallback.map((value, index) =>
          nonempty(value, `theme.fonts.chineseFallback[${index}]`),
        ),
      },
    },
    operator: {
      legalName: nonempty(operator.legalName, "operator.legalName"),
      supportEmail,
      ...(salesEmail ? { salesEmail } : {}),
    },
    links: {
      repository: url(links.repository, "links.repository"),
      deploymentDocs: url(links.deploymentDocs, "links.deploymentDocs"),
    },
    marketing: {
      tagline: localized(marketing.tagline, "marketing.tagline"),
      primaryCta: {
        label: localized(primaryCta.label, "marketing.primaryCta.label"),
        href: url(primaryCta.href, "marketing.primaryCta.href", true),
      },
      secondaryCta: {
        label: localized(secondaryCta.label, "marketing.secondaryCta.label"),
        href: url(secondaryCta.href, "marketing.secondaryCta.href", true),
      },
      offerings: {
        cloud: bool(offerings.cloud, "marketing.offerings.cloud"),
        community: bool(offerings.community, "marketing.offerings.community"),
        privateDeployment: false,
      },
      editorial: {
        blog: bool(editorial.blog, "marketing.editorial.blog"),
        changelog: bool(editorial.changelog, "marketing.editorial.changelog"),
        guides: bool(editorial.guides, "marketing.editorial.guides"),
      },
      seo: {
        siteName: localized(seo.siteName, "marketing.seo.siteName"),
        defaultDescription: localized(seo.defaultDescription, "marketing.seo.defaultDescription"),
      },
    },
  };
  return { profile, css: profileCss(profile), assets };
}

export function loadBrandProfile(profilePath = process.env.BRAND_PROFILE_PATH): LoadedBrandProfile {
  if (!profilePath) return validate(DEFAULT_BRAND_PROFILE_SOURCE, profileRoot);
  const absolute = resolve(profilePath);
  return validate(JSON.parse(readFileSync(absolute, "utf8")) as unknown, dirname(absolute));
}

export function brandBuild(surface: BrandSurface, profilePath?: string) {
  const loaded = loadBrandProfile(profilePath);
  if (
    surface === "marketing" &&
    process.env.MARKETING_ENV === "production" &&
    /not configured|placeholder|todo|example/i.test(loaded.profile.operator.legalName)
  ) {
    throw new Error("brand profile: production marketing builds require a real operator.legalName");
  }
  const title = loaded.profile.identity.surfaceNames[surface].en;
  const manifest = JSON.stringify({
    name: title,
    short_name: loaded.profile.identity.shortName,
    start_url: "/",
    display: surface === "marketing" ? "standalone" : "browser",
    background_color: loaded.profile.theme.light.canvas,
    theme_color: loaded.profile.theme.light.accent,
    icons: [
      {
        src: loaded.profile.identity.assets.faviconSvg,
        sizes: "any",
        type: "image/svg+xml",
        purpose: "any",
      },
    ],
  });
  return {
    profile: loaded.profile,
    define: { __FAIRLB_BRAND_PROFILE__: JSON.stringify(loaded.profile) },
    plugin: {
      name: "fairlb-brand-profile",
      configureServer(server: {
        middlewares: {
          use(
            handler: (
              req: { url?: string },
              res: {
                statusCode: number;
                setHeader(name: string, value: string): void;
                end(body: Uint8Array | string): void;
              },
              next: () => void,
            ) => void,
          ): void;
        };
      }) {
        const byPath = new Map(loaded.assets.map((asset) => [`/${asset.fileName}`, asset]));
        server.middlewares.use((req, res, next) => {
          if (surface !== "marketing" && req.url === "/site.webmanifest") {
            res.statusCode = 200;
            res.setHeader("Content-Type", "application/manifest+json; charset=utf-8");
            res.end(manifest);
            return;
          }
          if (req.url === "/brand/profile.css") {
            res.statusCode = 200;
            res.setHeader("Content-Type", "text/css; charset=utf-8");
            res.end(loaded.css);
            return;
          }
          const asset = byPath.get(req.url ?? "");
          if (!asset) return next();
          res.statusCode = 200;
          res.setHeader(
            "Content-Type",
            asset.fileName.endsWith(".svg") ? "image/svg+xml" : "font/woff2",
          );
          res.end(asset.source);
        });
      },
      generateBundle(this: {
        emitFile(value: { type: "asset"; fileName: string; source: Uint8Array | string }): void;
      }) {
        this.emitFile({ type: "asset", fileName: "brand/profile.css", source: loaded.css });
        if (surface !== "marketing") {
          this.emitFile({ type: "asset", fileName: "site.webmanifest", source: manifest });
        }
        for (const asset of loaded.assets) this.emitFile({ type: "asset", ...asset });
      },
      transformIndexHtml(html: string) {
        const colors = loaded.profile.theme;
        return html
          .replace(/<title>.*?<\/title>/s, `<title>${title}</title>`)
          .replaceAll('href="/favicon.svg"', 'href="/brand/favicon.svg"')
          .replace(/<link rel="icon" href="\/favicon\.ico"[^>]*>\s*/g, "")
          .replace(/<link rel="apple-touch-icon"[^>]*>\s*/g, "")
          .replace("#F5F7FA", colors.light.canvas)
          .replace("#0B1018", colors.dark.canvas)
          .replace(
            "</head>",
            `${surface === "marketing" ? "" : '  <link rel="manifest" href="/site.webmanifest" />\n'}  <link rel="stylesheet" href="/brand/profile.css" />\n  </head>`,
          );
      },
    },
  };
}
