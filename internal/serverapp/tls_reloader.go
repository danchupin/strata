package serverapp

import (
	"context"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// certReloader runs the fsnotify watch loop and the periodic
// reconciliation fallback for the US-003 hot-reload TLS path.
//
// Watch shape:
//   - Single-cert (certFile + keyFile): watch parent directories of both
//     paths for RENAME / CREATE / WRITE events. Kubelet's atomic-symlink
//     swap for Secret + ConfigMap mounts replaces the basename pointer
//     so fsnotify on the file itself misses the modify event — watching
//     the parent directory catches the rename. The file basename is
//     filtered in the handler.
//   - Cert dir: watch the directory itself for CREATE / RENAME / WRITE
//     events for any *.crt or *.key entry.
//
// Periodic reconciler (cfg.ReloadInterval, default 60s, range
// [10s, 1h], 0 disabled) re-stats every tracked path and rebuilds the
// snapshot when any fingerprint changes — absorbs fsnotify drops under
// load or on filesystems with weak event semantics.
type certReloader struct {
	store    *certStore
	logger   *slog.Logger
	certFile string
	keyFile  string
	certDir  string
	interval time.Duration
}

// run blocks until ctx is cancelled. Errors building a snapshot on a
// reload tick log WARN and keep the previous snapshot live — the
// gateway never falls back to plain HTTP and never serves an empty
// cert store.
func (r *certReloader) run(ctx context.Context) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		r.logger.Warn("tls reloader: fsnotify init failed — periodic reconciler only", "error", err.Error())
		r.runPeriodicOnly(ctx)
		return
	}
	defer watcher.Close()

	watched := map[string]bool{}
	addWatch := func(path string) {
		if path == "" || watched[path] {
			return
		}
		if err := watcher.Add(path); err != nil {
			r.logger.Warn("tls reloader: fsnotify add failed", "path", path, "error", err.Error())
			return
		}
		watched[path] = true
	}
	if r.certDir != "" {
		addWatch(r.certDir)
	}
	if r.certFile != "" {
		addWatch(filepath.Dir(r.certFile))
	}
	if r.keyFile != "" {
		addWatch(filepath.Dir(r.keyFile))
	}

	var tickerC <-chan time.Time
	if r.interval > 0 {
		t := time.NewTicker(r.interval)
		defer t.Stop()
		tickerC = t.C
	}

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-watcher.Events:
			if !ok {
				return
			}
			if r.shouldReact(ev) {
				r.reload("fsnotify:" + ev.Name)
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			r.logger.Warn("tls reloader: fsnotify error", "error", err.Error())
		case <-tickerC:
			r.reload("periodic")
		}
	}
}

func (r *certReloader) runPeriodicOnly(ctx context.Context) {
	if r.interval <= 0 {
		<-ctx.Done()
		return
	}
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.reload("periodic")
		}
	}
}

// shouldReact filters fsnotify events to the files we care about. CREATE
// + RENAME + WRITE all trigger; CHMOD is ignored.
func (r *certReloader) shouldReact(ev fsnotify.Event) bool {
	if ev.Op&(fsnotify.Create|fsnotify.Rename|fsnotify.Write|fsnotify.Remove) == 0 {
		return false
	}
	base := filepath.Base(ev.Name)
	if r.certDir != "" && filepath.Dir(ev.Name) == r.certDir {
		// Cert dir mode: any *.crt or *.key add/change.
		return hasCertOrKeySuffix(base)
	}
	if r.certFile != "" && filepath.Base(r.certFile) == base {
		return true
	}
	if r.keyFile != "" && filepath.Base(r.keyFile) == base {
		return true
	}
	// k8s Secret / ConfigMap atomic-swap layout: kubelet writes data
	// into a versioned ..YYYY_MM_DD_HH_MM_SS.ABCDEF directory and
	// rewires the ..data symlink. We watch the parent dir of the
	// projected file path; reacting to the ..data flip covers the
	// k8s case without us having to track the inner version dir.
	if base == "..data" {
		return true
	}
	return false
}

func hasCertOrKeySuffix(name string) bool {
	if len(name) >= 4 && name[len(name)-4:] == ".crt" {
		return true
	}
	if len(name) >= 4 && name[len(name)-4:] == ".key" {
		return true
	}
	return false
}

// reload rebuilds the snapshot from disk and swaps it in if it differs
// from the current one. A snapshot-build error leaves the existing
// pointer untouched.
func (r *certReloader) reload(reason string) {
	var next *certSnapshot
	var err error
	if r.certDir != "" {
		next, err = buildSnapshotFromDir(r.certDir)
	} else if r.certFile != "" {
		next, err = buildSnapshotFromSingle(r.certFile, r.keyFile)
	}
	if err != nil {
		r.logger.Warn("tls reloader: snapshot build failed — keeping previous", "reason", reason, "error", err.Error())
		return
	}
	if next == nil {
		return
	}
	current := r.store.load()
	if !snapshotChanged(current, next) {
		return
	}
	r.store.swap(next)
	r.logger.Info("tls reloader: cert snapshot swapped", "reason", reason, "pairs", len(next.pairs))
}

// snapshotChanged compares two snapshots by file-fingerprint set. A
// missing pair counts as changed; same-path same-fingerprint pairs are
// equal.
func snapshotChanged(prev, next *certSnapshot) bool {
	if prev == nil || next == nil {
		return true
	}
	if len(prev.pairs) != len(next.pairs) {
		return true
	}
	prevByPath := make(map[string]fileFingerprint, len(prev.pairs))
	for _, p := range prev.pairs {
		prevByPath[p.certPath] = p.fp
	}
	for _, p := range next.pairs {
		if fp, ok := prevByPath[p.certPath]; !ok || fp != p.fp {
			return true
		}
	}
	return false
}
