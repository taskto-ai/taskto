# Look and fonts

The web app wears the identity of the taskto.ai website, "mint on paper": warm cream paper,
near-black ink, one electric mint, headings in Bricolage Grotesque and text in Inter, objects
drawn with a firm ink line and the buttons on a short hard lift. This page says how that is
built so a change keeps it whole. It is a visual identity only: layouts, spacing and components
are the app's own. (The website has been through other identities; this is the one it serves
now, its "Electric Mint" page. The app follows the website's `tailwind.config.js` and
`src/styles.css`, not an earlier look.)

## Tokens

Every colour in `web/src/styles.css` is a custom property declared once at the top, as
`light-dark(light, dark)`. Which side applies is the page's `color-scheme`: the system's
(`light dark`) unless the person picked a theme in the top bar (`data-theme` on `<html>`,
`web/src/theme.ts`, which keeps the choice in this browser only). **Never write a colour
literal below the token block**; use a token, or add one there with both sides.

| Token | Light | Dark | For |
|---|---|---|---|
| `--tt-canvas` | cream `#faf8f4` | night `#111110` | the page, and the top bar |
| `--tt-stone` | stone `#f1efe9` | `#181816` | the sidebar, a board's columns, quiet bands |
| `--tt-surface` | white | `#1b1b19` | cards, inputs, menus |
| `--tt-fg` / `--tt-fg-soft` / `--tt-muted` | ink `#111110` / `#2e2d2a` / muted `#5c5a55` | cream `#faf8f4` / `#d6d3cc` / `#aba89f` | the three levels of text |
| `--tt-line` | ink at 15% | cream at 14% | hairlines between things |
| `--tt-rule` | ink | cream at 24% | the rule under the top bar |
| `--tt-line-strong` | `#7e7b74` | `#8a877f` | the edge of a control (inputs, buttons) |
| `--tt-hover`, `--tt-selected`, `--tt-user` | ink at 5% / 8%, stone | cream at 7% / 12%, `#262624` | hover, a pressed item, the person's own message |
| `--tt-accent`, `--tt-accent-hover`, `--tt-on-accent` | mint `#4cefa8`, bright `#6bf5b8`, ink | the same | the primary button: a mint fill with ink on it |
| `--tt-accent-edge` | ink | mint edge `#1fc98a` | the primary button's outline and its lift |
| `--tt-accent-soft` | mint soft `#dcfceb` | mint at 14% | the current item (a nav link, a tab, a notice) |
| `--tt-focus` | ink | mint | the focus ring (2.5px, offset 2px) and any thin line meaning "current" |
| `--tt-link` | deep green `#006b45` | mint | links, success, any green text |
| `--tt-node`, `--tt-node-edge` | mint, `#1fc98a` | the same | the AI's dot and its ring |
| `--tt-logo-dot` | `#00a86b` | the same | the logo's green, in the logo only |
| `--tt-danger`, `--tt-on-danger`, `--tt-warning` | `#b42318`, white, `#9a4700` | `#ff8a82`, night, `#ffb04d` | status |

The website is light only. The dark palette is the app's own, grown from the website's night
(`#111110`, which is also its terminal's ground), with the same mint on it.

### How the mint is used

**Mint is a fill, never the colour of text.** It is a bright accent (about 1.4:1 on cream and
white), so it carries ink on top of it (12.8:1) or it carries nothing. It is the primary button,
the current item's soft tint, a chosen choice, the dot that stands for the AI, a selection's
background. It is never a thin line or a word on a light ground: those use ink or the deep green.

- **The deep green** (`#006b45`) is for what must be read or seen on light grounds: links, the
  "Live" badge and other success text. It is the logo's own green (`#00a86b`, hue 158°) with
  its lightness taken from 33% to 21% until every pair below passed. It is not a second accent:
  it is the mint's text voice. In the dark the mint itself is readable (12.8:1 on night), so
  `--tt-link` is the mint there.
- **Focus** follows the website: an ink ring on light, a mint one on night.
- **The logo's green** (`#00a86b`) lives only inside the logo, as on the website, which keeps
  it out of the page's colours. A logotype is exempt from contrast rules (its `.ai` is 2.9:1 on
  cream, as it is on the website's own nav).
- **The AI's dot** is the website's node: mint with the mint edge as a ring. It marks what the
  agent did (the dot before a tool line in a conversation) beside words that say it, so it never
  carries a meaning alone. A dot that must be seen on its own (the inbox's unread mark, the app
  builder's change mark) is ringed in ink.

### Contrast

Checked numerically (WCAG 2.x relative luminance) for every pair the app puts together, on every
ground it can sit on: page, stone, white, the hover and selected tints, the soft mint, and in the
dark night, stone, surface, the user's message and the soft mint.

Light (cream / stone / white / selected tint on stone, the darkest):

- `--tt-fg` 17.8 / 16.4 / 18.9 / 13.9; `--tt-fg-soft` 13.0 / 12.0 / 13.8 / 10.2;
- `--tt-muted` 6.5 / 6.0 / 6.9 / 5.1;
- `--tt-link` 6.2 / 5.7 / 6.6 / 4.9; danger 6.2 / 5.7 / 6.6 / 4.8; warning 6.0 / 5.6 / 6.4 / 4.7;
- ink on mint 12.8, on bright mint 13.8; white on danger 6.6;
- the edge of a control, `--tt-line-strong`: 4.0 / 3.7 / 4.2 / 3.1 (3:1 is the floor for the
  edge of a control); the focus ring, ink: at least 13.9.

Dark (night / stone / surface / user message / soft mint on stone, the lowest):

- `--tt-fg` 17.8 / 16.8 / 16.3 / 14.3 / 12.2; `--tt-muted` 7.9 / 7.5 / 7.3 / 6.4 / 5.5;
- mint as text (`--tt-link`, `--tt-focus`) 12.8 / 12.0 / 11.7 / 10.3 / 8.8;
- danger 8.3 / 7.8 / 7.6 / 6.7 / 5.7; warning 10.4 / 9.8 / 9.5 / 8.4 / 7.1; night on danger 8.3;
- the edge of a control: 5.3 / 5.0 / 4.8 / 4.2 / 3.6.

Text of every kind meets 4.5:1, a placeholder (`::placeholder` is `--tt-muted`, 6.9 light and 7.3
dark on a field) and an app's validation message (`--a2ui-textfield-color-error` is `--tt-danger`:
6.2 / 6.6 light, 8.3 / 7.6 dark on the page / the field; the renderer's own `red` was 4.0 and 4.3).
A field's focus is the ring, not a change of its border, so an invalid field (whose border is the
error's) shows it too: `.tt-app .a2ui-textfield:focus-visible`, 17.8 light and 12.8 dark.

When you change a colour, check the pair again; a literal that "looks close" is how a dark-mode
badge ended up white on salmon.

## One stylesheet

`web/src/styles.css` is global, so a class name is shared by the whole app: declaring a name a
second time silently changes the first screen that used it (a code sample's grid once made every
highlighted word of a search result a row of its own). `web/scripts/css-check.mjs`, run by
`make check` and CI, fails when a `.tt-` selector is declared twice at the top level of the file
(a state or a screen size goes in an at-rule block; two different things get two names), and when
a `tt-` class a component's `className` uses has no rule. A class that is deliberately an
unstyled hook goes in the script's `UNSTYLED` list, with the reason.

## The buttons

Pills, as on the website. A plain button is white with a 1px control edge. **The primary button**
(the one thing a screen asks you to press) is mint with ink on it, outlined in 1.5px ink and lifted
on a hard 2px ink shadow that rises to 3px on hover and is pressed flat on click (in the dark, the
outline and the lift are the mint edge). The motion stops under `prefers-reduced-motion`. Icon
buttons, fields and cards keep the app's 8px radius.

## Fonts

The website's three families, each in the role the app needs:

| Family | Role in the app | Website's role |
|---|---|---|
| Inter | all text, labels, the controls: a dense interface wants its plain, even forms | running text |
| Bricolage Grotesque | headings (`h1` to `h3`, set heavy and tight), the apps' titles | headlines, and the site's default sans |
| JetBrains Mono | paths, commands, ids and code, never labels or prose | eyebrows, paths, commands |

The website sets its body in Bricolage; the app doesn't, because forms, tables and long lists in a
grotesque with character cost reading speed. Bricolage speaks where the website uses it to speak:
in headings and in the brand. `--tt-font`, `--tt-display` and `--tt-mono` in `web/src/styles.css`
name them, each with the system's fonts behind it.

They come from the npm packages `@fontsource-variable/inter`, `@fontsource-variable/bricolage-grotesque`
and `@fontsource-variable/jetbrains-mono` (SIL Open Font License 1.1), imported in
`web/src/main.tsx` and bundled by Vite as hashed `.woff2` files in `/assets/`. Bricolage is the
weight-axis build (`wght.css`); the website adds the optical-size axis for its display sizes, which
the app has none of, and leaving it out saves about 50 kB. **Nothing is fetched from another site**,
a CDN or a font service included: a self-hosted server must not tell a third party who visits it.
The system's fonts show until the files load (`font-display: swap`) and for scripts these don't
cover (Arabic, Hebrew, CJK and others).

The files are split by script with `unicode-range`, so a person downloads only the scripts they see
(about 130 kB for Latin: the `latin` file of each of the three, and each only when its family is used).
The service worker precaches all of them, about 370 kB in all, so the app is set in its fonts
offline too.

The fonts' licence text and copyright lines go with the build, in `/assets/THIRD_PARTY_NOTICES.txt`
(`web/pwa/notices.ts`, which a test covers), as the OFL asks.

## The logo

The top bar and the login screens show the website's logo (`web/src/BrandLogo.tsx`, drawn from its
`/logo.svg`): an arrow handing off to a dot, then "taskto.ai". The website's rule is that the logo
stands in for the name, so the app shows it too, with its own name as the image's label. The arrow
and the name take the page's text colour (ink on cream, cream on night, as the website's
`logo-dark.svg`); the dot and ".ai" are the logo's green. On a phone (48rem and narrower) the top
bar shows the mark alone, the drawing cropped to its first 64 units.

**The top bar never overflows**, from 320px up, whatever the workspace is called. The menu, the
mark, the bell, the theme and the account keep their size; the workspace's select is the part
that gives way (`min-width: 0`, an ellipsis, the whole name as its tooltip). Under 30rem the theme
picker is its icon alone, with the select laid over it unseen, so a name such as "Operations and
Field Services" still leaves room for itself. Checked in a real browser at 320, 360 and 390px
(and 768 and 1024) with "Acme", "Operations" and that name, in both themes.

## The apps renderer

The A2UI renderer styles itself from `--a2ui-*` custom properties with defaults of its own (a blue
primary button, grey borders). `:root` sets them from the tokens above, so an app's buttons, fields,
choices and captions follow the theme without any change to the renderer or to what an app can say:
pill buttons and choices, a mint primary button outlined in ink (the renderer draws it with no
border, so `styles.css` adds one), the display face for titles, and the deep green for a borderless
button's text. The renderer colours several things with its primary, which is the mint: a borderless
button's text, a focused field's border, the picked tab and a slider's thumb. Each is pointed at a
token that can be seen on the page instead (the deep green, or the focus colour), because mint there
would be 1.4:1 on cream. When the renderer gains another use of its primary, check it the same way.

A button's label is a Text component, and the renderer colours Text from `--_a2ui-text-color` (a
caption, from its own muted colour, over that). So `styles.css` points the label at the button's
ink: the borderless button sets that variable to the deep green as well as its own `color`, and a
label in a button inherits it in every variant, so a caption on a mint button is ink on mint
(12.8:1) and not muted grey (1.6:1).

## Installed app

`web/public/manifest.webmanifest` (`theme_color` and `background_color` cream `#faf8f4`) and the
`theme-color` metas in `web/index.html` (cream; night `#111110` in the dark) match the top bar and
the page. The metas follow the OS through their media queries, which is right for the System theme;
when the person picks Light or Dark in the top bar, `web/src/theme.ts` sets both metas to that
theme's colour (and System puts back each one's own), so the browser's address bar and status bar
follow the app's choice, not the OS's. The two colours are `THEME_COLOR` there, which a test keeps
equal to `--tt-canvas`, the metas and the manifest. The icons are the website's app tile (its `favicon.svg` and apple touch icon): a logo-green
(`#00a86b`) square with the logo's arrow and dot in ink, drawn by `web/scripts/make-icons.mjs` from
the same geometry as `icon.svg`. The top bar is light, so the iOS status bar is the default one (dark
text), not `black-translucent`, whose white text it would hide.
