// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package watch

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/kradalby/tsnixcache/db"
)

// Progress binds a session to its durable drain result.
type Progress struct {
	StateFile  string `json:"state_file"`
	Generation int64  `json:"generation"`
	Boundary   *int64 `json:"boundary"`
	Pending    int64  `json:"pending"`
	Expired    int64  `json:"expired"`
	Draining   bool   `json:"draining"`
}

func (p *poller) report(ctx context.Context) error {
	if p.w.Report == nil {
		return nil
	}

	cp, err := p.state.Checkpoint(ctx)
	if err != nil {
		return err
	}

	pending, err := p.state.CountPending(ctx)
	if err != nil {
		return err
	}

	progress := Progress{StateFile: p.statePath, Pending: pending, Expired: cp.Expired, Draining: p.draining}
	if p.draining {
		progress.Generation = cp.Generation
	}

	if p.draining && cp.Boundary.Valid {
		progress.Boundary = &cp.Boundary.Int64
	} else if p.draining && cp.LastGeneration == cp.Generation && cp.LastGeneration != 0 {
		progress.Boundary = &cp.LastBoundary.Int64
		progress.Expired = cp.LastExpired
	}

	return p.w.Report(progress)
}

// recoverDrain is called only while owning an abandoned session's lock.
func recoverDrain(ctx context.Context, progress Progress) (_ Progress, retErr error) {
	if progress.Generation == 0 || progress.StateFile == "" {
		return progress, ErrAbandoned
	}
	// Recovery must never create state or establish a fresh baseline.
	_, err := os.Stat(progress.StateFile)
	if err != nil {
		return progress, err
	}

	state, err := db.Open(ctx, progress.StateFile)
	if err != nil {
		return progress, err
	}

	defer func() { retErr = errors.Join(retErr, state.Close()) }()

	cp, err := state.Checkpoint(ctx)
	if err != nil {
		return progress, err
	}

	if cp.Generation != progress.Generation || cp.LastGeneration != progress.Generation || cp.Boundary.Valid {
		return progress, ErrAbandoned
	}

	progress.Pending, progress.Expired, progress.Boundary = 0, cp.LastExpired, &cp.LastBoundary.Int64
	if cp.LastExpired != 0 {
		return progress, fmt.Errorf("%w: %d expired", ErrIncomplete, cp.LastExpired)
	}

	return progress, nil
}
