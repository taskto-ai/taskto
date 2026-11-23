# Look and fonts

The web app wears the identity of the taskto.ai website, "the handoff": ink on white and
near-white, structure drawn in hairlines, heavy tight headings in Geist, and one accent, the
emerald of the logo's dot, which stands for the AI. This page says how that is built so a
change keeps it whole. It is a visual identity only: layouts, spacing and components are the
app's own.

## Tokens

Every colour in `web/src/styles.css` is a custom property declared once at the top, as
`light-dark(light, dark)`. Which side applies is the page's `color-scheme`: the system's
(`light dark`) unless the person picked a theme in the top bar (`data-theme` on `<html>`,
`web/src/theme.ts`, which keeps the choice in this browser only). **Never write a colour
literal below the token block**; use a token, or add one there with both sides.

| Token | Light | Dark | For |
|---|---|---|---|
| `--tt-canvas` | `#f6f7f9` | `#0b0d12` | the page |
| `--tt-surface` | `#ffffff` | `#12161d` | cards, inputs, the top bar |
| `--tt-fg` / `--tt-fg-soft` / `--tt-muted` | `#0b0d12` / `#2a2f3a` / `#565d6b` | `#e9edf3` / `#c9d0dc` / `#9aa3b2` | the three levels of text |
| `--tt-line` | `#e4e7ec` | `#262c38` | hairlines between things |
| `--tt-line-strong` | `#858e9e` | `#6b7384` | the edge of a control (inputs, buttons) |
| `--tt-hover`, `--tt-selected`, `--tt-user` | ink tints | light tints | hover, the current item, the person's own message |
| `--tt-accent`, `--tt-accent-hover`, `--tt-on-accent` | ink, `#2a2f3a`, white | `#e9edf3`, white, `#0b0d12` | the primary button, which is ink |
| `--tt-focus` | `#0b0d12` | `#e9edf3` | the focus ring (2px, offset 2px) |
| `--tt-node`, `--tt-node-text` | `#00a86b`, `#00744a` | `#00a86b`, `#2bc98a` | the AI node (a dot) and emerald as text |
| `--tt-danger`, `--tt-on-danger`, `--tt-success`, `--tt-warning` | `#b42318`, white, `#00744a`, `#9e4a00` | `#ff7a72`, `#0b0d12`, `#2bc98a`, `#ffa733` | status |

The website is light only. The dark palette is the app's own, grown from the website's ink and
its terminal (`#0b0d12` ground, `#e9edf3` text).

**Emerald means the AI.** It marks what the agent says and does (the dot before a tool line in
a conversation, the dot in the logo) and nothing else: not a background, not a decoration. As
text it is the deeper `--tt-node-text`. Success is that same deep green, so the system keeps
one green and no second hue.

### Contrast

Checked numerically (WCAG 2.x relative luminance), every pair the app uses:

- text on its ground, at least 4.5:1: `--tt-fg` 19.4 / 15.4 (on surface, light / dark),
  `--tt-muted` 6.6 / 7.1 on surface and 5.4 / 5.5 on the selected item, status colours between
  5.4 and 9.1, `--tt-on-accent` on `--tt-accent` 19.4 / 16.5;
- the edge of a control (`--tt-line-strong`) and the focus ring, at least 3:1 (3.1 / 3.8 for
  the edge on the page, 19.4 / 15.4 for the ring);
- the emerald dot is 2.9 to 3.1:1 on the light grounds. It is never the only carrier of a
  meaning: it always sits beside words.

When you change a colour, check the pair again; a literal that "looks close" is how a
dark-mode badge ended up white on salmon.

## Fonts

Geist (text) and Geist Mono (paths, commands and code, never labels or prose) are the
website's fonts. They come from the npm packages `@fontsource-variable/geist` and
`@fontsource-variable/geist-mono` (SIL Open Font License 1.1), imported in `web/src/main.tsx`
and bundled by Vite as hashed `.woff2` files in `/assets/`. **Nothing is fetched from another
site**, a CDN or a font service included: a self-hosted server must not tell a third party who
visits it. `--tt-font` and `--tt-mono` keep the system's fonts as the fallback, which show
until the files load (`font-display: swap`) and for scripts Geist doesn't cover (Arabic,
Hebrew, CJK and others).

The files are split by script with `unicode-range`, so a person downloads only the scripts they
see (about 30 kB for Latin). The service worker precaches all of them, about 155 kB in all, so
the app is set in its fonts offline too.

The fonts' licence text and copyright lines go with the build, in
`/assets/THIRD_PARTY_NOTICES.txt` (`web/pwa/notices.ts`, which a test covers), as the OFL asks.

## The apps renderer

The A2UI renderer styles itself from `--a2ui-*` custom properties with defaults of its own (a
blue primary button, grey borders). `:root` sets them from the tokens above, so an app's
buttons, fields, choices and captions follow the theme without any change to the renderer or to
what an app can say.

## Installed app

`web/public/manifest.webmanifest` (`theme_color` white, `background_color` the page's
`#f6f7f9`) and the `theme-color` metas in `web/index.html` (white; `#12161d` in the dark)
match the top bar. The icons are the website's app tile, an emerald square with the ink arrow
and dot, drawn by `web/scripts/make-icons.mjs` from the same geometry as `icon.svg`. The
top bar is light, so the iOS status bar is the default one (dark text), not
`black-translucent`, whose white text it would hide.
