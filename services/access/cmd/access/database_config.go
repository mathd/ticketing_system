package main

import (
	"database/sql"
	"errors"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// Access database configuration (TKT-557, ADR-085 §8 ticket 0, D21).
//
// Access accepts ONE form: an explicit sslmode=disable in DATABASE_URL, and an
// effective pgx configuration with no TLS and no TLS fallback. The reason is
// the availability ledger, not encryption. With the pgx default (prefer), a
// restart (SQLSTATE 57P03) or a full server (53300) fails BOTH the TLS attempt
// and the plaintext attempt, and the TLS refusal is an untyped error. The
// strict leaf rule then answers 500, and after TKT-558 a routine restart would
// stop the gates. With explicit disable the same failures are typed, fully
// supported trees and answer 503. The supported Compose PostgreSQL has no TLS,
// so prefer already ends in plaintext there: this costs no encryption.
//
// This is a Compose-only restriction. A topology with TLS needs a new ADR and
// a new real-driver probe; this check makes that need explicit rather than
// letting the class ledger change in silence.

// errExplicitDisable is the one refusal. It never contains the DSN, which can
// carry a password.
var errExplicitDisable = errors.New("DATABASE_URL must set sslmode=disable explicitly and enable no TLS " +
	"(Access supports only the Compose topology without TLS: ADR-085, TKT-557 D21)")

// accessDatabaseConfig validates DATABASE_URL and returns the parsed config
// that the caller must open, unchanged. Two independent predicates: the LAST
// explicit sslmode key in the string is disable (an environment default such
// as PGSSLMODE cannot supply its presence), and the effective config has no
// TLS and no TLS fallback (a later ssl=true URL alias fails here).
func accessDatabaseConfig(dsn string) (*pgx.ConnConfig, error) {
	mode, ok, err := explicitSSLMode(dsn)
	if err != nil || !ok || mode != "disable" {
		return nil, errExplicitDisable
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		// Not wrapped: a parse error can quote the connection string.
		return nil, errExplicitDisable
	}
	if cfg.TLSConfig != nil {
		return nil, errExplicitDisable
	}
	for _, fallback := range cfg.Fallbacks {
		if fallback.TLSConfig != nil {
			return nil, errExplicitDisable
		}
	}
	return cfg, nil
}

// openAccessDB validates dsn and opens a pool on that same parsed config. It
// does not dial: a refused configuration never reaches the network.
func openAccessDB(dsn string) (*sql.DB, error) {
	cfg, err := accessDatabaseConfig(dsn)
	if err != nil {
		return nil, err
	}
	return stdlib.OpenDB(*cfg), nil
}

// explicitSSLMode returns the value of the last sslmode key written in dsn,
// in URL or keyword/value form, as pinned pgx reads it: the last occurrence
// wins. ok is false when the string has no sslmode key. A malformed string is
// an error.
func explicitSSLMode(dsn string) (mode string, ok bool, err error) {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		return urlSSLMode(dsn)
	}
	return keywordSSLMode(dsn)
}

// urlSSLMode reads every query pair in order. Keys and values are
// percent-decoded; "+" is not a space, as in libpq.
func urlSSLMode(dsn string) (string, bool, error) {
	_, query, found := strings.Cut(dsn, "?")
	if !found {
		return "", false, nil
	}
	if i := strings.IndexByte(query, '#'); i >= 0 {
		query = query[:i]
	}
	var mode string
	var ok bool
	for _, pair := range strings.Split(query, "&") {
		if pair == "" {
			continue
		}
		rawKey, rawValue, _ := strings.Cut(pair, "=")
		key, err := url.PathUnescape(rawKey)
		if err != nil {
			return "", false, err
		}
		if key != "sslmode" {
			continue
		}
		value, err := url.PathUnescape(rawValue)
		if err != nil {
			return "", false, err
		}
		mode, ok = value, true
	}
	return mode, ok, nil
}

// keywordSSLMode tokenizes key=value pairs as libpq does: a value is either
// single-quoted, or runs to the next whitespace; a backslash escapes the next
// character in both forms. The string is never split on spaces, so text
// inside a value cannot pose as a key.
func keywordSSLMode(dsn string) (string, bool, error) {
	const space = " \t\n\r\v\f"
	var mode string
	var ok bool
	s := strings.TrimLeft(dsn, space)
	for s != "" {
		eq := strings.IndexByte(s, '=')
		if eq < 0 {
			return "", false, errors.New("keyword DSN: missing '='")
		}
		key := strings.Trim(s[:eq], space)
		if key == "" || strings.ContainsAny(key, space) {
			return "", false, errors.New("keyword DSN: invalid key")
		}
		s = strings.TrimLeft(s[eq+1:], space)
		var value strings.Builder
		if strings.HasPrefix(s, "'") {
			i, closed := 1, false
			for i < len(s) {
				switch s[i] {
				case '\\':
					i++
					if i < len(s) {
						value.WriteByte(s[i])
					}
				case '\'':
					closed = true
				default:
					value.WriteByte(s[i])
				}
				i++
				if closed {
					break
				}
			}
			if !closed {
				return "", false, errors.New("keyword DSN: unterminated quoted value")
			}
			s = s[i:]
		} else {
			i := 0
			for i < len(s) && !strings.ContainsRune(space, rune(s[i])) {
				if s[i] == '\\' {
					i++
					if i >= len(s) {
						break
					}
				}
				value.WriteByte(s[i])
				i++
			}
			s = s[i:]
		}
		if key == "sslmode" {
			mode, ok = value.String(), true
		}
		s = strings.TrimLeft(s, space)
	}
	return mode, ok, nil
}
