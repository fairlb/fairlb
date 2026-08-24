# Vendor marks

This directory is intentionally empty of third-party logos. `VendorMark` falls
back to a generated two-letter tile for every registry vendor, which is the safe
and complete default.

An SVG may be added as `{vendor-id}.svg` only when its reuse terms are clear:

- Simple Icons artwork may be used when the individual icon is published under
  Simple Icons' CC0 terms.
- Any other artwork needs explicit reuse permission in the vendor's official
  brand kit. Do not redraw, approximate, trace, or generate a vendor trademark.
- Preserve the supplied geometry and use the exact vendor slug from
  `internal/gateway/catalog/vendors.go` as the filename.
- Add the source URL, owner, and license or permission to the third-party section
  of `TRADEMARKS.md` in the same change.
- Add the slug and its served URL to `registry.ts`. Adding the file alone does
  nothing on purpose: a directory glob would have made "drop an SVG in" enough to
  ship a trademark, and the licence entry above is the point of the exercise.

Absence is not an incomplete state: keep the monogram tile when the provenance
or permission is unclear.
