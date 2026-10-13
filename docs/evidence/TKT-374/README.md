# TKT-374 evidence — how the sources were collected and how to re-check them

This folder supports [the spike note](../../spikes/TKT-374-fr-cash-scope.md). The machine-readable
register is [sources.json](./sources.json). It gives the id, publisher, edition, URL, access date,
HTTP result, sha256 and use of each source.

All fetches happened on **2026-10-13**. The code baseline is commit `8683c6fc`.

## 1. What is committed, and what is not

Only this README and `sources.json` are committed. No retrieved document is in the repository.

- **BYCYB / LNE referential (S09) and INFOCERT files (S11, S12, X01, X02):** not committed. They
  are copyright documents. The register keeps the URL and the sha256. The note quotes only short
  extracts.
- **BOFiP pages (S01–S05, S16):** the pages say "Sauf mention contraire, tous les contenus de ce
  site sont sous licence etalab-2.0". They could be committed. They are not, so that the evidence
  stays one kind of record (URL plus hash). Re-fetch them to re-check.
- **Assemblée nationale and Conseil constitutionnel pages (S06–S08):** not committed, same reason.
- **Légifrance and economie.gouv.fr (S13–S15):** nothing was retrieved. Each answered HTTP 403 with
  a Cloudflare "Just a moment..." challenge page.

## 2. Fetch log

Two sessions fetched the sources on the same day:
- **Orchestrator:** S01, S03–S06, S09–S12, X01, X02, and a first Légifrance attempt.
- **Implementer (this run):** S02, S07, S08, S16, a second attempt at S13 and S14, and S15. In
  fix round 2: S17.

The implementer fetched with:

```sh
curl -sS -L -A 'Mozilla/5.0 (X11; Linux x86_64)' -o <file> -w '%{http_code} %{url_effective}\n' '<url>'
sha256sum <file>
```

| Id | URL (short) | HTTP | Extracted |
|---|---|---|---|
| S01 | bofip …/identifiant=BOI-TVA-DECLA-30-10-30-20260325 | 200 | Full text (I–IV) |
| S02 | bofip …/identifiant=BOI-TVA-DECLA-30-10-30-20250416 | 200 | Full text, for the D9 diff |
| S03 | bofip …/ACTU-2026-00073 | 200 | Announcement text, commencement date |
| S04 | bofip …/ACTU-2025-00160 | 200 | Prior regime and deadlines |
| S05 | bofip …/identifiant=BOI-LETTRE-000242-20260325 | 200 | Attestation parts 1 and 2 |
| S06 | questions.assemblee-nationale.fr …/17-7497QE.htm | 200 | Ministerial answer |
| S07 | assemblee-nationale.fr …/l17t0227_texte-adopte-seance.pdf | 200 | Art. 124 and art. 125 |
| S08 | conseil-constitutionnel.fr …/2026901DC.htm | 200 | Dispositif; §271 (art. 124) |
| S09 | bycyb.com …/FRANCAIS-REF-1.8-CSC-AU-12-12-2025-publie.pdf | 200 | Revision table, I.2, ch. III, Exigences 1–21 |
| S10 | lne.fr …/referentiel-certification-systemes-caisse.pdf → bycyb.com/certification-des-systemes-de-caisse/ | 200 after redirect | COFRAC accreditation number |
| S11 | infocert.org/nf525/ | 200 | How NF525 rules are obtained |
| S12 | infocert.org …/Plaquette-NF525-FR-ENG.pdf | 200 | Procurement sentence |
| S13 | legifrance …/LEGIARTI000053546765/2026-02-21 | **403** | Nothing |
| S14 | legifrance …/JORFARTI000053508893 | **403** | Nothing |
| S15 | economie.gouv.fr/entreprises/certification-logiciel-caisse | **403** | Nothing |
| S16 | bofip …/1054-PGP.html → BOI-TVA-DECLA-20-30-20-30-20120912 | 200 | §1 and §10 only (existence check) |
| S17 | tools.cofrac.fr/annexes/sect5/5-0696.pdf | 200 | Full certificate and technical annex (4 pages) |

Text extraction:
- **HTML:** a small Python script removes `script` and `style`, turns block ends into new lines,
  removes tags and runs `html.unescape`. Run it with `python3 -I`.
- **PDF:** `pdftotext -layout`. For S09 and S12, also plain `pdftotext` (no `-layout`), because S12
  has two columns.

## 3. D9 edition diff (S02 against S01)

Method:
1. Extract the text of both editions.
2. Cut each document at its paragraph numbers (lines that hold only a number such as `10` or
   `(110)`).
3. Normalise white space and quote marks.
4. Compare each paragraph word by word (`difflib.SequenceMatcher`).

Result for **I §§10–50 and II §§60–260:** no change to any requirement.

| § | Change |
|---|---|
| §10 | Quote marks around "B to B" only |
| §25 | Adds a reference to CGI art. 293 B bis to the franchise exclusion |
| §30 | "obligation de certification" becomes "obligation de sécurisation". "Sont visés par l’obligation de sécurisation" becomes "Sont également visés par cette obligation". |
| §35 | Adds "modifiée" after the directive reference |
| §50 | "toute taxe comprise" becomes "toutes taxes comprises" |
| §90 | "Il ne s’agit pas seulement de protéger les données contre" becomes "Il s’agit de protéger les données non seulement contre". Same meaning. |
| §100 | Date format "(année, mois, jour, heure, minute)" becomes "(année-mois-jour-heure-minute)" |
| §170 | "fonction" becomes "fonctionnalité" |
| §180 | "et non pas seulement le Z de caisse" becomes "et pas seulement sur le Z de caisse" |
| all others in §§10–260 | Identical |

Result for the **preamble and III:**
- §1 and §§270–320 change to name both proof routes.
- §275 (the certificate-only transition with the 2025–2026 dates) is deleted.
- §§360–390 (the attestation section) come back.
- §§400–410 add the attestation to the sanctions text.
- §§330, 340 and 420 are identical.

Consequence (D9): the LNE Rev 1.8 Exigences footnote S02 paragraphs in I and II. Those paragraphs
are unchanged in substance, so the note uses the Exigences as requirement text. Two LNE footnotes
point outside S02: Ex. 1 footnotes BOI-CF-COM-10-80-20160803, and Ex. 18 footnotes "point 210",
which is a reserved number "(210)" in both editions. Neither was diffed. The note does not carry
over LNE's certificate-only framing.

## 4. Legal-text fallback (research item 2)

1. **Légifrance, once more, with a browser User-Agent:** HTTP 403 for both CGI art. 286 (S13) and
   LF 2026 art. 125 (S14). The body is a Cloudflare challenge page.
2. **Assemblée nationale adopted text:** TA n° 227 (S07), "Texte définitif" of 2 February 2026,
   opened. Article 125 reads: "Le code général des impôts est ainsi modifié : 1° Le 3° bis du I de
   l’article 286 est complété par les mots : « ou par une attestation individuelle de l’éditeur,
   conforme à un modèle fixé par l’administration » ; 2° Au premier alinéa de l’article 1770
   duodecies, après le mot : « production », sont insérés les mots : « de l’attestation ou »."
3. **Conseil constitutionnel, décision n° 2026-901 DC (S08):** opened. Article 1 strikes, among
   others, "l'article 124". Paragraph 271 says that article 124 "modifie la dénomination de
   certaines conventions relatives à l'échange de renseignements en matière fiscale". That matches
   article 124 in S07 (the tax-convention amendments). Article 125 is not named in the decision.
4. **Sénat adopted text:** not tried. S07 already gave the definitive text.

Consequences:
- The **amending text** was read, and the constitutional check does not strike it.
- The **consolidated** CGI art. 286 I 3° bis was **not** read. The full wording that art. 125
  completes is known only through BOFiP S01 §1 and §270.
- **Inference:** S07's numbering equals the promulgated law's numbering for art. 125, because BOFiP
  S03 cites "art. 125" of loi n° 2026-103. The JORF text was not read.

## 5. D13 existence check (CGI art. 290 quater)

S16 exists and is in force (only version listed, from 12/09/2012). Its §1 says that operators of
venues with an entry price must issue a ticket to each spectator, or record and keep entry data in
a computer system, before entry. The rules are in CGI annex IV art. 50 sexies B to 50 sexies I.
§10 says it applies to every venue where entry needs a paid price. Sporting events are excluded by
a remark in §1. Nothing more was analysed.

## 5a. Certifier accreditation check (fix round 2)

The note's first version cited BYCYB's own statement of its COFRAC accreditation (S10). The
implementer then tried COFRAC directly, once per URL:

- `https://tools.cofrac.fr/annexes/sect5/5-0696.pdf`: HTTP 200, a 4-page PDF (S17). Extracted with
  `pdftotext -layout`.
- `https://www.cofrac.fr/annexes/sect5/5-0696.pdf`: HTTP 404.

S17 is the "Attestation d'accréditation N° 5-0696", Rev. 1, for BYCYB (legal identifier 942788845,
the same number as on S09's cover). It covers "NF EN ISO/IEC 17065:2012", product certification,
valid from 18/02/2026 until 31/05/2030. The technical annex lists, under Code de la consommation
L433-3 and following, the product "Système de caisse" with the programme "Référentiel de
certification des systèmes de caisse". The annex names no version. The programme title differs
from S09's title ("systèmes d’encaissement"). The note treats S09 as that programme by inference.

## 5b. LNE addendum (auditor's check, not the implementer's)

The decision audit of fix round 1 opened the LNE addendum
`https://bycyb.com/wp-content/uploads/2025/06/Addendum-referentiel-caisse-Rev1.7.pdf`. The auditor
reports that it covers Rev. 1.7 and the transfer of the referential to BYCYB, not a Rev. 1.8
technical requirement. The implementer did not fetch or read it. It has no hash in
`sources.json`.

## 6. How a reviewer re-checks a quote

1. Find the source id next to the quote in the note, and get its URL and sha256 from
   `sources.json`.
2. Re-fetch it: `curl -sS -L -A 'Mozilla/5.0 (X11; Linux x86_64)' -o f '<url>'`.
3. Compare: `sha256sum f`. A different hash on a BOFiP or AN page can come from page furniture
   (menus, scripts). It does not by itself mean the text changed. On a mismatch, extract the text
   and grep for the quote.
4. Extract the text. For HTML: remove tags and unescape entities. For PDF: `pdftotext`.
5. Normalise white space, then grep the quote. BOFiP, the AN definitive text (S07) and the LNE
   referential use the typographic apostrophe ’. The AN question page (S06) and the Conseil
   constitutionnel page (S08) use the straight apostrophe '. The note keeps each source's form, so
   a plain `grep -F` on the extracted, white-space-normalised text matches.
6. For a quote with "…", grep each side of the cut on its own.

The implementer checked all 110 double-quoted strings in the note this way on 2026-10-13. Every
French quote matched its source exactly, after white-space normalisation only. The other quoted
strings are English. They come from repository files (ADR-003, ADR-048, `keyring.go`,
`prd-v1.md` as of `8683c6fc`, `docs/conventions/continuous-delivery.md`), from the ticket's COS text, or are the note's own glosses. Each repository quote was
checked against the file at `8683c6fc`.

## 7. Code evidence

Every `file:line` citation in the note was opened at `8683c6fc` in the TKT-374 worktree. The
absence claims come from `git grep` over `services/` and `web/` at that commit:
- no VAT or TVA concept;
- no SIRET, establishment or register identifier;
- no receipt or invoice number;
- no fiscal closure or perpetual total.

An absence claim holds only for that commit.
