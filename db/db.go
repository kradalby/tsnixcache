// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

// Package db persists watcher discoveries and retry state.
package db

//go:generate sqlc generate -f ../sqlc.yaml
//go:generate gofumpt -w ../gen/dbsqlc
//go:generate goimports -w -local github.com/kradalby/tsnixcache ../gen/dbsqlc

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	"github.com/juanfont/headscale/hscontrol/db/sqliteconfig"
	"github.com/tailscale/squibble"
	"golang.org/x/sys/unix"
	_ "modernc.org/sqlite" // SQLite driver registration.

	"github.com/kradalby/tsnixcache/gen/dbsqlc"
)

//go:embed schema.sql
var currentSchema string

var schema = &squibble.Schema{
	Current:      currentSchema,
	IgnoreTables: []string{"_litestream_seq", "_litestream_lock"},
}

// ErrBusy means another watcher owns this state database.
var ErrBusy = errors.New("db: watcher state is already in use")

var (
	errPrivateDirectory = errors.New("db: state directory must have mode 0700")
	errEmptyState       = errors.New("db: existing state is empty or uninitialized")
)

// DB owns a private, single-writer watcher database.
type DB struct {
	*dbsqlc.Queries

	// Created distinguishes a new baseline from damaged existing state.
	Created bool

	sql  *sql.DB
	lock *os.File
}

// Open acquires ownership and applies known schema migrations. Unknown schemas
// and corrupt state are errors; neither may become a fresh discovery baseline.
func Open(ctx context.Context, path string) (_ *DB, retErr error) {
	path, err := preparePath(path)
	if err != nil {
		return nil, err
	}

	// #nosec G304 G703 -- configured path.
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("db: open ownership lock: %w", err)
	}
	defer func() {
		if retErr != nil {
			_ = lock.Close()
		}
	}()

	err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB) // #nosec G115 -- OS descriptors fit int.
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return nil, ErrBusy
	}

	if err != nil {
		return nil, fmt.Errorf("db: acquire ownership: %w", err)
	}

	created, err := createState(path)
	if err != nil {
		return nil, err
	}

	// sqliteconfig appends URI parameters without escaping the filesystem path.
	config := sqliteconfig.Default((&url.URL{Path: path}).EscapedPath())
	config.Synchronous = sqliteconfig.SynchronousFull

	uri, err := config.ToURL()
	if err != nil {
		return nil, fmt.Errorf("db: configure state: %w", err)
	}

	sqlDB, err := sql.Open("sqlite", uri)
	if err != nil {
		return nil, fmt.Errorf("db: open state: %w", err)
	}

	sqlDB.SetMaxOpenConns(1)

	defer func() {
		if retErr != nil {
			_ = sqlDB.Close()
		}
	}()

	if !created {
		var tables int

		err = sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_schema WHERE name = 'checkpoint'`).Scan(&tables)
		if err != nil {
			return nil, fmt.Errorf("db: inspect existing state: %w", err)
		}

		if tables == 0 {
			return nil, errEmptyState
		}
	}

	err = schema.Apply(ctx, sqlDB)
	if err != nil {
		return nil, fmt.Errorf("db: apply state schema: %w", err)
	}

	return &DB{Queries: dbsqlc.New(sqlDB), Created: created, sql: sqlDB, lock: lock}, nil
}

// Close releases ownership only after SQLite has closed its connections.
func (d *DB) Close() error { return errors.Join(d.sql.Close(), d.lock.Close()) }

// Transaction commits a bounded group of queue and checkpoint updates together.
func (d *DB) Transaction(ctx context.Context, update func(*dbsqlc.Queries) error) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("db: begin state update: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is a no-op.

	err = update(d.WithTx(tx))
	if err != nil {
		return fmt.Errorf("db: update state: %w", err)
	}

	err = tx.Commit()
	if err != nil {
		return fmt.Errorf("db: commit state: %w", err)
	}

	return nil
}

// Maintain incrementally reclaims retired queue pages without loading the backlog.
func (d *DB) Maintain(ctx context.Context) error {
	_, err := d.sql.ExecContext(ctx, `PRAGMA incremental_vacuum(256)`)
	if err != nil {
		return fmt.Errorf("db: reclaim state pages: %w", err)
	}

	return nil
}

func preparePath(path string) (string, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("db: absolute path: %w", err)
	}

	err = os.MkdirAll(filepath.Dir(path), 0o700)
	if err != nil {
		return "", fmt.Errorf("db: state directory: %w", err)
	}

	info, err := os.Stat(filepath.Dir(path))
	if err != nil {
		return "", fmt.Errorf("db: inspect state directory: %w", err)
	}

	if info.Mode().Perm()&0o077 != 0 {
		return "", errPrivateDirectory
	}

	return path, nil
}

func createState(path string) (bool, error) {
	// SQLite otherwise creates files using the process umask.
	// #nosec G304 G703 -- configured path.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR|unix.O_NOFOLLOW, 0o600)

	created := err == nil
	if errors.Is(err, os.ErrExist) {
		f, err = os.OpenFile(path, os.O_RDWR|unix.O_NOFOLLOW, 0o600) // #nosec G304 G703 -- configured path.
	}

	if err != nil {
		return false, fmt.Errorf("db: create state: %w", err)
	}

	info, err := f.Stat()
	if err != nil || !created && info.Size() == 0 {
		_ = f.Close()

		return false, errors.Join(errEmptyState, err)
	}

	err = f.Chmod(0o600)

	closeErr := f.Close()

	err = errors.Join(err, closeErr)
	if err != nil {
		return false, fmt.Errorf("db: secure state: %w", err)
	}

	return created, nil
}
