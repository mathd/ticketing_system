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
    ticketVoided: 'No longer valid — do not present at the gate',
    ticketVoidedExchanged: 'Exchanged for a replacement ticket below',
    ticketVoidedRefunded: 'Refunded',
    festivalDays: 'Festival days',
    festivalPassesComingSoon: 'Festival passes go on sale soon.',
    seatFree: 'Available',
    seatSelected: 'Selected',
    seatTaken: 'Unavailable',
    seatRow: 'row',
    seat: 'seat',
    seatsSelected: '{n} of {max} seats selected',
    seatLimitReached: 'You can select at most {n} seats.',
    seatsNotOnSale: 'These seats are not on sale right now.',
    seatMapLoading: 'Loading the seat map…',
    seatSelectionUnavailable: 'Seat selection is temporarily unavailable. Please try again shortly.',
    seatsNoLongerAvailable: 'No longer available: {seats}',
    seatMapStale: 'Showing the last known seat availability — reconnecting.',
    reserveSeats: 'Reserve seats',
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
    ticketVoided: 'Plus valide — ne pas présenter à l’entrée',
    ticketVoidedExchanged: 'Échangé contre un billet de remplacement ci-dessous',
    ticketVoidedRefunded: 'Remboursé',
    festivalDays: 'Jours du festival',
    festivalPassesComingSoon: 'Les passeports festival seront bientôt en vente.',
    seatFree: 'Libre',
    seatSelected: 'Sélectionné',
    seatTaken: 'Indisponible',
    seatRow: 'rangée',
    seat: 'place',
    seatsSelected: '{n} place(s) sur {max} sélectionnée(s)',
    seatLimitReached: 'Vous pouvez sélectionner au plus {n} place(s).',
    seatsNotOnSale: 'Ces places ne sont pas en vente pour le moment.',
    seatMapLoading: 'Chargement du plan de salle…',
    seatSelectionUnavailable: 'La sélection des places est momentanément indisponible. Merci de réessayer sous peu.',
    seatsNoLongerAvailable: 'Plus disponible(s) : {seats}',
    seatMapStale: 'Affichage de la dernière disponibilité connue — reconnexion en cours.',
    reserveSeats: 'Réserver les places',
  },
};
