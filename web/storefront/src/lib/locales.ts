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
    seatsWouldStrand: 'This would leave {seats} on its own. Add it, or pick a different seat.',
    seatMapStale: 'Showing the last known seat availability — reconnecting.',
    reserveSeats: 'Reserve seats',
    signInAgain: 'Your sign-in has expired. Sign in again, then try once more — and check your tickets first in case this purchase already went through.',
    // The wallet (TKT-222).
    wallet: 'My purchases',
    walletEmpty: 'Nothing here yet. Anything you buy will show up on this page.',
    walletBrowseEvents: 'Browse events',
    walletUnavailable: 'Your purchases could not be loaded just now. Try again shortly.',
    walletViewTickets: 'View tickets',
    walletPurchasedOn: 'Bought',
    walletMore: 'Show more',
    walletUnnamedEvent: 'Purchase',
    // Customer accounts (TKT-220). An account is OPTIONAL everywhere: the copy
    // must never imply that buying requires one.
    account: 'Account',
    accountOptionalNotice:
      'An account is optional — you can always buy as a guest and keep your order reference.',
    signIn: 'Sign in',
    signOut: 'Sign out',
    register: 'Create an account',
    registerCta: 'No account yet? Create one.',
    signInCta: 'Already have an account? Sign in.',
    email: 'Email',
    password: 'Password',
    passwordMinimum: 'At least 8 characters.',
    signedInAs: 'Signed in as',
    // One message for every failure a caller can provoke. Distinguishing "no such
    // account" from "wrong password" here would undo, in the UI, exactly what the
    // store and the handler go to some trouble to prevent.
    credentialsInvalid: 'Those credentials are not valid.',
    accountTaken: 'An account already exists for that address.',
    // An outage is NOT a credential verdict: telling someone their correct
    // password is wrong sends them to reset it while the real fault goes
    // unreported.
    accountUnavailable: 'Accounts are temporarily unavailable. Try again shortly.',
    // Registration commits in commerce BEFORE the session is minted, so at
    // capacity the account genuinely EXISTS and only sign-in failed. Saying
    // "unavailable" there reads as "registration failed", and the buyer's retry
    // then answers "already exists" — which looks like a contradiction and is a
    // support call (ai-review pass 3).
    accountCreatedSignInLater:
      'Your account was created, but signing in is temporarily unavailable. Try signing in shortly.',
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
    seatsWouldStrand: 'Cela isolerait {seats}. Ajoutez cette place, ou choisissez-en une autre.',
    seatMapStale: 'Affichage de la dernière disponibilité connue — reconnexion en cours.',
    reserveSeats: 'Réserver les places',
    signInAgain: 'Votre connexion a expiré. Reconnectez-vous, puis réessayez — et vérifiez d’abord vos billets au cas où cet achat serait déjà passé.',
    wallet: 'Mes achats',
    walletEmpty: 'Rien pour l’instant. Vos achats apparaîtront sur cette page.',
    walletBrowseEvents: 'Voir les événements',
    walletUnavailable: 'Vos achats n’ont pas pu être chargés. Réessayez sous peu.',
    walletViewTickets: 'Voir les billets',
    walletPurchasedOn: 'Acheté le',
    walletMore: 'Afficher plus',
    walletUnnamedEvent: 'Achat',
    account: 'Compte',
    accountOptionalNotice:
      'Le compte est facultatif — vous pouvez toujours acheter en tant qu’invité et conserver votre référence de commande.',
    signIn: 'Se connecter',
    signOut: 'Se déconnecter',
    register: 'Créer un compte',
    registerCta: 'Pas encore de compte ? Créez-en un.',
    signInCta: 'Vous avez déjà un compte ? Connectez-vous.',
    email: 'Courriel',
    password: 'Mot de passe',
    passwordMinimum: 'Au moins 8 caractères.',
    signedInAs: 'Connecté en tant que',
    credentialsInvalid: 'Ces identifiants ne sont pas valides.',
    accountTaken: 'Un compte existe déjà pour cette adresse.',
    accountUnavailable: 'Les comptes sont temporairement indisponibles. Réessayez sous peu.',
    accountCreatedSignInLater:
      'Votre compte a été créé, mais la connexion est temporairement indisponible. Réessayez de vous connecter sous peu.',
  },
};
