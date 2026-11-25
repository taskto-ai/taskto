# Look and fonts

The web app wears the identity of the taskto.ai website, "mint on paper": warm cream paper,
near-black ink, one electric mint, headings in Bricolage Grotesque and text in Inter, objects
drawn with a firm ink line and the buttons on a short hard lift. This page says how that is
built so a change keeps it whole. It is a visual identity only: layouts, spacing and components
are the app's own. (The website has been through other identities; this is the one it serves
now, its "Electric Mint" page. The app follows the website's `tailwind.config.js` and
`src/styles.css`, not an earlier look.)

## Tokens

Every value the app's stylesheets use comes from a token, in two layers, in `web/src/ui/`:

1. **Scales** (`tokens.css`): space, type sizes and the type roles built from them, radii, control
   heights, elevation, motion, stacking and page widths. They are the same in every theme.
2. **Colour roles** (`themes/`): what a colour is for (a ground, a level of text, a status),
   never what it looks like, each written once as `light-dark(light, dark)`. Their values are a
   brand's: `themes/mint.css` is the default and the only one the app ships.

`web/src/ui/index.css`, which `web/src/main.tsx` imports before anything else that styles the page,
declares the cascade layers and imports the files:

| File | Layer | What |
|---|---|---|
| `index.css` | | `@layer tokens, themes, base, components, screens;` and the imports |
| `tokens.css` | `tokens` | the scales; and, in no layer, the switch of `color-scheme` by `data-theme` |
| `themes/mint.css` | `themes` | the default brand's colour roles, on `:root` |
| `themes/harbor.css` | `themes` | a sample second brand, not imported by the app (see below) |
| `legacy.css` | `tokens` | the older `--tt-*` names, as aliases of the roles and scales |
| `a2ui.css` | none | the apps renderer's `--a2ui-*` variables, set from the roles |
| `roles.ts`, `contrast.ts` | | the list of colour roles and the pairs they allow, as data for the tests and the gallery |

A later layer wins over an earlier one whatever the specificity, and a rule in no layer wins over
every layer. `base` (elements), `components` (one file per component, plain CSS) and `screens` (what
only one screen needs) are declared for the files that will fill them; `web/src/styles.css` is in
no layer until its rules move there. Two things stay out of the layers on purpose: the apps
renderer injects its defaults (`:where(:root) { color-scheme: light dark; --a2ui-… }`) in no
layer, so the `color-scheme` switch and `a2ui.css` would lose to them from inside one.

**The naming rule.** `--<kind>-<name>`, with the kind one of `color`, `space`, `text` (a size),
`type` (a role), `font`, `tracking`, `radius`, `control`, `shadow`, `duration`, `ease`, `z` and
`width`. Nothing but a colour role starts with `--color-`, and a colour is only ever a role: a scale
that needs one (a shadow, the lift) takes it from a role. **No colour literal is written outside
`web/src/ui/themes/`**: `web/scripts/css-check.mjs` fails on a hex colour, a colour function or a
named colour in any other stylesheet's declarations.

**The older names.** `web/src/styles.css` still uses `--tt-canvas`, `--tt-fg` and the rest.
`legacy.css` keeps each as an alias of its role or scale (`--tt-canvas: var(--color-bg)`), so the
screens look as they did; a test checks that every one still resolves, to the colour it had. New
CSS uses the roles and scales; the aliases go when the last screen has moved.

### The scales

| Scale | Steps |
|---|---|
| Space | `--space-1` 4px, `-2` 8, `-3` 12, `-4` 16, `-5` 20, `-6` 24, `-8` 32, `-10` 40, `-12` 48, `-16` 64 |
| Type sizes | `--text-xs` 12px, `-sm` 13, `-md` 14 (the interface), `-lg` 16 (reading), `-xl` 20, `-2xl` 24 |
| Radius | `--radius-sm` 6px (a tag), `-md` 10 (a control, a card), `-lg` 14 (a dialog), `-pill`. Today's controls and cards are still 8px, through the old `--tt-radius`: each moves to `--radius-md` (10px) as it is rebuilt, and 8px lives on only as that alias until the last one has moved |
| Control height | `--control-sm` 28px, `--control-md` 36, `--control-touch` 44 (the touch target) |
| Elevation | `--shadow-lift` and `--shadow-lift-hover` (the primary button's hard lift), `--shadow-pop` (a menu, a popover), `--shadow-dialog` |
| Motion | `--duration-fast` 120ms, `-base` 160, `-slow` 240; `--ease-out`, `--ease-in-out`; all of it stops under `prefers-reduced-motion` |
| Stacking | the `--z-*` levels, below |
| Width | `--width-reading` 760px (reading and forms), `--width-list` 1,040px (lists), `--width-sidebar` 232px; a board is the window's width |

### Stacking

Bottom to top. A modal `<dialog>` and a popover are in the browser's top layer, above every level
here, so a dialog, a menu or a tooltip built on them sets no `z-index` at all: these levels are for
what is positioned on the page.

| Level | Value | What belongs there |
|---|---|---|
| `--z-sticky` | 10 | a header or a bar that sticks while its page scrolls |
| `--z-sidebar` | 20 | the sidebar on a wide screen |
| `--z-scrim` | 25 | the shade behind the drawer on a phone: over the page, under the drawer |
| `--z-drawer` | 30 | the sidebar as a drawer on a phone |
| `--z-overlay` | 40 | what floats over the page without the top layer: a menu or a popup placed by hand |
| `--z-toast` | 50 | a passing message, the update banner, a guard that must stay seen |
| `--z-skip` | 100 | the skip link, above everything when it has the focus |

The stylesheet's own numbers (the skip link's 100, the banner's and the guard's 50, the phone's
scrim at 10 under the sidebar's 20) stay as they are until their rules move onto these levels. A
test keeps the levels in this order and `--z-skip` at or above every `z-index` in the stylesheets.

### The type roles

A type role is a `font` shorthand (weight, size / line height, family), used as
`font: var(--type-ui)`. The interface is 14px and what people read (a document, the chat) 16px.
The display face, Bricolage Grotesque, speaks at titles of 20px and up only, set tight
(`letter-spacing: var(--tracking-display)`); everything smaller is Inter, whose even forms read
better at small sizes. JetBrains Mono is for code, paths, commands and ids.

| Role | Face | Size / line | Weight | For |
|---|---|---|---|---|
| `--type-display` | Bricolage Grotesque | 24 / 30 | 750 | a page's title |
| `--type-title` | Bricolage Grotesque | 20 / 26 | 700 | a dialog's or a panel's title |
| `--type-section` | Inter | 16 / 24 | 600 | a section's title |
| `--type-reading` | Inter | 16 / 26 | 400 | documents, messages |
| `--type-ui`, `--type-ui-strong` | Inter | 14 / 20 | 400, 600 | the interface: labels, controls, lists |
| `--type-small` | Inter | 13 / 18 | 400 | secondary text, metadata |
| `--type-caption` | Inter | 12 / 16 | 400 | captions, counts |
| `--type-code` | JetBrains Mono | 13 / 20 | 400 | code, paths |

Today's screens still set their text at 15px and their headings in the display face as before;
the roles are defined for the screens to move onto.

### The colour roles

The roles of the default brand. The website is light only; the dark side is the app's own, grown
from the website's night (`#111110`, which is also its terminal's ground), with the same mint on it.
The old name each replaces is in brackets.

| Role | Light | Dark | For |
|---|---|---|---|
| `bg` (`--tt-canvas`) | cream `#faf8f4` | night `#111110` | the page |
| `bg-sunken` (`--tt-stone`) | stone `#f1efe9` | `#181816` | the sidebar, a board's columns, quiet bands |
| `surface` (`--tt-surface`) | white | `#1b1b19` | cards, inputs, menus |
| `surface-own` (`--tt-user`) | stone | `#262624` | the person's own message |
| `paper`, `media` | white, black | the same | a QR code or a signature (dark on light to be read); the ground behind a camera's picture |
| `hover`, `selected` | ink at 5% / 8% | cream at 7% / 12% | tints laid over a ground |
| `scrim`, `shadow` | ink at 42% / 35% | black at 60% / 80% | under a dialog; the colour of the shadows |
| `text` / `text-soft` / `text-muted` | ink `#111110` / `#2e2d2a` / `#5c5a55` | cream / `#d6d3cc` / `#aba89f` | the three levels of text |
| `border` (`--tt-line`) | ink at 15% | cream at 14% | hairlines between things |
| `border-strong` (`--tt-line-strong`) | `#7e7b74` | `#8a877f` | the edge of a control (3:1) |
| `rule` | ink | cream at 24% | a rule that sets the page apart |
| `accent`, `accent-hover`, `on-accent` | mint `#4cefa8`, bright `#6bf5b8`, ink | the same | the primary button: a mint fill with ink on it |
| `accent-edge` | ink | mint edge `#1fc98a` | the primary button's outline and its lift |
| `accent-soft` | mint soft `#dcfceb` | mint at 14% | the current item (a nav link, a tab) |
| `focus` | ink | mint | the focus ring (2.5px, offset 2px) and any thin line meaning "current" |
| `link` | deep green `#006b45` | mint | links |
| `node`, `node-edge` | mint, `#1fc98a` | the same | the agent's dot and its ring |
| `logo` (`--tt-logo-dot`) | `#00a86b` | the same | the logo's green, in the logo only |
| `success`, `success-soft` | deep green, mint soft | mint, mint at 14% | live, done, saved |
| `warning`, `warning-soft` | `#9a4700`, `#fbeccb` | `#ffb04d`, at 15% | due soon, a draft |
| `danger`, `danger-soft`, `on-danger` | `#b42318`, `#fde5e1`, white | `#ff8a82`, at 15%, night | overdue, errors, what deletes |
| `info`, `info-soft` | `#1c4f9c`, `#e1ebfa` | `#9cc2ff`, blue at 16% | information |
| `neutral`, `neutral-soft` | `#5c5a55`, `#ebe8e1` | `#aba89f`, cream at 9% | what is off or inactive |
| `label-<hue>`, `label-<hue>-soft` | see the contrast table | | a board label, by a hue picked from its name: `green`, `blue`, `violet`, `pink`, `amber`, `teal` |
| `column-todo`, `-doing`, `-review`, `-done` | `#2f6fde`, `#b86e00`, `#7c5ce0`, `#0e8a58` | `#7aa7ff`, `#ffb04d`, `#b9a5ff`, mint | the dot beside a board column's name |

Each is `--color-<role>`. Colour is spent where it says something: a status, a label, a column's
kind, the primary action and the agent. It is not decoration.

### Light, dark and the brand

**Light or dark** is the page's `color-scheme`: the system's (`light dark`, with no script) unless
the person picked one in the top bar, which sets `data-theme="light"` or `"dark"` on `<html>`
(`web/src/theme.ts`, which keeps the choice in this browser only). Every role is written once as
`light-dark()`, so the same block serves both.

**The brand** is `data-brand` on `<html>`, beside `data-theme` and independent of it: absent, the
default (mint, on `:root`); `data-brand="harbor"`, the brand in `themes/harbor.css`. A brand file
is one rule, `:root[data-brand="<name>"]`, in `@layer themes`, that sets every colour role, both
sides, and nothing else: no scale, no other property. Its attribute selector outranks the default's
`:root` in the same layer, whatever order the files load in. To add a brand:

1. Copy `themes/harbor.css` to `themes/<name>.css`, change the selector, and set every role.
2. Add the name to `BRANDS` in `web/src/ui/roles.ts`.
3. Run `pnpm --filter @taskto/web run test`: it fails on a missing role, on any property that is
   not a role, and on every pair of `contrast.ts` under its minimum, in light and in dark.
4. Import the file where the brand is used (the app imports only the default today) and set
   `data-brand`.

`themes/harbor.css` (cool paper, navy ink, a signal yellow) is the proof that a swap changes the
roles and nothing else: the app doesn't import it and has no brand picker; the tests load it, and
the gallery of components will. `THEME_COLOR` in `theme.ts`, the `theme-color` metas and the
manifest are the default brand's ground.

### How the mint is used

**Mint is a fill, never the colour of text.** It is a bright accent (about 1.4:1 on cream and
white), so it carries ink on top of it (12.8:1) or it carries nothing. It is the primary button,
the current item's soft tint, a chosen choice, the dot that stands for the AI, a selection's
background. It is never a thin line or a word on a light ground: those use ink or the deep green.

- **The deep green** (`#006b45`) is for what must be read or seen on light grounds: links, the
  "Live" badge and other success text. It is the logo's own green (`#00a86b`, hue 158°) with
  its lightness taken from 33% to 21% until every pair below passed. It is not a second accent:
  it is the mint's text voice. In the dark the mint itself is readable (12.8:1 on night), so
  `link` and `success` are the mint there.
- **Focus** follows the website: an ink ring on light, a mint one on night.
- **The logo's green** (`#00a86b`) lives only inside the logo, as on the website, which keeps
  it out of the page's colours. A logotype is exempt from contrast rules (its `.ai` is 2.9:1 on
  cream, as it is on the website's own nav).
- **The AI's dot** is the website's node: mint with the mint edge as a ring. It marks what the
  agent did (the dot before a tool line in a conversation) beside words that say it, so it never
  carries a meaning alone. A dot that must be seen on its own (the inbox's unread mark, the app
  builder's change mark) is ringed in ink.

### Contrast

The pairs the roles allow are data, in `web/src/ui/contrast.ts`: which roles may be drawn on which
grounds (a ground may be a tint laid over another, such as the selected tint on a column), and the
minimum, 4.5:1 for text and 3:1 for a control's edge, the focus ring and a mark seen on its own.
`web/src/ui/tokens.test.ts` computes every pair (WCAG 2.x relative luminance) from the token files'
values, for every brand, in light and in dark, and fails under the minimum. A screen that puts a
role on a ground not listed adds the pair there first.

The default brand's numbers. Grounds: cream or night / stone / surface / own message / the selected
tint on stone / the soft mint on stone.

Light:

- `text` 17.8 / 16.4 / 18.9 / 16.4 / 13.9 / 17.2; `text-muted` and `neutral` 6.5 / 6.0 / 6.9 / 6.0 / 5.1 / 6.3;
- `link` and `success` 6.2 / 5.7 / 6.6 / 5.7 / 4.9 / 6.0; `danger` the same; `warning` 6.0 / 5.6 / 6.4 / 5.6 / 4.7 / 5.9;
  `info` 7.5 / 6.9 / 7.9 / 6.9 / 5.9 / 7.2;
- ink on mint 12.8, on bright mint 13.8; white on danger 6.6;
- the edge of a control, `border-strong`: 4.0 / 3.7 / 4.2 / 3.7 / 3.1 / 3.9; the focus ring, ink: at least 13.9;
- a column's dot on stone / cream: todo 4.1 / 4.4, doing 3.5 / 3.8, review 4.1 / 4.4, done 3.8 / 4.1.

Dark:

- `text` 17.8 / 16.8 / 16.3 / 14.3 / 12.0 / 12.2; `text-muted` and `neutral` 7.9 / 7.5 / 7.3 / 6.4 / 5.3 / 5.4;
- mint as text (`link`, `success`, `focus`) 12.8 / 12.0 / 11.7 / 10.3 / 8.6 / 8.8;
- `danger` 8.3 / 7.8 / 7.6 / 6.6 / 5.6 / 5.7; `warning` and `info` 10.4 / 9.8 / 9.5 / 8.4 / 7.0 / 7.1; night on danger 8.3;
- the edge of a control: 5.3 / 5.0 / 4.8 / 4.2 / 3.5 / 3.6;
- a column's dot on stone, at least: todo 7.4, doing 9.8, review 8.4, done 12.0.

A status and a label on their own soft ground (on a card / a column / the page; the light soft
grounds are opaque, so one number):

| Pair | Light | Dark |
|---|---|---|
| `success` on `success-soft` | 6.0 | 8.4 / 8.8 / 9.5 |
| `warning` on `warning-soft` | 5.5 | 6.9 / 7.2 / 7.8 |
| `danger` on `danger-soft` | 5.5 | 5.8 / 6.0 / 6.5 |
| `info` on `info-soft` | 6.6 | 7.3 / 7.6 / 8.2 |
| `neutral` on `neutral-soft` | 5.6 | 5.6 / 5.9 / 6.4 |
| `label-green` `#00633f` on `#d5f7e4` (dark `#7df7c1` on mint at 16%) | 6.4 | 9.0 / 9.3 / 10.1 |
| `label-blue` `#1c4f9c` on `#dfe9fa` (dark `#a9c9ff` on blue at 18%) | 6.5 | 7.6 / 7.8 / 8.5 |
| `label-violet` `#5334a3` on `#e9e3fa` (dark `#cbbcff` on violet at 20%) | 7.0 | 7.3 / 7.6 / 8.2 |
| `label-pink` `#972159` on `#fae1ec` (dark `#ffb0d2` on pink at 17%) | 6.4 | 7.6 / 7.9 / 8.6 |
| `label-amber` `#7a4a00` on `#f9eac6` (dark `#ffd27a` on amber at 16%) | 6.3 | 8.4 / 8.8 / 9.5 |
| `label-teal` `#0b5f5a` on `#d5f1ee` (dark `#8be6de` on teal at 15%) | 6.3 | 8.6 / 8.9 / 9.6 |

Text of every kind meets 4.5:1, a placeholder (`::placeholder` is `text-muted`, 6.9 light and 7.3
dark on a field) and an app's validation message (`--a2ui-textfield-color-error` is `danger`:
6.2 / 6.6 light, 8.3 / 7.6 dark on the page / the field; the renderer's own `red` was 4.0 and 4.3).
A field's focus is the ring, not a change of its border, so an invalid field (whose border is the
error's) shows it too: `.tt-app .a2ui-textfield:focus-visible`, 17.8 light and 12.8 dark.

When you change a colour, the test checks the pairs again; a literal that "looks close" is how a
dark-mode badge ended up white on salmon.

## The stylesheets

Every stylesheet under `web/src/` is global, so a class name is shared by the whole app: declaring a
name a second time silently changes the first screen that used it (a code sample's grid once made
every highlighted word of a search result a row of its own). `web/scripts/css-check.mjs`, run by
`make check` and CI, reads every `.css` file under `web/src/` and fails when a `.tt-` or `.ui-`
selector is declared twice at the top level, in one file or in two (inside `@layer` blocks counts as
the top level; a state or a screen size goes in another at-rule block; two different things get two
names), when a `tt-` or `ui-` class a component's `className` uses has no rule, and when a colour literal is
written outside `web/src/ui/themes/`. A class that is deliberately an unstyled hook goes in the
script's `UNSTYLED` list, with the reason. It reads class selectors (not an attribute selector's
value), a component's `className`, and the strings of a lookup table in capitals
(`const HEALTH_CLASS = { ok: 'tt-ok' }`); a class built from pieces is not seen, and the header of
the script says what else it can't.

## Components

The controls every screen uses are in `web/src/ui/`, one component per file with its stylesheet
beside it in `@layer components` and `ui-` class names: `Icon`, `Button` and `ButtonLink`,
`IconButton`, `Tooltip`, `Field` with `Input`, `Textarea` and `Select`, `Checkbox`, `Switch`,
`SegmentedControl`, `Badge`, `Label` (and `labelHue`, the hue a label's name picks), `Avatar`,
`AgentDot`, `Skeleton`, `Spinner` and `VisuallyHidden`. A screen imports each file directly
(`../ui/Button`); there is no index file. A screen doesn't write a `<button>`, an `<input>` or a
`<select>` of its own.

- **One height per size.** Buttons, fields, selects and segmented controls of one size are one
  height (36px regular, 28px small), so they line up in a row; on a touch screen every one of
  them is 44px.
- **States are designed.** Hover, focus (the ring), pressed, disabled (a look of its own on the
  neutral ground, never half-opacity) and, where they apply, loading and invalid.
- **The old stylesheet's rules leave them alone.** `styles.css` is in no layer, so a rule of it
  that can match an element wherever it is would beat every component's own style: one by an
  element's name (`h1` to `h3`, `p`, `code`, `pre`, `kbd`, `samp`, `a`, `button`, the fields, a
  checkbox's or a radio's label, also under `pointer: coarse`, `fieldset`), by a state or a
  pseudo-element alone (`:focus-visible`, `::placeholder`), by an attribute alone, by `*` or by
  `:is()`, or under what wraps every page (`#root`, `.tt-shell`, `.tt-body`, `.tt-page`). Every
  element a component draws has a class that starts with `ui-` (a control's native element also
  has `ui-control`), and each of those rules skips such elements: its subject carries
  `:not(:where([class^="ui-"], [class*=" ui-"]))`. `:where()` adds no specificity, and the
  screens' own elements have no `ui-` class, so they are styled exactly as before. Each component
  draws its own focus ring (`--line-focus` in `--color-focus`, 3:1 on every ground). `css-check`
  fails on such a rule outside `web/src/ui/` without it, unless its `EVERYWHERE` list allows the
  selector, only for the properties it names, with the reason. `web/src/ui/elementRules.test.tsx`
  draws every component (each gallery, and the overlays open) and fails when an element a
  component draws has no `ui-` class or a rule of `styles.css` reaches one;
  `web/src/ui/controlRules.test.tsx` checks the controls the same way, and that no rule names a
  `ui-` class outside `:where()`. A rule scoped by an old container (`.tt-x h2`) still reaches a
  component put inside that container: a rebuilt screen leaves the container behind.
- **What reaches every element on purpose** (`EVERYWHERE`): the page's `html` and `body`, border-box
  sizing, the scrollbars' and the selection's colours, the search field's clear button, and two
  rules for every field: the caret's colour, and 16px text in a window under 48rem (it keeps iOS
  from zooming in on focus), with a mouse too. A field then has 16px text beside 14px buttons; its
  height is set by the control scale, not by its text, so a row still lines up. (On a touch screen
  a field's text is 16px in any window.)
- **Headings.** The look of a heading comes from the type roles, not from the element: the display
  face only from 20px up (`--type-display`, 24px: `PageHeader`'s title; `--type-title`, 20px: a
  dialog's and a drawer's title) and Inter below. A screen's own heading is `Heading` (`level` for
  the outline, `size` `display`, `title`, `section` (the default, 16px Inter) or `subsection`), never
  a bare `<h2>`, which would get the old stylesheet's heavy display face.

**The gallery.** `pnpm --filter @taskto/web run dev`, then `/_gallery` on the dev server: the colour
and type roles and every component's specimens, with switches for light and dark, the sample second
brand and the direction. It collects every `<Name>.gallery.tsx` beside the components
(`import.meta.glob`), so a new component adds its own file. It exists on the dev server only:
`main.tsx` opens it under `import.meta.env.DEV`, so a production build has none of its code. The
query `?theme=dark&brand=harbor&dir=rtl` sets the switches, for captures. Its text goes through
`t()` like every screen's (`gallery.*` in the locale files), so the gallery can be seen in each
language.

## The buttons

Pills, as on the website. A plain button is white with a 1px control edge. **The primary button**
(the one thing a screen asks you to press) is mint with ink on it, outlined in 1.5px ink and lifted
on a hard 2px ink shadow that rises to 3px on hover and is pressed flat on click (in the dark, the
outline and the lift are the mint edge). The motion stops under `prefers-reduced-motion`. Icon
buttons, fields and cards are 8px today, through the old `--tt-radius`; as each is rebuilt on the
components it takes `--radius-md` (10px, as in the approved prototype), and 8px lives on only as
that alias until the last one has moved.

## Fonts

The website's three families, each in the role the app needs:

| Family | Role in the app | Website's role |
|---|---|---|
| Inter | all text, labels, the controls: a dense interface wants its plain, even forms | running text |
| Bricolage Grotesque | headings (`h1` to `h3`, set heavy and tight), the apps' titles | headlines, and the site's default sans |
| JetBrains Mono | paths, commands, ids and code, never labels or prose | eyebrows, paths, commands |

The website sets its body in Bricolage; the app doesn't, because forms, tables and long lists in a
grotesque with character cost reading speed. Bricolage speaks where the website uses it to speak:
in headings and in the brand. `--font-sans`, `--font-display` and `--font-mono` in `web/src/ui/tokens.css`
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

An app's own screens are drawn by the A2UI renderer from what the agent wrote; the app can't change
the renderer or what an app's definition may say. The renderer styles itself from `--a2ui-*` custom
properties with defaults of its own (a blue primary button, grey borders, 16px text).
`web/src/ui/a2ui.css` sets them on `:root` from the tokens and the roles: the interface's type sizes
(14px), its spaces, edges and radii, pill buttons and choices, Input's text box for fields and dates,
Field's label, a card as a panel, the display face for titles. What a variable can't say is the
rules under `.ui-app` at the end of the same file, the element `AppView` draws an app in: the
control heights (36px, 44px on a touch screen, a field's text 16px there), the library's focus ring
on every control instead of the renderer's change of border colour, the primary button's ink edge
and lift (the renderer draws it with no border), a disabled button as Button's flat grey pill rather
than half opacity, and the error text as Field's. So an app's controls are drawn as the component
library's in light, dark and every brand. `web/src/apps/theme.test.ts` checks that every renderer
component the server's catalog allows is themed, that each control gets the ring and the touch
height, and that every colour variable is a role.

The file is in no layer on purpose. The renderer adopts its defaults (`:where(:root) { … }`) and each
component's rules (`a2ui-basic-button .a2ui-button { … }`) at run time, in no layer and after the
page's own stylesheets; a rule in no layer beats every rule in one. So the overrides are in none
either, and each takes one more class than the renderer's rule it overrides.

The renderer colours several things with its primary, which is the mint: a borderless button's text,
a focused field's border, the picked tab, a slider's thumb, a check box's mark. Each is pointed at a
token that can be seen on the page instead (the deep green, or the focus colour), because mint there
would be 1.4:1 on cream. When the renderer gains another use of its primary, check it the same way.

A button's label is a Text component, and the renderer colours Text from `--_a2ui-text-color` (a
caption, from its own muted colour, over that). So the label is pointed at the button's ink: the
borderless button sets that variable to the deep green as well as its own `color`, a disabled one to
the muted text, and a label in a button inherits it in every variant, so a caption on a mint button
is ink on mint (12.8:1) and not muted grey (1.6:1).

TaskTo.AI's own components in an app (`web/src/apps/catalog.tsx`) are styled with the screens
(`web/src/apps/AppView.css`): a number (`Stat`) is a tile, its label small and muted over a large
value with tabular figures, and tiles in one of the app's rows wrap onto the next line when there
isn't room.

## Installed app

`web/public/manifest.webmanifest` (`theme_color` and `background_color` cream `#faf8f4`) and the
`theme-color` metas in `web/index.html` (cream; night `#111110` in the dark) match the top bar and
the page. The metas follow the OS through their media queries, which is right for the System theme;
when the person picks Light or Dark in the top bar, `web/src/theme.ts` sets both metas to that
theme's colour (and System puts back each one's own), so the browser's address bar and status bar
follow the app's choice, not the OS's. The two colours are `THEME_COLOR` there, which a test keeps
equal to the default brand's `--color-bg`, the metas and the manifest. The icons are the website's app tile (its `favicon.svg` and apple touch icon): a logo-green
(`#00a86b`) square with the logo's arrow and dot in ink, drawn by `web/scripts/make-icons.mjs` from
the same geometry as `icon.svg`. The top bar is light, so the iOS status bar is the default one (dark
text), not `black-translucent`, whose white text it would hide.

## Capturing the screens

A pull request that changes how the app looks attaches its captures from before and after.
`make screens` makes them: it builds, starts the server on an empty temporary data directory and a
free loopback port, seeds sample data through the HTTP API (invented names and `example.com`
addresses only: documents, cards with labels and due dates, comments, a shared space with boards,
a second person who joins and assigns the owner a card and mentions them, so the inbox has
notifications, a pending invite, an access token, apps installed from the templates and published, Home widgets and
pins, conversations), and visits each screen in Playwright's Chromium (`web/e2e/screens.mjs`). It
stops the server and removes the data directory when it ends, also when a step failed or it was
interrupted (Ctrl-C, `SIGTERM` and `SIGHUP` stop and reap the server, close the browser, remove the
directory and exit with 130, 143 or 129). CI never runs it. The seeding uses the script's own HTTP
client (the session cookie stays in memory), and a failed call is reported as its method, path
and status only, never a header or a body.

The PNGs go to `web/screens/`, which git ignores, as `<size>-<theme>/<screen>.png`: `desktop`
(1440x900), `tablet` (820x1100) and `phone` (390x844), each `light` and `dark` (the browser's colour
scheme, as a person's system sets it). The names are the same on every run, so a run on the base
branch and one on the change can be compared file by file, for instance `desktop-dark/boards.png`.
The screens are the first-run setup and the sign-in, every section empty, then with the sample data:
Home and its customise mode, the conversations (the list, and one with messages), the knowledge base
(the list, a document, the editor, search results), the boards (a board, a card open, adding a
card), the apps (the list, the templates, an app open, an app that uses every component of the
renderer, at its top and at its end, the builder), the inbox, every tab of Settings, the account
menu, and on the phone the menu drawer. Each is the browser's window, not the whole page, so what
scrolls out of view is not in it. A picture is saved only once the screen's own ready check is on
the page (a heading, a control or a seeded item that only that screen shows, in English): a screen
that shows something else, such as an error page or another server's, is a failed capture and the
run exits non-zero. The sample dates are relative to the day it runs, so "overdue" and "soon" read
the same every day; the times of conversations are the time of the run, so those differ between two
runs.

When the browser logs an error or a request fails, the script lists it with the capture it
happened in, and it exits non-zero when a screen could not be captured or the list of pictures (the manifest) could not be written (`--strict` also fails on
what the browser reported). Options, by flag or environment variable:

| Flag | Variable | |
|---|---|---|
| `--out DIR` | `TASKTO_SCREENS_OUT` | Where the PNGs go (default `web/screens/`). The script replaces only the files it writes, which it lists in `.screens-manifest.json` there (another file already at a capture's path fails that capture and stays): a capture that failed or was skipped loses its old picture, a full run that failed nowhere also removes the pictures an earlier run listed and this one didn't write, and no other file is ever deleted. |
| `--only TEXT[,TEXT]` | `TASKTO_SCREENS_ONLY` | Only the captures whose name contains one of the texts: `desktop-dark/`, `boards`, `phone-light/drawer`. |
| `--list` | | Print the capture names and stop. |
| `--strict` | | Fail on anything the browser reported too. |
| `--unsafe-agent` | `TASKTO_SCREENS_UNSAFE_AGENT=1` | See below. |

With `make`, pass flags in `SCREENS_ARGS`: `make screens SCREENS_ARGS="--only desktop-light/boards"`.
`TASKTO_SCREENS_BIN` runs another binary than `bin/taskto`.

**The agent runtime and the conversations.** A conversation with replies needs the server started
with `-agent`, where the scripted offline model answers. The server refuses to start the agent
when its user can run `sudo` without a password ([agent-safety.md](agent-safety.md)). The script
tries `-agent` and, when the server refuses, starts it without: it says so, the conversation list
is captured empty and the conversation detail is skipped (no picture of it is made). On a development machine you can opt in to
the override with `TASKTO_SCREENS_UNSAFE_AGENT=1` (or `--unsafe-agent`), which passes
`-unsafe-privileged-agents`: for development only, a throwaway VM where the developer is root, say;
never set it on a server people use. The script never sets it by itself.

When you add a screen, add it to `SCREENS` in the script with its `ready` check; its test
(`web/e2e/screens.test.mjs`, which runs in `pnpm run test` and needs no browser) checks that names
are unique, that every screen has a ready check and that every Settings tab is there. The same test
covers starting and stopping the server against fake server processes, the cleanup on a signal, the
HTTP client's error text and what is left in the output folder.
