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

A GitHub Action (US-011) publishes `docs/site/public/` to the `gh-pages`
branch on every merge to `main`. Live URL: <https://danchupin.github.io/strata/>.
