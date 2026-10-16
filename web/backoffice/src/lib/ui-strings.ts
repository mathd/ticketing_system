// The back office's UI strings (TKT-368, D1).
//
// The UI language is what the OPERATOR reads. It is not the event's content
// locale: an event name still carries every locale catalog requires (`name_en`,
// `name_fr`), whatever language the form around it is written in. Nothing here
// may decide which content field is submitted.
//
// Same pattern as the storefront's table (web/storefront/src/lib/locales.ts):
// English defines the keys, French must have every one of them, and the compiler
// checks it. Kept inside this app on purpose — the two apps build separately and
// share no package. No plural forms: this scope has none.

export const UI_LANGUAGES = ['en', 'fr'] as const;
export type UiLanguage = (typeof UI_LANGUAGES)[number];

/**
 * The closed set, from an untrusted value (a cookie, a form field). Exact match
 * only: `FR` or ` fr` is not a language this app serves, and anything that is not
 * one resolves to English rather than to an error.
 */
export function parseUiLanguage(raw: unknown): UiLanguage {
  return raw === 'fr' ? 'fr' : 'en';
}

export function otherLanguage(language: UiLanguage): UiLanguage {
  return language === 'fr' ? 'en' : 'fr';
}

/** Each language named in itself, for the switch button that offers it. */
export const LANGUAGE_NAMES = { en: 'English', fr: 'Français' } as const satisfies Record<UiLanguage, string>;

const ENGLISH_UI_STRINGS = {
  // Shell (Layout).
  backOffice: 'Back-office',
  languageSwitch: 'Interface language',

  // Venue list (index).
  venuesTitle: 'Venues — Back-office',
  venues: 'Venues',
  venueName: 'Name',
  gaCapacity: 'GA capacity',
  seatMaps: 'Seat maps',
  authorSeatMaps: 'Author seat maps',
  noVenues: 'No venues yet.',
  createEvent: 'Create an event',
  lookUpOrder: 'Look up an order',
  salesChannels: 'Sales channels',
  signOut: 'Sign out',

  // Event builder.
  createEventTitle: 'Create an event — Back-office',
  stepProgress: 'Step {step} of 4',
  eventStep: '1. The event',
  eventNameEnglish: 'Name (English)',
  eventNameFrench: 'Name (French)',
  createEventAction: 'Create event',
  dateStep: '2. When and where',
  venueListUnavailable: 'The venue list is unavailable, so this step cannot be completed yet.',
  venue: 'Venue',
  startsAt: 'Starts at',
  timezone: 'Timezone (IANA)',
  addDate: 'Add the date',
  priceStep: '3. The price',
  ticketNameEnglish: 'Ticket name (English)',
  ticketNameFrench: 'Ticket name (French)',
  amountMinorUnits: 'Amount in minor units',
  amountExample: '4550 (= €45.50)',
  currency: 'Currency',
  setPrice: 'Set the price',
  publishStep: '4. Publish',
  publishExplanation: 'This makes the date available to buyers.',
  publish: 'Publish',
  // Smoke pins "Publication accepted" (smoke/backoffice_event_builder_test.go).
  publicationAccepted: 'Publication accepted',
  publicationReported: 'Catalog reports this date as {status}.',
  publicationNotPublished: 'Catalog reports this date as {status}, not published.',
  backToVenues: 'Back to venues',
  // Catalog's slot status, for display only. The success test stays on the raw
  // value (`status === 'published'`), never on a label.
  statusDraft: 'draft',
  statusPublished: 'published',
  statusArchived: 'archived',
  statusUnknown: 'unknown',

  // Validation and errors.
  nameRequired: 'A name in {locale} is required.',
  amountTooLarge: 'That amount is too large to represent exactly.',
  amountInvalid: 'Enter the amount in minor units — digits only, no decimal point (€45.50 is 4550).',
  currencyInvalid: 'Use a three-letter uppercase ISO code, e.g. EUR.',
  catalogUnavailable: 'Something went wrong talking to the catalog. Try again.',
  saveAmbiguous:
    'Catalog may have saved this step, but its response could not be verified. ' +
    'Reload and reconcile the current event state before retrying. This form keeps the same idempotency key.',
  publishAmbiguous:
    'Catalog may have published this date, but its response could not be verified. ' +
    'Reload and reconcile the current event state before retrying.',
  // D1(d): the lead before catalog's own reason, chosen by status class.
  catalogRefusedStep: 'Catalog refused this step:',
  catalogRefusedChange: 'Catalog refused this change:',
} as const;

export type MessageKey = keyof typeof ENGLISH_UI_STRINGS;

/**
 * Every language's table has every key. English cannot declare this itself,
 * because MessageKey is derived from it; UI_STRINGS below checks it instead.
 */
type Messages = { [K in MessageKey]: string };

// French: vouvoiement, and a no-break space (U+00A0) before ":" and inside « »,
// as in the storefront's French strings.
const FRENCH_UI_STRINGS = {
  backOffice: 'Back-office',
  languageSwitch: 'Langue de l’interface',

  venuesTitle: 'Salles — Back-office',
  venues: 'Salles',
  venueName: 'Nom',
  gaCapacity: 'Jauge en placement libre',
  seatMaps: 'Plans de salle',
  authorSeatMaps: 'Gérer les plans de salle',
  noVenues: 'Aucune salle pour le moment.',
  createEvent: 'Créer un événement',
  lookUpOrder: 'Rechercher une commande',
  salesChannels: 'Canaux de vente',
  signOut: 'Se déconnecter',

  createEventTitle: 'Créer un événement — Back-office',
  stepProgress: 'Étape {step} sur 4',
  eventStep: '1. L’événement',
  eventNameEnglish: 'Nom (anglais)',
  eventNameFrench: 'Nom (français)',
  createEventAction: 'Créer l’événement',
  dateStep: '2. Date et lieu',
  venueListUnavailable:
    'La liste des salles est indisponible\u00a0: vous ne pouvez pas terminer cette étape pour le moment.',
  venue: 'Salle',
  startsAt: 'Début',
  timezone: 'Fuseau horaire (IANA)',
  addDate: 'Ajouter la date',
  priceStep: '3. Le prix',
  ticketNameEnglish: 'Nom du billet (anglais)',
  ticketNameFrench: 'Nom du billet (français)',
  amountMinorUnits: 'Montant en unités mineures',
  amountExample: '4550 (= 45,50\u00a0€)',
  currency: 'Devise',
  setPrice: 'Définir le prix',
  publishStep: '4. Publication',
  publishExplanation: 'La date devient alors disponible pour les acheteurs.',
  publish: 'Publier',
  publicationAccepted: 'Publication acceptée',
  publicationReported: 'Le catalogue indique que cette date est à l’état «\u00a0{status}\u00a0».',
  publicationNotPublished:
    'Le catalogue indique que cette date est à l’état «\u00a0{status}\u00a0», et non publiée.',
  backToVenues: 'Retour aux salles',
  statusDraft: 'brouillon',
  statusPublished: 'publiée',
  statusArchived: 'archivée',
  statusUnknown: 'inconnu',

  nameRequired: 'Le nom ({locale}) est obligatoire.',
  amountTooLarge: 'Ce montant est trop élevé pour être représenté exactement.',
  amountInvalid:
    'Saisissez le montant en unités mineures\u00a0: chiffres uniquement, sans séparateur décimal (45,50\u00a0€ s’écrit 4550).',
  currencyInvalid: 'Utilisez un code ISO de trois lettres majuscules, par exemple EUR.',
  catalogUnavailable: 'Une erreur est survenue lors de l’échange avec le catalogue. Réessayez.',
  saveAmbiguous:
    'Le catalogue a peut-être enregistré cette étape, mais sa réponse n’a pas pu être vérifiée. ' +
    'Rechargez la page et vérifiez l’état actuel de l’événement avant de réessayer. ' +
    'Ce formulaire conserve la même clé d’idempotence.',
  publishAmbiguous:
    'Le catalogue a peut-être publié cette date, mais sa réponse n’a pas pu être vérifiée. ' +
    'Rechargez la page et vérifiez l’état actuel de l’événement avant de réessayer.',
  catalogRefusedStep: 'Le catalogue a refusé cette étape\u00a0:',
  catalogRefusedChange: 'Le catalogue a refusé cette modification\u00a0:',
} satisfies Messages;

export const UI_STRINGS = {
  en: ENGLISH_UI_STRINGS,
  fr: FRENCH_UI_STRINGS,
} satisfies Record<UiLanguage, Messages>;

/** Replaces each `{name}` in a message; an unknown placeholder stays visible. */
export function fill(template: string, params: Readonly<Record<string, string | number>> = {}): string {
  return template.replace(/\{(\w+)\}/g, (whole, name: string) =>
    Object.hasOwn(params, name) ? String(params[name]) : whole);
}

/**
 * The text before and after one placeholder, so a page can put markup (a
 * `<strong>`) between them without building HTML from a string. Throws when the
 * placeholder is missing: a translation that dropped it would otherwise render
 * the value nowhere, silently.
 */
export function splitAt(template: string, name: string): [string, string] {
  const token = `{${name}}`;
  const at = template.indexOf(token);
  if (at < 0) throw new Error(`message has no ${token}: ${template}`);
  return [template.slice(0, at), template.slice(at + token.length)];
}
