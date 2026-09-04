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

const ENGLISH_UI_STRINGS = {
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
    // Claiming a past guest order (TKT-223).
    claimTitle: 'Add these tickets to your account',
    claimBody: 'You are signed in. Add this order to your account so you can find it again without this link.',
    claimAction: 'Add to my account',
    claimSignedOut: 'Sign in to add this order to your account.',
    claimRefused: 'This order could not be added to your account.',
    claimUnavailable: 'That could not be done just now. Try again shortly.',
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
    // Password recovery (TKT-226).
    forgotPassword: 'Forgot your password?',
    forgotPasswordCta: 'Forgotten your password?',
    forgotPasswordIntro:
      'Enter the address you registered with and we will send you a link to choose a new password.',
    sendResetLink: 'Send a reset link',
    // The ONE answer, for a known and an unknown address alike. It says what was
    // done, not what was found — "if that address has an account" is what keeps the
    // page from being the membership oracle the endpoint refuses to be. Wording it
    // as a promise ("we sent you an email") would be a lie for the unknown case.
    resetRequested:
      'If that address has an account, a reset link is on its way. The link works once and expires in an hour.',
    chooseNewPassword: 'Choose a new password',
    newPassword: 'New password',
    setNewPassword: 'Set the new password',
    // A dead link is not a credential verdict and not an outage. It sends the buyer
    // back to ask for another one, which is the only thing that helps them.
    resetLinkDead: 'That reset link is invalid or has expired. Ask for a new one.',
    resetDone: 'Your password has been changed. Sign in with it.',
    // Every other session for this customer ended, which is the point rather than a
    // side effect: a reset the owner performs must sign out whoever else was in.
    resetSignedOutEverywhere: 'Any other devices signed in to this account have been signed out.',
    // An outage is NOT a credential verdict: telling someone their correct
    // password is wrong sends them to reset it while the real fault goes
    // unreported.
    accountUnavailable: 'Accounts are temporarily unavailable. Try again shortly.',
    // A rate limit is NOT an outage and NOT a credential verdict (TKT-224,
    // ADR-051). Rendering it as `accountUnavailable` would tell a buyer to
    // escalate when all they have to do is wait, and rendering it as
    // `credentialsInvalid` would be false. It says nothing about whether the
    // address holds an account — the wording is identical either way, which is
    // the whole point of refusing before commerce looks.
    tooManyAttempts: 'Too many attempts. Wait a minute and try again.',
    // Registration commits before the session is minted. At session capacity the
    // account exists, so the message must distinguish creation from sign-in.
    accountCreatedSignInLater:
      'Your account was created, but signing in is temporarily unavailable. Try signing in shortly.',
    reserve: 'Reserve',
    pay: 'Pay',
    quantity: 'Quantity',
    heldFor: 'Held for',
    holdExpired: 'Hold expired',
    quantityUnavailable: 'Quantity unavailable',
    orderConfirmed: 'Order confirmed',
    viewMyTickets: 'View my tickets',
    paymentDeclined: 'Payment declined — try again',
    serviceUnavailable: 'Service unavailable',
    paymentChecking: 'Payment status is being checked',
    checkoutRetryShortly: 'This order is being finalised — try again in a moment.',
    nameLabel: 'Name',
    emailLabel: 'Email',
} as const;

export type MessageKey = keyof typeof ENGLISH_UI_STRINGS;

const FRENCH_UI_STRINGS = {
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
    claimTitle: 'Ajoutez ces billets à votre compte',
    claimBody: 'Vous êtes connecté. Ajoutez cette commande à votre compte pour la retrouver sans ce lien.',
    claimAction: 'Ajouter à mon compte',
    claimSignedOut: 'Connectez-vous pour ajouter cette commande à votre compte.',
    claimRefused: 'Cette commande n’a pas pu être ajoutée à votre compte.',
    claimUnavailable: 'Impossible pour le moment. Réessayez sous peu.',
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
    forgotPassword: 'Mot de passe oublié ?',
    forgotPasswordCta: 'Mot de passe oublié ?',
    forgotPasswordIntro:
      'Saisissez l’adresse utilisée à l’inscription et nous vous enverrons un lien pour choisir un nouveau mot de passe.',
    sendResetLink: 'Envoyer un lien',
    resetRequested:
      'Si cette adresse a un compte, un lien est en route. Il ne fonctionne qu’une fois et expire dans une heure.',
    chooseNewPassword: 'Choisissez un nouveau mot de passe',
    newPassword: 'Nouveau mot de passe',
    setNewPassword: 'Enregistrer le mot de passe',
    resetLinkDead: 'Ce lien est invalide ou a expiré. Demandez-en un nouveau.',
    resetDone: 'Votre mot de passe a été modifié. Connectez-vous avec celui-ci.',
    resetSignedOutEverywhere:
      'Les autres appareils connectés à ce compte ont été déconnectés.',
    accountUnavailable: 'Les comptes sont temporairement indisponibles. Réessayez sous peu.',
    tooManyAttempts: 'Trop de tentatives. Attendez une minute et réessayez.',
    accountCreatedSignInLater:
      'Votre compte a été créé, mais la connexion est temporairement indisponible. Réessayez de vous connecter sous peu.',
    reserve: 'Réserver',
    pay: 'Payer',
    quantity: 'Quantité',
    heldFor: 'Réservé pendant',
    holdExpired: 'Réservation expirée',
    quantityUnavailable: 'Quantité indisponible',
    orderConfirmed: 'Commande confirmée',
    viewMyTickets: 'Voir mes billets',
    paymentDeclined: 'Paiement refusé — réessayez',
    serviceUnavailable: 'Service indisponible',
    paymentChecking: 'Vérification du paiement en cours',
    checkoutRetryShortly: 'Cette commande est en cours de finalisation — réessayez dans un instant.',
    nameLabel: 'Nom',
    emailLabel: 'Courriel',
} satisfies Record<MessageKey, string>;

export const UI_STRINGS = {
  en: ENGLISH_UI_STRINGS,
  fr: FRENCH_UI_STRINGS,
} satisfies Record<Locale, Record<MessageKey, string>>;
