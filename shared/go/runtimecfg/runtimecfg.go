// Package runtimecfg defines the bounded process resource policy shared by
// service and gateway entrypoints.
package runtimecfg

import (
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"
)

type HTTP struct {
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
}

func HTTPFromEnv() (HTTP, error) {
	readHeader, err := duration("HTTP_READ_HEADER_TIMEOUT", 5*time.Second)
	if err != nil {
		return HTTP{}, err
	}
	read, err := duration("HTTP_READ_TIMEOUT", 15*time.Second)
	if err != nil {
		return HTTP{}, err
	}
	write, err := duration("HTTP_WRITE_TIMEOUT", 30*time.Second)
	if err != nil {
		return HTTP{}, err
	}
	idle, err := duration("HTTP_IDLE_TIMEOUT", 60*time.Second)
	if err != nil {
		return HTTP{}, err
	}
	return HTTP{readHeader, read, write, idle}, nil
}

func (c HTTP) Apply(server *http.Server) {
	server.ReadHeaderTimeout = c.ReadHeaderTimeout
	server.ReadTimeout = c.ReadTimeout
	server.WriteTimeout = c.WriteTimeout
	server.IdleTimeout = c.IdleTimeout
}

type Database struct {
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
	ConnMaxIdleTime time.Duration
}

func DatabaseFromEnv() (Database, error) {
	open, err := integer("DB_MAX_OPEN_CONNS", 25)
	if err != nil {
		return Database{}, err
	}
	idle, err := integer("DB_MAX_IDLE_CONNS", 10)
	if err != nil {
		return Database{}, err
	}
	if idle > open {
		return Database{}, fmt.Errorf("DB_MAX_IDLE_CONNS must not exceed DB_MAX_OPEN_CONNS")
	}
	lifetime, err := duration("DB_CONN_MAX_LIFETIME", 30*time.Minute)
	if err != nil {
		return Database{}, err
	}
	idleTime, err := duration("DB_CONN_MAX_IDLE_TIME", 5*time.Minute)
	if err != nil {
		return Database{}, err
	}
	return Database{open, idle, lifetime, idleTime}, nil
}

func (c Database) Apply(db *sql.DB) {
	db.SetMaxOpenConns(c.MaxOpenConns)
	db.SetMaxIdleConns(c.MaxIdleConns)
	db.SetConnMaxLifetime(c.ConnMaxLifetime)
	db.SetConnMaxIdleTime(c.ConnMaxIdleTime)
}

// retiredInternalToken is the checked-in default this repo shipped before
// TKT-83. It is a public fingerprint, not an active secret: server mode
// refuses it forever so stale automation cannot keep authenticating with it.
const retiredInternalToken = "local-service-token"

// InternalTokenFromEnv validates the internal service credential at startup.
// Server entrypoints call it before touching any dependency so a
// misconfigured deployment fails fast instead of timing out. Errors never
// echo the supplied value.
func InternalTokenFromEnv() (string, error) {
	token := os.Getenv("INTERNAL_SERVICE_TOKEN")
	switch token {
	case "":
		return "", fmt.Errorf("INTERNAL_SERVICE_TOKEN required: no default is shipped, run `make up` once to generate a local credential")
	case retiredInternalToken:
		return "", fmt.Errorf("INTERNAL_SERVICE_TOKEN is the retired checked-in default: generate a real credential (`make up`)")
	}
	return token, nil
}

func duration(name string, fallback time.Duration) (time.Duration, error) {
	raw, ok := os.LookupEnv(name)
	if !ok {
		return fallback, nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", name)
	}
	return value, nil
}

func integer(name string, fallback int) (int, error) {
	raw, ok := os.LookupEnv(name)
	if !ok {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return value, nil
}
