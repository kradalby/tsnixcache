// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package watch

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
	"golang.org/x/sys/unix"
	"tailscale.com/atomicfile"
)

// Session errors distinguish an unverified exit from successful delivery.
var (
	ErrSessionBusy     = errors.New("watch: session already owned")
	ErrAbandoned       = errors.New("watch: session exited without a verified result")
	ErrSessionReplaced = errors.New("watch: session replaced")
	ErrSessionFailed   = errors.New("watch: session failed")
	ErrSessionNotReady = errors.New("watch: session ended before readiness was observed")
	errSessionStatus   = errors.New("watch: invalid session status")
	errSessionPrivate  = errors.New("watch: session directory must be private")
)

// Session lifecycle states are persisted as protocol values.
const (
	SessionStarting  = "starting"
	SessionReady     = "ready"
	SessionDraining  = "draining"
	SessionCompleted = "completed"
	SessionFailed    = "failed"
)

const (
	controlStop   = "/stop"
	controlStatus = "/status"
)

// Status is retained at <pid-file>.status.json after the watcher exits.
type Status struct {
	Version  int      `json:"version"`
	Session  string   `json:"session"`
	PID      int      `json:"pid"`
	State    string   `json:"state"`
	Socket   string   `json:"socket"`
	Progress Progress `json:"progress"`
	Error    string   `json:"error,omitempty"`
}

// Session owns status publication and an authenticated local control endpoint.
// Watch remains the sole owner of draining and its deadline.
type Session struct {
	closeOnce sync.Once
	closeErr  error
	mu        sync.Mutex
	status    Status
	path      string
	lock      *os.File
	dir       string
	server    *http.Server
	group     errgroup.Group
	cancel    context.CancelFunc
	stop      sync.Once
	writeErr  error
}

// NewSession acquires the session path before replacing any previous result.
// Call Close with the Watch result, including startup errors.
func NewSession(ctx context.Context, path string, cancel context.CancelFunc) (_ *Session, retErr error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}

	info, err := os.Stat(filepath.Dir(path))
	if err != nil {
		return nil, err
	}

	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return nil, errSessionPrivate
	}

	lock, err := sessionLock(path)
	if err != nil {
		return nil, err
	}

	token := make([]byte, 32)

	_, err = rand.Read(token)
	if err != nil {
		_ = lock.Close()

		return nil, err
	}

	s := &Session{
		path: path, lock: lock, cancel: cancel,
		status: Status{Version: 1, Session: hex.EncodeToString(token), PID: os.Getpid(), State: SessionStarting},
	}

	defer func() {
		if retErr != nil {
			retErr = s.Close(ctx, retErr)
		}
	}()
	// Keep the Unix address short even when the caller's workspace path is long.
	s.dir, err = os.MkdirTemp("/tmp", "tsnc-")
	if err != nil {
		return nil, err
	}

	s.status.Socket = filepath.Join(s.dir, "control")

	err = s.write()
	if err != nil {
		return nil, err
	}

	err = atomicfile.WriteFile(path, []byte(strconv.Itoa(s.status.PID)+"\n"), 0o600)
	if err != nil {
		return nil, err
	}

	listener, err := (&net.ListenConfig{}).Listen(ctx, "unix", s.status.Socket)
	if err != nil {
		return nil, err
	}

	s.server = &http.Server{
		Handler: http.HandlerFunc(s.handle), ReadHeaderTimeout: time.Second,
		ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second, IdleTimeout: 5 * time.Second, MaxHeaderBytes: 1024,
	}
	s.group.Go(func() error {
		err := s.server.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}

		s.stop.Do(s.cancel)

		return err
	})

	return s, nil
}

func sessionLock(path string) (*os.File, error) {
	// #nosec G304 G703 -- configured session path; stable lock inode.
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}

	err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB) // #nosec G115 -- OS descriptor fits int.
	if err != nil {
		_ = lock.Close()

		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, ErrSessionBusy
		}

		return nil, err
	}

	return lock, nil
}

// Ready cannot undo a stop request that arrived during startup.
func (s *Session) Ready() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.status.State == SessionStarting {
		s.status.State = SessionReady
	}

	return s.write()
}

// Report persists the drain binding before Watch attempts final uploads.
func (s *Session) Report(progress Progress) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.status.Progress = progress
	if progress.Draining {
		s.status.State = SessionDraining
	}

	return s.write()
}

// Close publishes the final result after joining control handlers. Status and
// the lock inode remain; the PID file and private socket are no longer live.
func (s *Session) Close(ctx context.Context, result error) error {
	s.closeOnce.Do(func() { s.closeErr = s.close(ctx, result) })

	return s.closeErr
}

func (s *Session) close(ctx context.Context, result error) error {
	if s.server != nil {
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()

		result = errors.Join(result, s.server.Shutdown(shutdownCtx), s.server.Close(), s.group.Wait())
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	result = errors.Join(result, s.writeErr)
	if s.dir != "" {
		result = errors.Join(result, os.RemoveAll(s.dir))
	}

	s.status.State, s.status.Error = SessionCompleted, ""
	if result != nil {
		s.status.State, s.status.Error = SessionFailed, result.Error()
		if len(s.status.Error) > 8000 {
			s.status.Error = s.status.Error[:8000]
		}
	}

	result = errors.Join(result, s.write())

	err := os.Remove(s.path)
	if !errors.Is(err, os.ErrNotExist) {
		result = errors.Join(result, err)
	}

	return errors.Join(result, s.lock.Close())
}

// ReadStatus reads a bounded, complete record; partial and legacy PID files
// provide no evidence of a successful drain.
func ReadStatus(path string) (Status, error) {
	var status Status

	file, err := os.Open(path + ".status.json") // #nosec G304 G703 -- configured session path.
	if err != nil {
		return status, err
	}
	defer file.Close()

	decoder := json.NewDecoder(io.LimitReader(file, 16*1024))
	decoder.DisallowUnknownFields()

	err = decoder.Decode(&status)
	if err != nil {
		return status, fmt.Errorf("%w: %w", errSessionStatus, err)
	}

	var extra any

	validStates := []string{SessionStarting, SessionReady, SessionDraining, SessionCompleted, SessionFailed}

	_, tokenErr := hex.DecodeString(status.Session)
	if decoder.Decode(&extra) != io.EOF || status.Version != 1 || status.PID <= 0 || len(status.Session) != 64 ||
		tokenErr != nil || !slices.Contains(validStates, status.State) {
		return status, errSessionStatus
	}

	return status, nil
}

// WaitOptions selects readiness, explicit stop, or passive result waiting.
type WaitOptions struct {
	Ready bool
	Stop  bool
	// Session optionally pins the expected identifier before the first read.
	Session string
	// StartupTimeout bounds missing/starting status; zero uses two minutes.
	StartupTimeout time.Duration
}

// WaitSession uses the control socket and durable result, never PID signals.
func WaitSession(ctx context.Context, path string, opts WaitOptions) (Status, error) {
	startup := time.NewTimer(orDuration(opts.StartupTimeout, 2*time.Minute))
	defer startup.Stop()

	startupC := startup.C

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	var status Status

	for {
		current, done, err := pollSession(ctx, path, opts)
		if err != nil && (!errors.Is(err, os.ErrNotExist) || current.Session != "") {
			return current, err
		}

		if done {
			return current, nil
		}

		if err == nil {
			status = current

			opts.Session = status.Session
			if status.State != SessionStarting {
				startup.Stop()

				startupC = nil
			}
		}

		select {
		case <-ctx.Done():
			return status, ctx.Err()
		case <-startupC:
			return status, fmt.Errorf("watch: waiting for session startup: %w", context.DeadlineExceeded)
		case <-ticker.C:
		}
	}
}

func pollSession(ctx context.Context, path string, opts WaitOptions) (Status, bool, error) {
	status, err := ReadStatus(path)
	if err != nil {
		return status, false, err
	}

	if opts.Session != "" && status.Session != opts.Session {
		return status, false, ErrSessionReplaced
	}

	if status.State == SessionCompleted || status.State == SessionFailed {
		return status, true, sessionResult(status, opts.Ready)
	}

	remote, err := sessionControl(ctx, status, opts.Stop)
	if errors.Is(err, ErrSessionReplaced) {
		return status, false, err
	}

	if err != nil {
		recovered, recoveryErr := recoverSession(ctx, path, status)
		if errors.Is(recoveryErr, ErrSessionBusy) {
			return status, false, nil
		}

		if recoveryErr != nil {
			return status, false, recoveryErr
		}

		return recovered, true, sessionResult(recovered, opts.Ready)
	}

	if remote.Session != status.Session {
		return remote, false, ErrSessionReplaced
	}

	if opts.Ready && remote.State == SessionDraining {
		return remote, false, ErrSessionNotReady
	}

	return remote, opts.Ready && remote.State == SessionReady, nil
}

func sessionResult(status Status, ready bool) error {
	if status.State == SessionFailed {
		return fmt.Errorf("%w: %s", ErrSessionFailed, status.Error)
	}

	if ready {
		return ErrSessionNotReady
	}

	return nil
}

func sessionControl(ctx context.Context, status Status, stop bool) (Status, error) {
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", status.Socket)
	}}
	defer transport.CloseIdleConnections()

	client := &http.Client{Transport: transport, Timeout: 2 * time.Second}

	method, path := http.MethodGet, controlStatus
	if stop {
		method, path = http.MethodPost, controlStop
	}

	request, err := http.NewRequestWithContext(ctx, method, "http://localhost"+path, nil)
	if err != nil {
		return status, err
	}

	request.Header.Set("Authorization", "Bearer "+status.Session)

	response, err := client.Do(request)
	if err != nil {
		return status, err
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return status, ErrSessionReplaced
	}

	var current Status

	err = json.NewDecoder(io.LimitReader(response.Body, 16*1024)).Decode(&current)

	return current, err
}

func recoverSession(ctx context.Context, path string, status Status) (Status, error) {
	err := ctx.Err()
	if err != nil {
		return status, err
	}

	lock, err := sessionLock(path)
	if err != nil {
		return status, err
	}
	defer lock.Close()

	current, err := ReadStatus(path)
	if err != nil {
		return status, err
	}

	if current.Session != status.Session {
		return current, ErrSessionReplaced
	}

	if current.State == SessionCompleted || current.State == SessionFailed {
		return current, nil
	}

	current.Progress, err = recoverDrain(ctx, current.Progress)
	if err != nil && !errors.Is(err, ErrAbandoned) && !errors.Is(err, ErrIncomplete) {
		return current, err
	}

	current.State = SessionCompleted
	if err != nil {
		current.State, current.Error = SessionFailed, errors.Join(ErrAbandoned, err).Error()
	}

	s := &Session{path: path, status: current}

	return current, s.write()
}

func (s *Session) write() error {
	data, err := json.Marshal(s.status)
	if err != nil {
		return err
	}

	err = atomicfile.WriteFile(s.path+".status.json", data, 0o600)
	if err != nil {
		return err
	}

	dir, err := os.Open(filepath.Dir(s.path)) // #nosec G304 G703 -- owned session directory.
	if err != nil {
		return err
	}

	return errors.Join(dir.Sync(), dir.Close())
}

func (s *Session) handle(w http.ResponseWriter, r *http.Request) {
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+s.status.Session)) != 1 {
		http.Error(w, "session mismatch", http.StatusConflict)

		return
	}

	if (r.URL.Path != controlStatus || r.Method != http.MethodGet) &&
		(r.URL.Path != controlStop || r.Method != http.MethodPost) {
		http.NotFound(w, r)

		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if r.URL.Path == controlStop && s.status.State != SessionCompleted && s.status.State != SessionFailed {
		s.stop.Do(func() {
			s.status.State = SessionDraining
			s.writeErr = s.write()
			s.cancel()
		})
	}

	w.Header().Set("Content-Type", "application/json")

	err := json.NewEncoder(w).Encode(s.status)
	if err != nil {
		slog.Debug("watch: control response", "err", err)
	}
}
