---
title: 'Developers'
weight: 70
bookFlatSection: true
description: 'Editing the docs — frontmatter, cross-links, contributing.'
---

# Developers — editing the docs

The docs site is a Hugo project under `docs/site/`. Theme is `hugo-book`
as a git submodule at `docs/site/themes/hugo-book/`. Fresh checkouts need
`git submodule update --init --recursive`.

## Local preview

```bash
make docs-serve   # hugo server -D on :1313
make docs-build   # hugo --minify → docs/site/public/
```

Hugo extended (>= 0.128) is required by the theme; Homebrew's `hugo`
formula already ships extended.

## Adding a page

1. Pick the right section under `docs/site/content/<section>/`.
2. Add Hugo frontmatter at the top:
   ```yaml
   ---
   title: 'Page Title'
   weight: 10
   description: 'One-line description for the sidebar tooltip.'
   ---
   ```
3. Cross-link other pages with the `{{</* ref */>}}` shortcode:
   `[label]({{</* ref "/section/page" */>}})`.
4. Build locally: `make docs-build` should be warning-free.

## Publishing

`.github/workflows/docs.yml` publishes `docs/site/public/` to the
`gh-pages` branch on every merge to `main` (path-filtered on `docs/**`
and the workflow itself) and on manual `workflow_dispatch`. The workflow
does NOT run on PRs to avoid leaking pre-merge drafts. Live URL:
<https://danchupin.github.io/strata/>.

### One-time repository settings

After the first successful workflow run creates the `gh-pages` branch,
flip the GitHub Pages source to publish from it:

1. Repo → **Settings** → **Pages**.
2. **Source** → **Deploy from a branch**.
3. **Branch** → `gh-pages` / `/ (root)` → **Save**.
4. Wait ~30 s, then confirm <https://danchupin.github.io/strata/>
   responds 200.

Subsequent merges republish automatically; no further manual action.
