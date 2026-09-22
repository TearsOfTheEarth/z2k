# Whitelist bulk editor

Scope: existing “Исключения → Домены” page, no new runtime dependencies.

The page supports case-insensitive filtering, checkbox selection, Shift ranges
within visible rows, selecting search results, deleting selected rows, clearing
all domain entries, editing selected entries or the whole document, and one-level
undo. Hidden selected rows remain selected and count toward the visible total.
Selecting a range never selects hidden rows. Selected replacement preserves all
unselected lines and comments; clear removes domains and retains comments.

The text editor keeps its draft after validation/network/conflict errors.
Mutations disable other controls while running; async responses are ignored
once the page has been replaced. A shorter saved list and bulk deletion require
confirmation. Undo lasts within this page session and is revision-guarded; it
cannot overwrite another tab's subsequent changes. Add/import now use the same
transaction, with whole-import rejection on invalid lines instead of partial
acceptance. Existing add/import/delete API routes remain available.

GET /whitelist adds text and a SHA-256 revision to its existing domains response.
POST /whitelist/save?revision=<hash> accepts up to 1 MiB of text, validates all
domains, normalizes case/outer whitespace, removes duplicates, acquires the same
list lock as old mutations, compares the revision, and atomically renames a 0644
candidate over the file. Stale revisions return HTTP 409; invalid text returns
400; write failures return 500. No service restart is involved. Trailing blank
lines are not preserved by the JSON snapshot; domain entries and comments are.
External writers that bypass this shared lock cannot be serialized by this API.

Verification commands:

```sh
sh tests/test_whitelist_bulk.sh
sh tests/test_whitelist_editor.sh
sh scripts/ci_local.sh
# Optional browser suite: install Playwright outside production dependencies.
python3 tests/browser/whitelist_server.py
# In another terminal (Chrome installed):
PLAYWRIGHT_MODULE=/path/to/playwright/index.mjs node tests/browser/whitelist.mjs
```

The browser suite uses a local temporary whitelist and the real CGI. It tests
range/filter selection, selected replacement, confirmed/cancelled clear,
selective deletion, undo, concurrent edits, invalid-input draft retention,
1,500 entries, mobile overflow, dark theme, and keyboard navigation.
The pure model suite also covers a 10,000-entry document.

Router candidate: backup files are in `/opt/z2k-whitelist-backup-20260922/`.
Deployment replaces actions.sh, api.sh, exclude.js, style.css and adds the shared
whitelist.js module. Existing whitelist contents are left intact. The separate
BusyBox runtime test uses only a temporary fixture file. This is a local router
preview, not a published release.

Completed verification: full local CI green (one existing macOS BSD-sed skip),
69 authentication tests and 10 module-delivery tests passed. Browser fixture
checks and BusyBox isolated-file checks passed. After deployment, the actual
router page loaded 182 domains, selected all, opened/cancelled both editors,
and passed mobile-width and JavaScript-error checks. Before/after live whitelist
files compare byte-for-byte. No live user entries were modified by the tests.
