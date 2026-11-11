---
name: readme-screenshots
description: Regenerate the README screenshots in docs/images from sample data only (dev-seed fixtures plus a fictional Claude spend report). Use when the UI changed and the README images are stale, or when asked to refresh or add screenshots.
---

# Refresh README screenshots

The recipe lives in `docs/screenshots.md`; follow it exactly. In short:

1. Start a **separate** Compose project with sample data and the fictional "Acme Robotics"
   branding (never the live stack, never a real spend report).
2. Generate the spend report with `node scripts/sample-spend-report.mjs <file>`.
3. Run `node scripts/readme-screenshots.mjs <baseUrl> <file> docs/images`. It refuses to capture
   unless the API reports fixture or demo data.
4. **Look at every image** before committing: no clipped charts, no hover tooltips, no sticky
   toolbar covering content, nothing but Acme/sample names.
5. Tear the project down with `docker compose --project-name <name> down -v`.

If you add a screenshot, add it to `scripts/readme-screenshots.mjs` (not a one-off command), so
the next refresh reproduces it. Keep images under ~250 KB each.
