# ADR-089: Québec French typography and display formatting

Date: 2026-11-28

## Status

Accepted (TKT-531, slice S1; plan approved under `config.gates: autonomous`, decision log on the
ticket).

The typography rule below is the owner's **working rule**. TKT-593 (the Québec native-speaker
review, human-owned) confirms or changes it. Until then, this ADR and its tests hold the rule.

## Context

The owner decided on 2026-10-12 that the French of this product is Québec French first. The same
decision keeps the French content key, the route and the metadata as `fr`, and formats French
displays with `fr-CA`.

Three components show French text:

- the storefront (`UI_STRINGS.fr` in `web/storefront/src/lib/locales.ts`);
- the back office (`UI_STRINGS.fr` in `web/backoffice/src/lib/ui-strings.ts`);
- the commerce password-reset message (`resetCopies["fr"]` in
  `services/commerce/internal/api/password_reset.go`).

The scanner has no French UI.

Before TKT-531, the copy followed France French typography in some places and not in others. Four
storefront strings had a space before `?`. The French reset message had a plain space before a
colon. The back-office test accepted U+00A0 or U+202F before high punctuation. The storefront had
no typography test.

Display formatting used the `fr` locale. CLDR `fr` groups thousands with U+202F and writes times as
`19:30`. CLDR `fr-CA` groups thousands with U+00A0 and writes times as `19 h 30`. One storefront
date (the wallet purchase date) and one back-office number (the venue GA capacity) were formatted
outside the storefront's format module.

## Possible Solutions

- **Option 1: keep France French typography and `fr` formats.**
    - Pros: no change.
    - Cons: contradicts the owner's decision.
- **Option 2: rename the content locale to `fr-CA`.**
    - Pros: one locale value for content and display.
    - Cons: changes the `/fr/` routes, `lang`, hreflang, the cookie values and the catalog's
      locale data. No French content differs between Québec and France today, so the rename buys
      nothing for the copy.
- **Option 3: keep the `fr` content key, write the copy with Québec typography, and map `fr` to
  `fr-CA` at the display-format sinks.**
    - Pros: no route, metadata or data change. One place per web app decides the display locale.
    - Cons: two locale values to keep apart in code and tests.

For the typography check itself:

- **Accept U+202F as well as U+00A0.** Rejected for now: one exact character is simple to test,
  and TKT-593 can widen the rule.
- **One shared helper for the three test suites.** Rejected: the suites use two languages and
  three packages, and the helper is short. Each suite keeps a local copy with the same rule IDs.

## Decision

We adopt Option 3.

### 1. Content locale and display locale

- The content key stays `fr`. The route stays `/fr/`. The page keeps `<html lang="fr">`. French
  hreflang uses `fr` when it is published.
- Display formatting for the content key `fr` uses `fr-CA`. English stays `en`.
- **Storefront:** one map, `DISPLAY_LOCALES` in `web/storefront/src/lib/format.ts`. Every
  storefront display format is built in that file: money, start times, ticket history times and
  the wallet purchase date (`accountDateFormatter`). A source scan in
  `web/storefront/test/format.test.ts` refuses `Intl.<member>` and `toLocale*String(` anywhere
  else in `web/storefront/src`. It exempts only the type `Intl.LDMLPluralRule` and
  `new Intl.PluralRules(locale)` in `lib/locales.ts`, because plural selection follows the content
  locale.
- **Back office:** the one French number sink, the venue GA capacity in
  `web/backoffice/src/pages/index.astro`. It has no source scan. The venue version timestamp in
  `web/backoffice/src/pages/venues/[id].astro` is out of scope: it takes no operator language.
- **Scanner:** no French UI, no change.
- A later France French variant must change both sites: the storefront map and the back-office
  sink.

### 2. Typography rule (working rule pending TKT-593)

"Space" means a character with the Unicode `White_Space` property, including tab, U+00A0 and
U+202F.

| Rule | Requirement |
|---|---|
| P1 | No space immediately before `?`, `!` or `;`. |
| P2a | The only space allowed immediately before `:` is U+00A0, whatever follows the colon. |
| P2b | U+00A0 immediately before `:` when the colon is followed by a space or ends the text. A colon inside a token, such as `https://` or `19:30`, passes. |
| P3a | U+00A0 immediately after `«`, also when `«` ends the text. |
| P3b | U+00A0 immediately before `»`, also when `»` starts the text. |

- P2 and P3 require exactly U+00A0. U+202F fails.
- The rule applies to the storefront's French strings, including every plural form, to the
  back office's French strings, and to the French reset subject, body template and lifetime
  phrases. The composed message is not scanned: its link is not French text. An exact-body test
  pins the composed message.
- Each test suite has its own helper with the same rule IDs:
  `web/storefront/test/french-typography.test.ts`, `web/backoffice/test/ui-strings.test.ts` and
  `services/commerce/internal/api/password_reset_locale_test.go`. A change to the rule changes
  all three.
- In Go raw strings, U+00A0 is a literal character with a comment next to it. Elsewhere it is
  written `\u00a0`.
- Out of the rule, for TKT-593: U+202F acceptance, spacing around the em dash, and the spaces
  inside `Intl` output (`fr-CA` writes `19 h 30` with U+0020).

### 3. Expected values in tests

The tests pin full `fr-CA` strings, for example `1 234,50 €` (U+00A0 twice),
`vendredi 18 septembre 2026 à 19 h 30` and `15 juill. 2026, 04 h 00 min 05 s UTC+5:30`. These
values follow from the owner's `fr-CA` decision: they are CLDR `fr-CA` output. They were checked on
Node 26.13.1 (ICU 78.3), the version CI and the storefront image use, and in host Chrome for the
Pay label and the ticket history line. An ICU update that changes them fails the tests. That
failure needs a decision, not a new expected value copied from the run.

## Consequences

- **Positive:**
    - French copy has one tested rule in all three components.
    - The storefront has one seam for display formatting, and a test keeps new formatters in it.
- **Negative:**
    - `fr-CA` shows CAD as a bare `$` (`1 234,50 $`; `fr` gave `1 234,50 $CA`) and USD as
      `1 234,50 $ US`. English shows `CA$` for CAD and a bare `$` for USD. A buyer who switches
      language on a CAD offer sees `$` mean different currencies. TKT-593 reviews this. No test
      pins it.
    - History zone labels change with the locale. `fr-CA` names some North American zones with
      French abbreviations, such as `HAE` for Toronto in summer, and other zones by offset, such
      as `UTC+2` for Paris in summer. ADR-083 §7 still holds: every time has a zone label, and a
      missing or unknown zone shows labelled `UTC`.
    - The wallet purchase date formats in the process time zone, as before. ADR-083 §7 does not
      govern it.
    - The source scan reads text. It does not see `Intl` reached by destructuring or by a computed
      member such as `Intl['DateTimeFormat']`.
    - The back-office sink has no scan. A new back-office number or date format must apply the
      map itself.

## See also

- [ADR-083](ADR-083-access-reads-performance-time-zones-from-catalog.md#decision) §7: history
  times show the performance zone, with labelled `UTC` for a missing or unknown zone. This ADR
  does not amend it.
- [ADR-050, TKT-366 amendment](ADR-050-transactional-mail-and-password-recovery.md#tkt-366-amendment-the-language-of-the-reset-message):
  the closed table of reset copy. This ADR changes the typography of its French entry, not the
  set of locales.
- [ADR-001](ADR-001-go-typescript-stack.md): money is integer minor units with an ISO currency
  code. The display locale changes only the presentation.

## References *(optional)*

- TKT-531 (this decision, slice S1), TKT-593 (native-speaker review), TKT-365 (French purchase
  path), TKT-366 (French reset message), TKT-368 (back-office UI language).
