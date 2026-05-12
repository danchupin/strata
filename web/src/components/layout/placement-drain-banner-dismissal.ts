// Pure helpers for PlacementDrainBanner's per-session dismissal flow.
// Extracted from the component body so the comparison logic stays
// straight-line testable from `node --test` without spinning up React.
//
// Semantics: the dismissal stamp is keyed to the SET of currently
// draining cluster ids (sorted, JSON-stringified). Dismissing pins the
// stamp; the banner stays hidden only while the live set produces the
// same stamp. A fresh cluster entering the draining set → stamp drifts
// → banner reappears. A cluster leaving the set produces a strict
// subset → stamp drifts → banner reappears so the operator notices the
// change of state (and re-dismisses if they don't care).

export const DISMISS_KEY = 'drain_banner_dismissed';

// stampForDrainingIds normalises a slice of cluster ids into the
// canonical localStorage stamp shape (sorted JSON array). Duplicate
// entries are deduplicated so the stamp matches set semantics.
export function stampForDrainingIds(ids: readonly string[]): string {
  const dedup = Array.from(new Set(ids));
  dedup.sort();
  return JSON.stringify(dedup);
}

// shouldHideBanner returns true when the dismissed stamp matches the
// stamp produced from the current draining set. Returns false when no
// dismissal is stored, when the stamp parses to a non-matching value,
// or when the JSON is malformed.
export function shouldHideBanner(
  currentDrainingIds: readonly string[],
  storedStamp: string | null,
): boolean {
  if (!storedStamp) return false;
  return stampForDrainingIds(currentDrainingIds) === storedStamp;
}

export function readDismissedStamp(): string | null {
  if (typeof window === 'undefined') return null;
  try {
    return window.localStorage.getItem(DISMISS_KEY);
  } catch {
    return null;
  }
}

export function writeDismissedStamp(stamp: string): void {
  if (typeof window === 'undefined') return;
  try {
    window.localStorage.setItem(DISMISS_KEY, stamp);
  } catch {
    // localStorage may throw under privacy modes — silently degrade so
    // the banner just keeps showing rather than crashing the shell.
  }
}
