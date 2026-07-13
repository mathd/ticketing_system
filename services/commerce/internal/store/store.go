package store

import (
	"context"
	"database/sql"
	"embed"
	"github.com/pressly/goose/v3"
	"io/fs"
)

//go:embed all:migrations
var migrationsFS embed.FS

func Migrate(ctx context.Context, db *sql.DB) error {
	f, e := fs.Sub(migrationsFS, "migrations")
	if e != nil {
		return e
	}
	p, e := goose.NewProvider(goose.DialectPostgres, db, f)
	if e != nil {
		return e
	}
	_, e = p.Up(ctx)
	return e
}
