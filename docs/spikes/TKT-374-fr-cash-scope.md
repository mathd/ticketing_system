# Spike report — TKT-374: French cash-system rule, applicability and gaps

**Date:** 2026-10-13 · **Type:** timeboxed research spike (no production code) · **Parent:** TKT-11
(French fiscal profile, later phase) · **Code baseline:** commit `8683c6fc`
**Outcome:** findings and a recommendation only. This note does **not** state that the product
meets the French rule. It states which paths the rule can reach, what the rule and one accredited
referential require, and where the current design falls short.

Evidence and hashes: [docs/evidence/TKT-374/README.md](../evidence/TKT-374/README.md) and
[sources.json](../evidence/TKT-374/sources.json). Source ids in square brackets (for example
[S01]) refer to that register.

How to read the quotes: French text in double quotes is verbatim from the retrieved source. An
English gloss follows it. An ellipsis (…) marks a cut. Text marked **Inference** is my reading.
It is not a statement from a source or from code.

## 1. Scope

- **Legal cutoff:** 2026-10-13. Every source has an access date and an edition (D1).
- **Rule in scope:** CGI art. 286 I 3° bis only. This is the obligation for a VAT-taxable person
  who records customer payments in cash software to use software that meets four conditions:
  inalterability, security, conservation and archiving.
- **Out of scope (named, not analysed, D13):**
  - **CGI art. 290 quater (ticketing for venues with an entry price).** It exists. BOFiP
    BOI-TVA-DECLA-20-30-20-30 (12/09/2012) [S16] says that venue operators must "délivrer un
    billet à chaque spectateur ou enregistrer et conserver dans un système informatisé les
    données relatives à l'entrée, avant l'accès au lieu de spectacle" (issue a ticket to each
    spectator, or record and keep entry data in a computer system, before entry). It applies "à
    tous les lieux de spectacles dès lors que l'accès à ces lieux est subordonné au paiement d'un
    prix d'entrée". This regime applies directly to a ticketing product. It is a separate
    obligation and needs its own spike (see §13).
  - **E-invoicing reform.** Not analysed.
- **Product state:** this repository is a testbed. Payments run only with a fake provider or a
  Stripe test key. Live keys are refused (`services/payments/cmd/payments/main.go:178`). So no
  real taxable payment is recorded today. Applicability below is about a **proposed French
  deployment**.

## 2. Source register (summary)

The full register with hashes is [sources.json](../evidence/TKT-374/sources.json).

| Id | Publisher | Title | Edition / date | Result on 2026-10-13 |
|---|---|---|---|---|
| S01 | DGFiP (BOFiP) | BOI-TVA-DECLA-30-10-30, secure cash software | 20260325, in force from 25/03/2026 | Read in full |
| S02 | DGFiP (BOFiP) | Same BOI, prior edition | 20250416 | Read; diffed against S01 (§3.3) |
| S03 | DGFiP (BOFiP) | ACTU-2026-00073, attestation restored (LF 2026 art. 125) | 25/03/2026 | Read |
| S04 | DGFiP (BOFiP) | ACTU-2025-00160, certificate deadline moved to 1 Sep 2026 | 01/10/2025 | Read (superseded regime) |
| S05 | DGFiP (BOFiP) | BOI-LETTRE-000242, attestation model | 20260325 | Read |
| S06 | Assemblée nationale | Written question 7497, ministerial answer | Answer JO 10/03/2026 p. 2113 | Read |
| S07 | Assemblée nationale | Texte adopté n° 227, LF 2026, "Texte définitif" | 2 Feb 2026 | Read art. 124–125 |
| S08 | Conseil constitutionnel | Décision n° 2026-901 DC (LF 2026) | 19 Feb 2026 | Read dispositif and §271 |
| S09 | BYCYB for LNE | Référentiel de certification des systèmes d'encaissement | Rev. 1.8, 14 Oct 2025, corrected 11 Dec 2025 | Read ch. III and Exigences 1–21 |
| S10 | BYCYB | Certification page (target of the LNE URL) | fetched 2026-10-13 | Read (accreditation number) |
| S11, S12 | INFOCERT | NF525 page and brochure | 2025 brochure | Read; NF525 rules **not obtained** |
| S13, S14 | Légifrance | CGI art. 286; LF 2026 art. 125 (JORF) | — | **HTTP 403** (bot challenge) |
| S15 | economie.gouv.fr | Cash-software certification page | — | **HTTP 403** |
| S16 | DGFiP (BOFiP) | BOI-TVA-DECLA-20-30-20-30, ticketing (art. 290 quater) | 20120912 | Existence check only |

**What was not read:**
- The consolidated text of CGI art. 286 I 3° bis was not read (Légifrance 403). Its content is
  known here through BOFiP S01 §1 and through the amending article in S07.
- The NF525 certification rules (INFOCERT) were not obtained. INFOCERT sends them "Une fois les
  formalités administratives effectuées - signature d’un devis et premier paiement d’une facture
  administrative" (after a signed quote and a first invoice payment) [S12]. No NF525 clause is
  cited in this note.

## 3. The legal change

### 3.1 Timeline

| Date | Event | Source |
|---|---|---|
| 16 Feb 2025 | LF 2025 (loi 2025-127) art. 43 removes the publisher attestation. Only a certificate from an accredited body is accepted. | S04 |
| 1 Oct 2025 | The deadline for a certificate is moved from 1 Mar 2026 to 1 Sep 2026. | S04 |
| 19 Feb 2026 | LF 2026 (loi 2026-103) is promulgated. The Conseil constitutionnel decision of the same day does not strike art. 125. | S03, S08 |
| 21 Feb 2026 | Art. 125 applies: "Ces dispositions s’appliquent à compter du 21 février 2026." | S03 |
| 10 Mar 2026 | Ministerial answer: "Le recours à un organisme agréé n'est donc plus, à ce jour, obligatoire (cf article 125 de la loi de finances pour 2026)." (Use of an approved body is no longer mandatory.) | S06 |
| 25 Mar 2026 | BOFiP publishes edition 20260325 with both proof routes. | S01 |

### 3.2 The amending text

The AN definitive text [S07], art. 125, 1°: "Le 3° bis du I de l’article 286 est complété par les
mots : « ou par une attestation individuelle de l’éditeur, conforme à un modèle fixé par
l’administration »" (adds: "or by an individual attestation from the publisher, in a form the
administration sets"). Art. 125, 2° makes the matching change to the penalty article 1770
duodecies.

The Conseil constitutionnel [S08] strikes "l'article 124" among others. §271 of the decision says
that article 124 "modifie la dénomination de certaines conventions relatives à l'échange de
renseignements en matière fiscale". In S07, article 124 is that tax-convention article, and the
cash-software text is article 125. The decision does not name article 125. BOFiP [S03] cites the
same text as art. 125 of loi n° 2026-103.

**Inference:** the numbering in S07 and in the promulgated law match for article 125, because
BOFiP uses the same number. I did not read the JORF text.

### 3.3 Both proof routes are valid now

BOFiP S01 §270: the conditions are justified "soit par un certificat délivré par un organisme
accrédité" … "soit par une attestation individuelle de l’éditeur du logiciel ou système de caisse
concerné, conforme à un modèle fixé par l’administration". It adds: "Il s’agit d’un mode de preuve
alternatif" (one of the two documents is sufficient).

**Edition diff (D9).** I compared S02 (20250416, the edition that LNE Rev 1.8 cites) with S01
(20260325) paragraph by paragraph. Method and output: evidence README §3.

- I §§10–50 and II §§60–260: the requirements are unchanged. The changes are editorial
  (spelling, date format, sentence order in §90 and §180). There are two small edits:
  - §25 adds art. 293 B bis CGI to the franchise exclusion.
  - §30 now says "soumis à l’obligation de sécurisation" where S02 said "obligation de
    certification".
- The preamble (§1) and III §§270–320 change to name both routes. §275 (the certificate-only
  transition) is deleted. §§360–390 (the attestation) are restored.

Consequence: LNE Rev 1.8 Exigences footnote S02 paragraphs that did not change in I or II. So
this note uses those Exigences as requirement text. It does **not** use LNE's certificate-only
framing. Rev 1.8 lists among its changes "la fin de l’auto-attestation" [S09]. That statement is
no longer true from 21 Feb 2026.

## 4. Who is in scope, and what is a cash system

| § (S01) | What it says (verbatim, short) | Gloss |
|---|---|---|
| §10 | "qui effectuent des livraisons de biens et des prestations de services ne donnant pas lieu à facturation" | Taxable persons who make sales that need no invoice (that is, mostly B2C sales). |
| §10 | "les assujettis qui réalisent l’intégralité de leur chiffre d’affaires avec un ou des professionnels sont exclus du dispositif" | B2B-only sellers are out. Mixed B2B/B2C sellers are in. |
| §10 | "Les sociétés mandatées pour la gestion des règlements des clients à l’aide de logiciels ou systèmes de caisse pour le compte d’un autre assujetti" | A company mandated to manage customer payments for another taxable person must also use compliant software. |
| §25 | Franchise en base, flat-rate farm refund, and exempt-only sellers are excluded. | — |
| §30 | "mémoriser et à enregistrer extra-comptablement des paiements reçus en contrepartie d’une vente" | A cash function stores and records payments for a sale outside the accounting system. |
| §30 | "quel que soit le mode de règlement (espèces, chèques, cartes bancaires, virements, prélèvements, etc.)" | Any payment method, cards included. |
| §30 | "Cette obligation s’applique y compris en cas d’enregistrement sur un logiciel ou système de caisse accessible en ligne." | Online software is included. |
| §30 | "Les terminaux de paiements seuls ou les prestataires de services de paiement" … "sont exclus du dispositif." | Stand-alone payment terminals and PSPs (as defined in CoMoFi L. 521-1) are excluded. |
| §40 | "seule la fonctionnalité de caisse enregistreuse et encaissement, et non l’ensemble du logiciel, doit être sécurisée." | In multi-function software, only the cash function must be secured. |
| §45 | "Les logiciels ou systèmes de caisse dits « libres » ou développés en interne sont également concernés par l’obligation." | Free and in-house software is included. |

**§35 banking relief: both conditions.** The relief applies only if both of these are true:

1. **All payments, all sales.** The taxable person uses, "pour tous les paiements reçus en
   contrepartie de toutes ses ventes ou prestations de services", direct intermediation by a
   credit institution under CoMoFi Book V Title I (or an EU bank under the DAC directive). The
   relief does not apply "dès lors qu’une partie des ventes ou prestations est payée par un autre
   moyen, quelle que soit l’importance de cette partie" (if any part of sales is paid another way,
   however small).
2. **Provider status.** The relief does not apply "si le prestataire de services de paiement n’a
   pas le statut d’établissement de crédit" (if the PSP is not a credit institution, or an EU
   bank under the DAC directive).

§37 Example 2 applies this to e-commerce through a PSP: the merchant is relieved "que si ce
prestataire répond aux exigences mentionnées au I-B § 35" (only if that provider meets §35).

**The internal payments service is not an L. 521-1 PSP.** The §30 PSP exclusion covers regulated
payment providers. `services/payments` is a component of this product. It records journal facts
and calls an external provider (`services/payments/cmd/payments/main.go:178` selects fake or
Stripe). **Inference:** the exclusion covers Stripe as a provider. It does not cover this
product's record of the payments. I did not read CoMoFi L. 521-1, and I did not check Stripe's
legal entity or licence.

**Is this product a cash system?** The journal records payments received for sales
(`payment.captured`, `order.completed` and others, `services/payments/internal/store/store.go:230`).
`git grep` finds no accounting integration. **Inference:** the product has a cash function in the
§30 sense. The §35 relief is the only general exit for card-only operators, and it depends on
facts this repository does not hold.

## 5. Operator scenarios (D8)

| Id | Scenario | Taxable person (assujetti) | Publisher (éditeur, §300) | Code basis |
|---|---|---|---|---|
| S1 | SaaS. The platform company runs the software. A separate organizer sells tickets. | Organizer | Platform company | Per-organizer journal chain, `store.go:292` |
| S2 | The platform collects for the organizer. The platform PSP charges the buyer for the organizer. | Organizer, and the platform under the §10 mandate sentence if a mandate exists | Platform company | [ADR-048](../adr/ADR-048-settlement-ledger.md) §5: "The platform PSP charges the buyer" |
| S3 | The platform sells its own B2C fees. | Platform company (for fee revenue) | Platform company | ADR-046 `passed_on` fees; ADR-047 `system` payee kind |
| S4 | An organizer runs this code in-house. | Organizer | Organizer (§45, §315 Example 3) | Self-hosted deployment |

§300 defines the publisher: "la personne qui détient le code source du logiciel ou système de
caisse et qui a la maîtrise de la modification des paramètres de ce produit" (the person who holds
the source code and controls its parameters).

## 6. Product paths at `8683c6fc`

| Id | Path | State | Code evidence |
|---|---|---|---|
| P1 | Web checkout (card) | Implemented | Storefront posts the order: `web/storefront/src/pages/checkout.ts:61`. Commerce calls payments for a positive total: `services/commerce/internal/api/server.go:1917`. |
| P2 | Staff sales (operational-hold conversion, group draw-down) | Implemented as APIs. Payment goes through the shared checkout. No cash. | `services/commerce/internal/api/server.go:2052` |
| P3 | Refunds, voids, bulk cancellation refunds | Implemented | `services/commerce/internal/refunds/service.go:83` (refund), `:128` (comp/void reversal); `services/commerce/internal/bulkrefund/runner.go:342`; recovery void `services/commerce/internal/recovery/clients.go:175` |
| P4 | Exchanges | Implemented. A positive delta charges, a negative delta refunds, zero calls no provider. Both gross legs are journaled. | `services/commerce/internal/api/exchanges.go:698`, `:831` |
| P5 | Comps and zero-total orders | Implemented. Total zero skips the charge. | `services/commerce/internal/api/server.go:1904` |
| P6 | Partner sales (merchant-of-record terms) | Implemented | `services/commerce/internal/api/partner.go:514`; ADR-048 §5 |
| P7 | Settlement attribution | Implemented. It commits with the capture fact. It is outside the hash chain. | `services/payments/internal/store/store.go:329`, `services/payments/internal/store/settlement.go:278` |
| P8 | Box office / POS with cash | Planned (TKT-16) | `docs/product/prd-v1.md:52` |
| P9 | Cashless wallet: top-up, spend, balance refund | Planned (TKT-15) | `docs/product/prd-v1.md:51`. The existing "wallet" is a read of past purchases, `services/commerce/internal/api/wallet.go:3`. |
| P10 | Invoices and receipts with sequential numbers | Planned (TKT-34) | `docs/product/prd-v1.md:64` |
| P11 | Test mode | Implemented (fake / Stripe test only) | `services/payments/cmd/payments/main.go:178` |

## 7. Applicability matrix

Legend: **IN** = in scope; **OUT** = out of scope; **U** = undetermined (facts missing). "U(35)"
means: in scope unless both §35 conditions hold.

| Path | S1 | S2 | S3 | S4 | Deciding § (S01) | Missing facts |
|---|---|---|---|---|---|---|
| P1 Web checkout | U(35) | U(35) | U(35) | U(35) | §10, §30, §35, §37 Ex. 2 | Customer class (B2C share); the operator's full payment mix; the PSP's legal entity and status; §25 exclusions (franchise, exempt-only) |
| P2 Staff sales (card) | U(35) | U(35) | U(35) | U(35) | same as P1 | same as P1 |
| P3 Refunds, voids | Follows the sale | Follows the sale | Follows the sale | Follows the sale | §50 (last item: correction traces), §90 | none beyond P1 |
| P4 Exchanges | Follows the sale | Follows the sale | Follows the sale | Follows the sale | §50, §90 | none beyond P1 |
| P5 Comps, zero total | U | U | OUT (no fee) | U | §50 ("peut conduire à l’émission … d’un justificatif") | Whether a no-payment issue is a "transaction" under §50. Zero face value with a positive fee is a payment and follows P1. |
| P6 Partner sales | U(35) | U(35) | U(35) | n/a | §10, §30, §35 | Who is the seller under merchant-of-record terms |
| P7 Settlement | OUT | OUT | OUT | OUT | §30 | **Inference:** it records no payment from a customer. It splits a captured payment. Its fee lines feed S3 revenue. |
| P8 POS with cash | IN | IN if the platform collects at the counter | n/a unless fees are sold at the counter | IN | §30 ("espèces"), §35 ("quelle que soit l’importance de cette partie") | VAT regime only (§25) |
| P9 Cashless spends | IN (**Inference**) | IN (**Inference**) | n/a | IN (**Inference**) | §30, §35 | Spends are paid with stored value, not by a credit institution. |
| P9 Top-up, balance refund | U | U | n/a | U | §30, §50 | Whether a top-up is a payment "en contrepartie d’une vente" (S01 §30). S01 does not say. |
| P10 Invoicing | IN for payments from individuals | same | same | same | §40, §10 | B2B-only invoices are out (§10). §50, §170 and §200 have specific remarks for invoicing software. |
| P11 Test mode | IN when deployed | IN | IN | IN | §50 ("module de type « école » ou « test »"), §150 | none |

**Key finding: cash at the box office removes the relief for the whole operator.** If an operator
takes cash at a TKT-16 counter, or uses TKT-15 stored value, the §35 relief is lost for all its
sales. That includes its web checkout: "dès lors qu’une partie des ventes ou prestations est payée
par un autre moyen". So the planned POS and cashless paths decide P1's status for any operator who
uses them.

## 8. Requirement map

Columns:
- **BOFiP § (S01):** the official guidance.
- **LNE Ex. (S09):** the accredited referential, Rev 1.8.
- **Today:** what the code provides at `8683c6fc`.
- **Gap**, and the **Owner** per D12.

Quotes are kept short. NF525 clauses: not verified (§2).

| Group | BOFiP § | LNE Ex. | Today | Gap | Owner |
|---|---|---|---|---|---|
| Line data | §50: "doivent être enregistrées ligne par ligne": receipt number, date to the minute, register number, total incl. tax, per-line label, quantity, unit price, line total excl. tax, VAT rate, payment data, correction traces | Ex. 3: adds "un identifiant unique de l’établissement" and the payment date. Elementary data must be stored "et pas seulement pouvoir être retrouvées sur la base d’un calcul". "Un code article, identifiant article n’est pas suffisant." | Journal columns: `fact_id, organizer_id, sequence, fact_type, occurred_at, buyer_id, amount, currency, payload` (`services/payments/internal/store/migrations/0001_journal.sql`). Payload may hold only `order_id` (`store.go:239`). Commerce `reservations` holds `quantity, unit_amount, total_amount, ticket_type_id` (`services/commerce/internal/store/migrations/0001_checkout.sql`), outside the chain. | Missing from signed data: receipt number, register id, establishment id, label, quantity, unit price, line total excl. tax, VAT rate, payment method. No VAT concept exists in `services/`. **Conflict with ADR-003 Decision 4**, see §9. | TKT-32 (VAT rate, line total excl. tax); TKT-34 (receipt number); TKT-16 / TKT-379 (register, establishment); ADR-003 amendment ticket (journal fields) |
| Corrections | §90: "par des opérations de « plus » et de « moins » et non par modification directe des données d’origine" | Ex. 4 | Compensating facts: `order.refunded`, `payment.voided`, `payment.refunded`, the exchange pair (`store.go:230`). Append-only triggers (`0001_journal.sql`). | Corrections are per order and amount. They are not per line, because the journal has no lines. The link to the original is `order_id` only. | TKT-11 profile; ADR-003 amendment ticket |
| Test mode | §150: "factice" or "simulation" on receipts; operator id | Ex. 5: test data "explicitement identifiées" and secured like real data | Provider choice by key (`main.go:178`). Journal entries carry no test flag. | No test marker in the signed record or on receipts | TKT-11 profile → TKT-376, TKT-378 |
| Closures | §170: daily, monthly and yearly closures. "Ces trois échéances sont cumulatives et impératives." | Ex. 6: no new, changed or cancelled transaction on a closed period | None (`git grep` finds no fiscal closure code in `services/`) | All. Late facts are a design question: exchange legs are written one after the other (`exchanges.go:831`), and recovery appends later. | TKT-376 (daily), TKT-377 (month, fiscal year) |
| Totals and counters | §170: period grand total and perpetual total, "ne se remettant jamais à zéro"; counters continue across a version change | Ex. 7: totals per closure; "générées séparément pour chaque assujetti" | Per-organizer chain under a `journal_heads` row lock (`store.go:292`). This matches per-taxpayer separation in S1. | No totals. Order facts, payment facts and exchange legs cannot simply be summed. A revenue definition is needed. | TKT-376, TKT-377 |
| Inalterability | §80, §90 (also against "modifications non tracées effectuées par le propriétaire et détenteur des données lui-même"), §100 (proof that data is unchanged "depuis son enregistrement"), §120 | Ex. 8: chain or keyed hash. The key must be unknown to "l’utilisateur final (le professionnel assujetti)". Restore to an earlier state: "L’opération doit être détectée ou rendue impossible." | SHA-256 chain plus HMAC-SHA-256 per entry (`store.go:132`, `:138`, `:312`). Keyring with retired keys (`services/payments/internal/store/keyring.go:33`). Verifier (`store.go:362`). HMAC-SHA-256 is on Ex. 8's acceptable list. | (a) Coordinated rollback is not detected (§10). (b) Key custody: the HMAC ring is secret material, and "every holder of the ring can forge" (`keyring.go:40`). In S2, S3 and S4 the taxable person holds it. (c) Coverage: Ex. 8 covers Ex. 3, 4, 5, 7, 9, 15 and 18 data. Today only journal columns are signed. Settlement entries are "**not** in the journal's hash chain" (ADR-048). | TKT-375 (rollback), TKT-11 (key custody) |
| Security | §130: "Il doit empêcher leur suppression ou modification sans laisser de trace." Covers §50 data, changes, and receipt production data. §140: "de nature à garantir la restitution des données dans l’état de leur enregistrement d’origine" | Ex. 8 robustness: "Vérifier la sécurité des éléments en bout de chaîne, notamment contre leur suppression." | As above | Receipt production data is not in the journal. The chain tail is not protected against deletion together with its head (§10). | TKT-375; TKT-34 |
| Receipts and reprints | §130 (data for receipts issued) | Ex. 9: before-payment and after-payment receipts must be distinct; "duplicata" on reprints; secured print and send trace. Its note says: "Facture, note, reçu, ticket, billets sont des justificatifs." | The ticket lifecycle trail records issue, delivery and reissue events (ADR-003 Decision 2). I did not check them against Ex. 9. | **Inference:** a ticket is itself a receipt under Ex. 9. No receipt number, no duplicate marking, and no print trace tied to the money journal. | TKT-34; TKT-379; TKT-11 |
| Archive | §220 (at most yearly; "donner date certaine aux données archivées"), §230 ("format ouvert", French notice), §240 (archive generation trace), §250 | Ex. 10 (open format, frozen and time-stamped, totals included), Ex. 11 (one archive covers at most one year or fiscal year; any past period under 7 years), Ex. 12 (integrity can be checked without the system) | None | All | TKT-378 |
| Purge | §155, §250, §260: "La purge n’est que partielle" (totals stay online) | Ex. 13, Ex. 14 | No purge. The journal is append-only. | A retention design is needed before any purge. Totals must never be purged. | TKT-378 |
| Operation traceability | §240 | Ex. 15: archive, purge and restore operations, with time stamp and terminal id | None | All | TKT-378; TKT-375 (restore) |
| Conservation | §155 (six years), §180 ("et pas seulement sur le Z de caisse") | Ex. 16 (6 years, checked as 7), Ex. 17 (archives) | The journal keeps all entries. No retention policy is written. | Retention period, storage failure handling, backups versus archives | TKT-378 |
| Centralisation, offline | §190 | Ex. 18: reliable transfer, a short offline period, user is told when offline. A browser web app "n’est pas considéré comme un système centralisateur". | Web paths are client-server. Offline admission (ADR-025) is for entry, not payment. | Applies to a planned POS with "offline tolerance" (`prd-v1.md:52`) | TKT-16, TKT-379 |
| Tax-authority access | §60 ("un accès de l’administration fiscale à l’ensemble des données enregistrées"), §100 | Ex. 19: automated integrity check; a French manual "compréhensible par un non-informaticien" | Operator command `verify-journal` (`services/payments/cmd/payments/main.go:74`) | No access for the tax authority, no French manual, no export of compliance settings | TKT-378; TKT-11 |
| Fiscal perimeter, versions | §330, §340 (major version = parameters that affect the four conditions changed), §380 (attestation for later minor versions) | Ex. 20 (list every fiscal-perimeter source file and fingerprint it), Ex. 21 (major and minor version numbers; any fiscal-perimeter code change increments the major version) | None. Continuous delivery to `main` (`docs/conventions/continuous-delivery.md`). | No perimeter and no version scheme. **Inference:** frequent releases that touch fiscal code would each be a new major version that needs a new certificate or attestation. | TKT-11 |
| Organisation | — (§290: the publisher, not the taxable person, obtains the certificate) | Ch. III: compliance management system (roles, legal watch, controls, design records, subcontractor control, distribution traceability). Ex. 1: seven documents in French. Ex. 2: supporting documentation. | None | All | Owner decision (TKT-11) |

## 9. Gaps against ADR-003, and the conflict

[ADR-003](../adr/ADR-003-append-only-audit-trail.md) Decision 4 says that period closures, fiscal
archives and audit export "must layer on the money journal **without schema or flow changes**".
TKT-11's PRD row says the same (`docs/product/prd-v1.md:47`).

**The conflict.** BOFiP §50 and LNE Ex. 3 require line-level data in the secured record: label,
quantity, unit price, line total excl. tax, VAT rate, receipt number, register id and
establishment id. Ex. 8 requires that all of it is covered by the integrity mechanism. The
journal cannot hold this data today. `validate()` refuses every payload key except `order_id`
(`services/payments/internal/store/store.go:239`). The producers (commerce, payments) do not send
it, and no VAT data exists. To satisfy §50 you must change the journal payload or schema, or the
producers, or both. That **conflicts with ADR-003 Decision 4**. This spike does not edit ADR-003
(D4).

What is additive and fits Decision 4 (**Inference**): closures and totals, archive export,
operation traceability and an external witness for rollback can be built as new components that
read the journal. Line data and receipt identity cannot.

Also: ADR-003's title has a readiness label for this regime. The gaps above do not support that
label as written. The amendment ticket should review it.

**Follow-up (D11):**
1. The owner records a decision on TKT-11, through this spike's `kind=spike-result`.
2. A new Backlog ticket amends ADR-003 Decision 4 (journal line data and receipt identity).
   Gate 1 for that ticket stays human.

Other ADR-003 gaps:
- Settlement is outside the chain ([ADR-048](../adr/ADR-048-settlement-ledger.md)). This matters
  for S3 fee revenue.
- The journal records one gross `amount` per fact. Whether that amount includes tax is not
  stated anywhere, because no tax concept exists.
- ADR-003's own note says its ISCA reading is "inferred from how NF525 works, not verified against
  the standard's text". This spike does not close that, because the NF525 rules were not obtained.

## 10. Rollback: name the adversary

Per [ADR-021](../adr/ADR-021-ticket-lifecycle-trail-integrity.md) (§The trust boundary), state
inside the database cannot constrain an adversary who writes to the database.

| Adversary | Can do today | Who it is, per scenario |
|---|---|---|
| A. Database writer without the HMAC key | Roll back a chain suffix and its head together. Remove an organizer. Restore an older snapshot. ADR-003 §Status: `verify-journal` "**cannot detect a coordinated rollback**". | S1: platform staff with database access. S2, S3: the platform, which is also the taxable person. S4: the organizer, who is the taxable person. |
| B. Holder of the HMAC key ring | Forge any entry under any key id in the ring (`keyring.go:40`) | S1: the platform. S2, S3: the platform (taxable). S4: the organizer (taxable). |

The guidance and the referential target the data owner itself:
- BOFiP §90 protects against "modifications non tracées effectuées par le propriétaire et
  détenteur des données lui-même" (untraced changes by the owner and holder of the data).
- BOFiP §130: "empêcher leur suppression ou modification sans laisser de trace" (prevent deletion
  or change without a trace).
- LNE Ex. 8 asks that the key is not known to the taxable professional. It asks that a restore to
  an earlier state "doit être détectée ou rendue impossible". The text puts that sentence under a
  condition: "un poste pour lequel l’utilisateur dispose des droits administrateurs" (a machine
  where the user has administrator rights). That is true of the taxable person in S2, S3 and S4.
  The Ex. 8 robustness check has no condition: "Vérifier que la restauration d’une base ou d’un
  dossier contenant les données protégées est empêchée ou détectée."

The two governing tests:
- `TestVerifyLifecycleAcceptsACoordinatedRollback`
  (`services/access/internal/store/lifecycle_smoke_test.go:1224`) asserts that a coordinated
  rollback of the ticket trail **verifies clean**. It pins the limitation on purpose.
- `TestJournalVerifyRejectsHeadEntryDesync`
  (`services/payments/internal/store/journal_smoke_test.go:364`) detects a head that disagrees
  with its entries. A coordinated rollback moves both, so this test does not cover it. The
  journal's rollback blindness is stated in ADR-003 §Status, not pinned by a test.

Both are correct about today's code. The lifecycle test pins a state, and the journal has the same
blindness. §90, §130 and Ex. 8 appear to forbid that state. So **TKT-375 (witness journal heads
outside the database) is a requirement, not hardening**, for any in-scope deployment.

In S2, S3 and S4 the taxable person is adversary A and adversary B. **Inference:** the witness and
the key must then sit with a party the taxable person does not control. The Ex. 8 robustness check
for case 3 mentions control "se basant sur un tiers de confiance" (based on a trusted third
party).

## 11. Proof-route recommendation per scenario

This is a recommendation with stated premises. It is not a compliance claim. It issues no
attestation, and it starts no certification.

Rules applied:
- §290: "Ce n’est pas l’assujetti qui demande la certification du logiciel ou système de caisse
  qu’il détient à l’autorité certifiante." (The publisher obtains the certificate, not the
  taxable person.)
- §375: the publisher who issues the attestation "ne peut pas être l’assujetti à la TVA au nom
  duquel est établie l’attestation, sauf si l’activité déclarée par cet assujetti est une activité
  d’édition de logiciels ou de systèmes de caisse". Also: "L’activité d’éditeur de logiciel devra
  être réelle et corroborée."
- §315 Example 3: for in-house software, "L’entreprise est considérée comme étant « l’éditeur »".
- The attestation must follow BOI-LETTRE-000242 [S05]. Part 1 is signed by the publisher's legal
  representative and names the version and the covered and excluded functions. Part 2 is signed by
  the user company.

| Scenario | Routes available | Recommendation | Premises to confirm |
|---|---|---|---|
| S1 SaaS | Certificate or attestation | Attestation is a valid route. Design to certification grade anyway (D10). | The platform holds the source code and controls the fiscal parameters (§300). Organizers cannot change them (§310). A version root and minor-version scheme exists (§380). |
| S2 Collector | For the platform's own obligation: attestation only if its declared activity is cash-software publishing (§375); else certificate | Certification (provisional) | The mandate fact; the platform's declared activity |
| S3 Own fees | Same as S2 | Certification (provisional) | Same as S2 |
| S4 In-house | Certificate only (§375, §315 Ex. 3), unless the organizer's declared activity is cash-software publishing | Certification | Who modifies the code. Any change to fiscal functions invalidates the document (§315). |

**Overall:** use certification as the planning target. Treat the attestation as an option for S1
only, after the owner confirms the premises. LNE (via BYCYB) is one accredited route. BYCYB states
COFRAC accreditation "n°5-0696" under ISO/CEI 17065 [S10]. §320 accepts any body accredited under
C. consom. L. 433-4.

## 12. Design-to baseline for TKT-375..379 (D10)

The applicability verdict can stay `undetermined`. The children still need a target. Design to
this baseline unless the owner relaxes it:

1. **Assume in scope.** Design for an operator who takes cash (P8), because that removes §35 for
   everything.
2. **Full §50 / Ex. 3 line data in the secured record.** This depends on the ADR-003 amendment
   (§9).
3. **Certification-grade evidence:** LNE Ex. 3–19 as the requirement text, Ex. 20–21 for release
   control.
4. **Per-taxpayer separation.** Totals per organizer (Ex. 7). The existing per-organizer chain is
   the base.
5. **Rollback detected outside the database.** The key is not known to the taxable person (Ex. 8).
6. **Archive:** open format, French notice, one year or fiscal year per archive at most, integrity
   checkable without the system (Ex. 10–12). Archive, purge and restore operations are traced
   (Ex. 15).
7. **Tax-authority access with an automated integrity check and a French manual** (Ex. 19).
8. **No reset of perpetual totals** across software versions (§170).

## 13. Inputs to children and other owners (D12)

| Ticket | Input |
|---|---|
| TKT-375 | Rollback detection is a requirement (§90, §130, Ex. 8). Name adversaries A and B per scenario (§10). The witness and the key are outside the taxable person's control in S2, S3 and S4. Restore operations need a trace (Ex. 15). |
| TKT-376 | Daily closure (§170, Ex. 6). No new transactions in a closed period, so define where a late fact goes. Write a revenue definition for summing order, payment and exchange facts. Totals per organizer (Ex. 7). Test-mode data is separated (Ex. 5). |
| TKT-377 | Month and fiscal-year closure. A fiscal year can differ from the calendar year, so it is per organizer. The perpetual total never resets on a version change (§170). |
| TKT-378 | Archive, purge, conservation, tax-authority access (§§155–260, Ex. 10–17, Ex. 19) |
| TKT-379 | POS and cashless facts: register id, establishment id, payment method per tender, offline trace (Ex. 3, Ex. 18) |
| TKT-32 | VAT rate and line total excl. tax per line, in the secured record (§50) |
| TKT-34 | Receipt and invoice numbering. Before-payment and after-payment receipts, duplicates, and print trace (Ex. 9). Remarks for invoicing software (§50, §170, §200). |
| TKT-16, TKT-15 | Cash or stored value removes §35 relief for the whole operator (§7). POS offline mode is Ex. 18. |
| New Backlog ticket | Amend ADR-003 Decision 4 for journal line data and receipt identity (§9, D11) |
| Proposed follow-up | CGI art. 290 quater (ticketing for venues with an entry price) is confirmed to exist [S16]. It needs its own spike (D13). |

## 14. Open questions for the owner

1. **Deployment scenario.** Which of S1–S4 will run in France? Who is the seller of the tickets?
   Does the platform charge its own fees to buyers? Does a collection mandate exist (S2)?
2. **Payment provider.** Which legal entity and licence does the PSP hold (credit institution,
   payment institution, e-money institution)? This decides §35 condition 2. Not verified here.
3. **Payment mix.** Will any operator take cash, vouchers or stored value? If yes, §35 relief is
   lost for all its sales.
4. **Operator VAT facts.** Are buyers individuals? Is any operator B2B only, under the franchise,
   or exempt only (§25)?
5. **NF525 mark.** Do you want the INFOCERT NF525 mark specifically? The rules come only after a
   signed quote and a first payment [S12]. That is a purchase decision. LNE/BYCYB Rev 1.8 is
   public and is the referential used here (D7).
6. **Platform company's declared activity** (NAF/NACE). It decides §375 for S2 and S3.
7. **Distribution model.** SaaS only, self-hosted, or a free-software licence? §45 and §315
   Example 4 make the user company the publisher for free software.
8. **ADR-003 Decision 4.** Do you accept the amendment path in §9?
9. **CGI art. 290 quater.** Do you want a follow-up spike?

## 15. COS verdicts

**COS-1 — "cites the current legal text, official tax guidance and the actual referential clauses
for each proposed cash path": met, with two stated residuals.**
- Official tax guidance: met. BOFiP 20260325 (S01) is read in full and cited by paragraph for each
  path (§7). The edition diff against 20250416 is done (§3.3).
- Referential clauses: met under D7. BOFiP II §§60–260 and LNE Rev 1.8 Exigences 1–21 are cited
  by number (§8). **Residual:** the NF525 (INFOCERT) rules were not obtained. No NF525 clause is
  cited.
- Legal text: partly met. The amending article (LF 2026 art. 125) is read in the AN definitive
  text (S07). The Conseil constitutionnel decision (S08) does not strike it. **Residual:** the
  consolidated text of CGI art. 286 I 3° bis was not read, because Légifrance answered HTTP 403.
  For the base wording of 3° bis, this part of COS-1 is met through official guidance (S01 §1,
  S03, S06) only.

**COS-2 — "lists gaps against ADR-003 and states whether certification or publisher attestation is
the target; it does not assert compliance": met.**
- Gaps: §8 and §9, with the ADR-003 Decision 4 conflict stated.
- Target: §11. Certification is the planning target. Attestation is an option for S1 on stated
  premises.
- This note makes no compliance claim.
