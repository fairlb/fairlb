import { CheckIcon, CopyIcon, WarningCircleIcon } from "@phosphor-icons/react";
import { useI18n } from "@fairlb/i18n";
import { useCallback, useEffect, useRef, useState } from "react";
import { Button, type ButtonProps } from "./button";

export type ClipboardStatus = "idle" | "copied" | "error";

async function writeClipboard(text: string): Promise<void> {
  if (typeof navigator !== "undefined" && navigator.clipboard?.writeText) {
    await navigator.clipboard.writeText(text);
    return;
  }
  // localhost/insecure contexts may not expose Clipboard API. Keep a small compatibility
  // fallback so a copy action does not silently become a no-op.
  const textarea = document.createElement("textarea");
  textarea.value = text;
  textarea.readOnly = true;
  textarea.style.position = "fixed";
  textarea.style.opacity = "0";
  document.body.append(textarea);
  textarea.select();
  const copied = document.execCommand("copy");
  textarea.remove();
  if (!copied) throw new Error("Clipboard write failed");
}

function useClipboard({ resetAfter = 2_000 }: { resetAfter?: number } = {}) {
  const [status, setStatus] = useState<ClipboardStatus>("idle");
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  const mounted = useRef(true);
  const operation = useRef(0);

  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
      operation.current += 1;
      clearTimeout(timer.current);
    };
  }, []);

  const reset = useCallback(() => {
    operation.current += 1;
    clearTimeout(timer.current);
    if (mounted.current) setStatus("idle");
  }, []);

  const copy = useCallback(
    async (text: string) => {
      const current = ++operation.current;
      clearTimeout(timer.current);
      try {
        await writeClipboard(text);
        if (!mounted.current || current !== operation.current) return undefined;
        setStatus("copied");
        timer.current = setTimeout(() => {
          if (mounted.current && current === operation.current) setStatus("idle");
        }, resetAfter);
        return true;
      } catch {
        if (!mounted.current || current !== operation.current) return undefined;
        setStatus("error");
        timer.current = setTimeout(() => {
          if (mounted.current && current === operation.current) setStatus("idle");
        }, resetAfter);
        return false;
      }
    },
    [resetAfter],
  );

  return { status, copy, reset } as const;
}

type TextButtonProps = Extract<ButtonProps, { shape?: "base" }>;

export type CopyActionProps = Omit<TextButtonProps, "children" | "onClick"> & {
  text: string;
  copyLabel?: string;
  copiedLabel?: string;
  errorLabel?: string;
  onCopied?: () => void;
  onCopyError?: () => void;
};

/** One copy button: the visible feedback, the failure feedback and the polite
 * screen-reader announcement all come from the same state machine. */
export function CopyAction({
  text,
  copyLabel,
  copiedLabel,
  errorLabel,
  onCopied,
  onCopyError,
  variant = "outline",
  size = "sm",
  ...props
}: CopyActionProps) {
  const { t } = useI18n();
  const clipboard = useClipboard();
  const labels = {
    idle: copyLabel ?? t("copy"),
    copied: copiedLabel ?? t("copied"),
    error: errorLabel ?? t("copyFailed"),
  };
  const Icon =
    clipboard.status === "copied"
      ? CheckIcon
      : clipboard.status === "error"
        ? WarningCircleIcon
        : CopyIcon;
  const label = labels[clipboard.status];

  return (
    <>
      <Button
        {...props}
        variant={variant}
        size={size}
        aria-label={label}
        onClick={() => {
          void clipboard.copy(text).then((ok) => {
            if (ok === true) onCopied?.();
            if (ok === false) onCopyError?.();
          });
        }}
      >
        <Icon aria-hidden />
        <span>{label}</span>
      </Button>
      <span className="sr-only" role="status" aria-live="polite">
        {clipboard.status === "idle" ? "" : label}
      </span>
    </>
  );
}
