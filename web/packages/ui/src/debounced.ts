import { useEffect, useState } from "react";

/**
 * The value, held still until it stops changing.
 *
 * For search boxes whose query goes to the server: without it every keystroke
 * is a request, and the answers can arrive out of order, so the list settles on
 * whichever response happened to be slowest rather than on the last thing typed.
 *
 * The delay is a parameter with no default. A picker that queries a database
 * wants a couple of hundred milliseconds; something cheaper wants less, and
 * making the caller say which keeps the number next to the reason for it.
 */
export function useDebounced<T>(value: T, delayMs: number): T {
  const [settled, setSettled] = useState(value);
  useEffect(() => {
    const timer = setTimeout(() => setSettled(value), delayMs);
    return () => clearTimeout(timer);
  }, [value, delayMs]);
  return settled;
}
