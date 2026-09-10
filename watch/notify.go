// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package watch

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/fsnotify/fsnotify"
)

var errNotificationClosed = errors.New("notification channel closed")

type notifications struct {
	retry   *time.Timer
	dir     string
	base    string
	info    os.FileInfo
	watcher *fsnotify.Watcher
	events  <-chan fsnotify.Event
	errors  <-chan error
	create  func() (*fsnotify.Watcher, error)
	next    time.Time
	backoff time.Duration
}

func newNotifications(w *Watcher) *notifications {
	create := w.notificationFactoryForTesting
	if create == nil {
		create = fsnotify.NewWatcher
	}

	retry := time.NewTimer(time.Hour)
	retry.Stop()

	return &notifications{retry: retry, dir: filepath.Dir(w.DBPath), base: filepath.Base(w.DBPath), create: create}
}

// SetNotificationFactoryForTesting injects unavailable or interrupted event sources.
func (w *Watcher) SetNotificationFactoryForTesting(create func() (*fsnotify.Watcher, error)) {
	w.notificationFactoryForTesting = create
}

func (n *notifications) close() {
	n.retry.Stop()

	if n.watcher != nil {
		_ = n.watcher.Close()
	}

	n.watcher, n.events, n.errors, n.info = nil, nil, nil, nil
}

func (n *notifications) failed(err error, now time.Time) {
	n.close()
	n.backoff = min(max(n.backoff*2, time.Second), time.Minute)
	n.next = now.Add(n.backoff)
	n.retry.Reset(n.backoff)
	slog.Warn("watch: notifications unavailable; polling continues", "err", err, "retryAfter", n.backoff)
}

func (n *notifications) ensure(now time.Time) {
	if now.Before(n.next) {
		return
	}

	info, err := os.Stat(n.dir)
	if err != nil {
		n.failed(err, now)

		return
	}

	if n.watcher != nil && os.SameFile(info, n.info) && slices.Contains(n.watcher.WatchList(), n.dir) {
		return
	}

	n.close()

	w, err := n.create()
	if err != nil {
		n.failed(err, now)

		return
	}

	n.watcher = w

	err = w.Add(n.dir)
	if err != nil {
		n.failed(err, now)

		return
	}

	n.events, n.errors, n.info = w.Events, w.Errors, info
	n.next, n.backoff = time.Time{}, 0
}

func (n *notifications) relevant(event fsnotify.Event) bool {
	// The database is authoritative; sidecars only accelerate the next poll.
	name := filepath.Base(event.Name)

	return slices.Contains([]string{n.base, n.base + "-wal", n.base + "-journal", n.base + "-shm"}, name) ||
		filepath.Clean(event.Name) == n.dir
}
