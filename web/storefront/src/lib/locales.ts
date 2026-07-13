// The live locale set mirrors the catalog's (SupportedLocales in
// services/catalog/internal/api): data, not schema (TKT-36).
export const LOCALES = ['en', 'fr'] as const;
export type Locale = (typeof LOCALES)[number];
export const DEFAULT_LOCALE: Locale = 'en';

export function isLocale(value: string): value is Locale {
  return (LOCALES as readonly string[]).includes(value);
}

export const LOCALE_LABELS: Record<Locale, string> = {
  en: 'English',
  fr: 'Français',
};

export const UI_STRINGS: Record<Locale, Record<string, string>> = {
  en: {
    events: 'Events',
    from: 'From',
    tickets: 'Tickets',
    venue: 'Venue',
    noEvents: 'No events on sale right now.',
    backToEvents: 'Back to events',
    myTickets: 'My tickets',
    ticket: 'Ticket',
    ticketQrAlt: 'Ticket QR code',
    ticketHistory: 'Ticket history',
    ticketDeliveryNotice: 'Your tickets are ready. Keep this page available for entry.',
  },
  fr: {
    events: 'Événements',
    from: 'À partir de',
    tickets: 'Billets',
    venue: 'Salle',
    noEvents: 'Aucun événement en vente pour le moment.',
    backToEvents: 'Retour aux événements',
    myTickets: 'Mes billets',
    ticket: 'Billet',
    ticketQrAlt: 'Code QR du billet',
    ticketHistory: 'Historique du billet',
    ticketDeliveryNotice: 'Vos billets sont prêts. Conservez cette page pour votre entrée.',
  },
};
