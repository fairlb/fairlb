/**
 * Turn the deliberately coarse User-Agent stored with a session into a short
 * label for security decisions. Unknown agents stay verbatim: a wrong device
 * guess is less useful than the original evidence.
 *
 * Token order matters because Chromium-family UAs also contain Safari and
 * mobile UAs commonly contain desktop compatibility tokens (`Mac OS X` or
 * `Linux`). Match the specific browser/platform before those fallbacks.
 */
export function sessionDeviceLabel(userAgent: string | undefined): string | undefined {
  if (!userAgent) return undefined;

  const browser = /Edg(?:A|iOS)?\//.test(userAgent)
    ? "Edge"
    : /OPR\/|Opera/.test(userAgent)
      ? "Opera"
      : /Firefox\/|FxiOS\//.test(userAgent)
        ? "Firefox"
        : /Chrome\/|CriOS\//.test(userAgent)
          ? "Chrome"
          : /Safari\//.test(userAgent)
            ? "Safari"
            : undefined;

  const os = /iPhone/.test(userAgent)
    ? "iPhone"
    : /iPad/.test(userAgent) || (/Macintosh/.test(userAgent) && /Mobile\//.test(userAgent))
      ? "iPad"
      : /Android/.test(userAgent)
        ? "Android"
        : /Macintosh|Mac OS X/.test(userAgent)
          ? "macOS"
          : /Windows/.test(userAgent)
            ? "Windows"
            : /Linux/.test(userAgent)
              ? "Linux"
              : undefined;

  if (!browser && !os) return userAgent;
  return [browser, os].filter(Boolean).join(" · ");
}
