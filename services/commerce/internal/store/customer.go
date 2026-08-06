package store

// Customer accounts (TKT-220 / US-A1). See ADR-049 for why buyer identity lives
// in commerce rather than catalog, the gateway, or a sixth service.
//
// Commerce owns orders and buyer_pii, so the buyer principal lives beside them.
// That is deliberately NOT ADR-042's argument transplanted: staff went to catalog
// because catalog owns organizers and staff administer an organizer. A customer
// administers nothing.
//
// This file owns credential storage and verification ONLY. Nothing here attaches
// an account to an order — that is TKT-221, and buyer_pii is untouched by design
// (it is an order-time snapshot rewritten on every checkout, not an identity).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/crypto/bcrypt"
)

var (
	// ErrCustomerCredentialsInvalid is the ONLY authentication failure this
	// package reports. An unknown address and a wrong password are the same
	// error by construction, so no caller can accidentally branch them apart
	// and disclose which accounts exist.
	ErrCustomerCredentialsInvalid = errors.New("invalid customer credentials")

	// ErrCustomerEmailTaken reports a registration collision. Registration is
	// create-only: an existing account is never silently overwritten, so
	// re-registering an address cannot reset a live account's password.
	ErrCustomerEmailTaken = errors.New("customer email already registered")

	// ErrCustomerPasswordUnusable reports a password bcrypt cannot faithfully store.
	ErrCustomerPasswordUnusable = errors.New("customer password unusable")

	// ErrCustomerInvalidInput reports an argument the schema would reject anyway,
	// caught before the KDF so a typo costs no work factor.
	ErrCustomerInvalidInput = errors.New("invalid customer account input")

	// errCustomerNotFound is internal: it never leaves this package, because
	// "no such account" is precisely the fact the authentication path must not
	// report.
	errCustomerNotFound = errors.New("customer account not found")
)

// customerBcryptCost is the KDF work factor. Both the real and the dummy hash are
// pinned to it: a dummy of a different cost would restore the timing signal this
// package exists to remove.
const customerBcryptCost = bcrypt.DefaultCost // 10

// customerDummyPassword / customerDummyHash are the fixed comparison target for an
// unknown address. The hash is a LITERAL, generated once out of band and pasted
// here — never produced by hashCustomerPassword at init. A fixture derived from
// the code under test encodes the property it is meant to prove: it would track a
// regression in the cost constant instead of failing on it. customer_test.go
// asserts its cost independently.
const (
	customerDummyPassword = "tkt-220 dummy comparison password"
	customerDummyHash     = "$2a$10$V7KJBgmYOoxGdCbOKKZ9g.vsuzs3YTUR58dphisvvKkuW/M7dNHBi"
)

// compareCustomerHashAndPassword is the KDF seam. Production always uses bcrypt;
// tests swap it to COUNT comparisons, which is the only way to prove the
// unknown-account path does not short-circuit past the KDF. Asserting equal status
// codes cannot prove it — the two paths can return identical bytes and still
// differ by an order of magnitude in wall-clock.
var compareCustomerHashAndPassword = bcrypt.CompareHashAndPassword

// swapCompareCustomerHashAndPassword installs a comparison function and returns a
// restore func. Test-only helper; it lives here rather than in _test.go so the
// seam and its single legitimate use are read together.
func swapCompareCustomerHashAndPassword(fn func(hashed, password []byte) error) func() {
	prev := compareCustomerHashAndPassword
	compareCustomerHashAndPassword = fn
	return func() { compareCustomerHashAndPassword = prev }
}

// CustomerAccount is a buyer principal. It carries no password material: nothing
// above this package ever sees a hash.
type CustomerAccount struct {
	ID uuid.UUID
	// Email is the buyer's ORIGINAL spelling, for display. Every lookup goes
	// through customerEmailKey instead.
	Email string
}

// customerCredential is the internal join of a principal and its stored hash. It
// never leaves this package.
type customerCredential struct {
	Account      CustomerAccount
	PasswordHash string
}

// customerLookup is the seam between credential verification (pure, testable
// without Postgres) and the query that resolves an address.
type customerLookup interface {
	lookupCustomerCredential(ctx context.Context, emailKey string) (customerCredential, error)
}

// customerEmailKey is the normalized lookup form. Global, not organizer-scoped: a
// customer buys across organizers, so "which organizer are you signing in to?" has
// no answer at the storefront's sign-in form.
//
// Lower-cases **ASCII only**, deliberately, and not strings.ToLower.
//
// The database CHECK recomputes this key from `email` so a row cannot display one
// address while reserving another's (ai-review pass 1). That makes Go and
// Postgres two implementations of one function, and they have to agree for every
// input the contract admits — otherwise a legitimate registration is refused by a
// constraint. `strings.ToLower` is full-Unicode; Postgres's `lower()` is
// COLLATION-DEPENDENT, and this deployment pins no collation. On a Turkish-locale
// database `lower('I')` is a dotless ı where Go produces 'i', so an address with a
// capital I would be refused (ai-review pass 2 [medium]).
//
// ASCII-only folding is the same function on both sides regardless of server
// locale — `lower(x COLLATE "C")` touches exactly A-Z. The cost is that two
// addresses differing only in the case of a non-ASCII character are two accounts.
// That is a real limitation and the honest one to take: the alternative is a
// constraint whose behaviour depends on a locale nobody has written down.
// The trim is ASCII-only for the same reason as the fold, and it is NOT
// strings.TrimSpace. TrimSpace strips Unicode whitespace; Postgres's btrim with
// this character set strips exactly these six bytes. A direct writer inserting an
// address padded with U+2003 EM SPACE would otherwise satisfy the CHECK with a
// key that still carries those bytes, while every application lookup trims them
// and asks for a different key — an account nobody can reach (ai-review pass 3).
//
// The application path never sees whitespace here anyway: RegisterCustomer trims
// with TrimSpace before storing, which is the buyer-friendly behaviour and is
// applied to `email` and `email_key` alike, so the two normalizers agree whatever
// arrives.
//
// A byte walk is safe on UTF-8 by construction: every byte of a multibyte
// sequence has the high bit set, so none can fall in A-Z or match an ASCII
// whitespace byte, and none can be modified here.
const asciiWhitespace = " \t\n\r\v\f"

func customerEmailKey(email string) string {
	out := []byte(strings.Trim(email, asciiWhitespace))
	for i := 0; i < len(out); i++ {
		if out[i] >= 'A' && out[i] <= 'Z' {
			out[i] += 'a' - 'A'
		}
	}
	return string(out)
}

// hashCustomerPassword produces the stored credential. bcrypt salts internally, so
// two registrations of the same password yield different hashes.
func hashCustomerPassword(password string) (string, error) {
	if password == "" {
		return "", fmt.Errorf("%w: empty", ErrCustomerPasswordUnusable)
	}
	if len(password) > bcryptMaxPasswordBytes {
		return "", fmt.Errorf("%w: longer than %d bytes, which bcrypt would silently truncate",
			ErrCustomerPasswordUnusable, bcryptMaxPasswordBytes)
	}
	h, err := bcrypt.GenerateFromPassword([]byte(password), customerBcryptCost)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrCustomerPasswordUnusable, err)
	}
	return string(h), nil
}

// bcryptMaxPasswordBytes is bcrypt's hard input bound. Past it the algorithm
// silently ignores the tail, so a 200-byte password and its 72-byte prefix would
// authenticate each other.
const bcryptMaxPasswordBytes = 72

// authenticateCustomer verifies a credential in constant shape: exactly one KDF
// comparison whether or not the address resolves, against a hash of the same cost
// either way. The unknown-address branch compares against customerDummyHash and
// then discards the result — even a caller who submits customerDummyPassword is
// refused, because `found` gates the answer, not the comparison's verdict.
//
// "Constant shape" means MASKED, not identical, and the difference is worth being
// precise about (ai-review, TKT-220 [medium]): a hit takes a successful Scan and a
// miss takes sql.ErrNoRows, so the two paths do differ by one indexed single-row
// lookup. What the KDF buys is that the difference sits underneath a bcrypt
// comparison two to three orders of magnitude larger. That defends against
// someone timing responses over a network; it does not defend against someone
// measuring the database, and ADR-049 §3 says so rather than implying otherwise.
func authenticateCustomer(ctx context.Context, s customerLookup, email, password string) (CustomerAccount, error) {
	// Refuse an over-long password BEFORE the lookup, and without touching the
	// database. bcrypt returns ErrPasswordTooLong *without doing the work* past
	// 72 bytes, so letting such an input through would skip the KDF — and the KDF
	// is the only thing masking the cost difference between a row that was found
	// and sql.ErrNoRows. The contract's maxLength cannot catch this: OpenAPI
	// counts CHARACTERS and bcrypt counts BYTES, so 72 multibyte characters pass
	// validation and are three times over the limit.
	//
	// Refusing here is uniform across addresses — every caller gets the same
	// immediate answer — so it introduces no oracle of its own.
	if len(password) > bcryptMaxPasswordBytes {
		return CustomerAccount{}, ErrCustomerCredentialsInvalid
	}

	cred, err := s.lookupCustomerCredential(ctx, customerEmailKey(email))
	found := err == nil
	switch {
	case found:
	case errors.Is(err, errCustomerNotFound):
		cred = customerCredential{PasswordHash: customerDummyHash}
	default:
		return CustomerAccount{}, fmt.Errorf("lookup customer credential: %w", err)
	}

	matched := compareCustomerHashAndPassword([]byte(cred.PasswordHash), []byte(password)) == nil
	if !found || !matched {
		return CustomerAccount{}, ErrCustomerCredentialsInvalid
	}
	return cred.Account, nil
}

// --- Postgres ---

// customerDB is the narrow database surface these two operations need. Commerce's
// store package takes *sql.DB per function rather than holding a Postgres value
// (store.go), and this follows that shape.
type customerDB struct{ db *sql.DB }

func (c customerDB) lookupCustomerCredential(ctx context.Context, emailKey string) (customerCredential, error) {
	var cred customerCredential
	err := c.db.QueryRowContext(ctx, `
		SELECT id, email, password_hash
		  FROM customer_accounts
		 WHERE email_key = $1`, emailKey).
		Scan(&cred.Account.ID, &cred.Account.Email, &cred.PasswordHash)
	if errors.Is(err, sql.ErrNoRows) {
		return customerCredential{}, errCustomerNotFound
	}
	if err != nil {
		return customerCredential{}, fmt.Errorf("lookup customer account: %w", err)
	}
	return cred, nil
}

// AuthenticateCustomer resolves a buyer principal from an address and password.
func AuthenticateCustomer(ctx context.Context, db *sql.DB, email, password string) (CustomerAccount, error) {
	return authenticateCustomer(ctx, customerDB{db}, email, password)
}

// RegisterCustomer creates one account. Create-only: a colliding address is
// ErrCustomerEmailTaken, never an overwrite.
//
// There is no pre-check for an existing address, deliberately. A SELECT followed
// by an INSERT is a race two concurrent registrations both win; the UNIQUE
// constraint is the authority, and its violation is the answer.
func RegisterCustomer(ctx context.Context, db *sql.DB, email, password string) (CustomerAccount, error) {
	display := strings.TrimSpace(email)
	if display == "" {
		return CustomerAccount{}, fmt.Errorf("%w: empty email", ErrCustomerInvalidInput)
	}
	hash, err := hashCustomerPassword(password)
	if err != nil {
		return CustomerAccount{}, err
	}
	account := CustomerAccount{Email: display}
	err = db.QueryRowContext(ctx, `
		INSERT INTO customer_accounts (email, email_key, password_hash)
		VALUES ($1, $2, $3) RETURNING id`,
		display, customerEmailKey(display), hash).Scan(&account.ID)
	switch {
	case isCustomerUniqueViolation(err):
		return CustomerAccount{}, ErrCustomerEmailTaken
	case err != nil:
		return CustomerAccount{}, fmt.Errorf("insert customer account: %w", err)
	}
	return account, nil
}

func isCustomerUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.SQLState() == "23505"
}

// --- the wallet read (TKT-222 / US-A3) ---

// WalletOrder is one completed purchase, as the wallet lists it.
//
// Money stays integer minor units + ISO currency (ADR-001); nothing here divides
// or formats. GuestOrderRef is the bearer credential the ticket page takes
// (ADR-012) — it is in this struct because the wallet's whole job is to link
// there, and it must never reach a log.
type WalletOrder struct {
	OrderID       uuid.UUID
	GuestOrderRef uuid.UUID
	CreatedAt     time.Time
	SlotID        uuid.UUID
	Quantity      int32
	TotalAmount   int64
	Currency      string
}

// WalletCursor is the keyset position between pages.
//
// (created_at, id), because created_at alone is not unique — two purchases in the
// same millisecond would make a page boundary ambiguous, and the row that lost the
// tie would be skipped. `id` breaks it deterministically.
type WalletCursor struct {
	CreatedAt time.Time
	OrderID   uuid.UUID
}

// walletPageQuery is a const so the ADR-019 scan-scope proof can EXPLAIN the exact
// statement production executes, rather than a lookalike.
//
// The keyset predicate is the row-value form `(created_at, id) < ($2, $3)`, which
// Postgres can drive straight off the index — writing it as
// `created_at < $2 OR (created_at = $2 AND id < $3)` is the same rows and a worse
// plan.
//
// $2/$3 are always bound: the first page passes a sentinel far in the future
// rather than switching to a second SQL string, because two statements mean the
// plan proof covers one of them.
const walletPageQuery = `
	SELECT o.id, o.guest_order_ref, o.created_at, r.slot_id, r.quantity, r.total_amount, r.currency
	  FROM orders o
	  JOIN reservations r ON r.id = o.reservation_id
	 WHERE o.customer_id = $1
	   AND o.status = 'completed'
	   AND (o.created_at, o.id) < ($2, $3)
	 ORDER BY o.created_at DESC, o.id DESC
	 LIMIT $4`

// WalletPageLimit bounds one page. A wallet is read by a person; a limit an order
// of magnitude larger would only ever serve a scraper.
const WalletPageLimit = 20

// CustomerOrders returns one page of a customer's completed orders, newest first.
//
// `customer` comes from the verified assertion and nowhere else — see the handler.
// This function has no opinion about who is allowed to ask; it answers for the id
// it is given, which is why the authorization check must not live here.
//
// Returns the page and the cursor for the next one, or a zero cursor when the page
// is the last. It fetches limit+1 to know that without a second count query, and
// a count would be a second scan of the same index for information the extra row
// already carries.
func CustomerOrders(ctx context.Context, db *sql.DB, customer uuid.UUID, after WalletCursor, limit int) ([]WalletOrder, WalletCursor, error) {
	if limit <= 0 || limit > WalletPageLimit {
		limit = WalletPageLimit
	}
	// The first page's sentinel: a timestamp no order can have. Not time.Now(),
	// which would race an order completed during the request and silently drop it
	// from its own buyer's first page.
	if after.CreatedAt.IsZero() {
		after = WalletCursor{CreatedAt: time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC), OrderID: uuid.Max}
	}

	rows, err := db.QueryContext(ctx, walletPageQuery, customer, after.CreatedAt, after.OrderID, limit+1)
	if err != nil {
		return nil, WalletCursor{}, fmt.Errorf("read customer orders: %w", err)
	}
	defer func() { _ = rows.Close() }()

	page := make([]WalletOrder, 0, limit)
	var next WalletCursor
	for rows.Next() {
		var o WalletOrder
		if err := rows.Scan(&o.OrderID, &o.GuestOrderRef, &o.CreatedAt, &o.SlotID, &o.Quantity, &o.TotalAmount, &o.Currency); err != nil {
			return nil, WalletCursor{}, fmt.Errorf("scan customer order: %w", err)
		}
		if len(page) == limit {
			// The limit+1'th row is not returned; it only proves there is more,
			// and the cursor is the LAST EMITTED row so the next page resumes
			// exactly where this one stopped.
			next = WalletCursor{CreatedAt: page[limit-1].CreatedAt, OrderID: page[limit-1].OrderID}
			break
		}
		page = append(page, o)
	}
	if err := rows.Err(); err != nil {
		return nil, WalletCursor{}, fmt.Errorf("iterate customer orders: %w", err)
	}
	return page, next, nil
}
