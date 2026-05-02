package master

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"
)

// FileProvider resolves the master key from a file on disk. The file's mtime
// is checked on every Resolve call; when it changes the file is re-read and
// re-validated, providing hot-reload without a separate watcher goroutine.
type FileProvider struct {
	path string

	mu       sync.RWMutex
	cacheKey []byte
	cacheID  string
	cacheMt  time.Time
}

// NewFileProvider binds to a path; the file is not read until the first Resolve.
func NewFileProvider(path string) *FileProvider {
	return &FileProvider{path: path}
}

// Resolve stat()s the file, returns the cached key when mtime is unchanged, or
// re-reads and re-validates when it has moved forward.
func (p *FileProvider) Resolve(_ context.Context) ([]byte, string, error) {
	info, err := os.Stat(p.path)
	if err != nil {
		return nil, "", fmt.Errorf("stat master key file %q: %w", p.path, err)
	}
	mtime := info.ModTime()

	p.mu.RLock()
	if p.cacheKey != nil && mtime.Equal(p.cacheMt) {
		k, id := p.cacheKey, p.cacheID
		p.mu.RUnlock()
		return k, id, nil
	}
	p.mu.RUnlock()

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cacheKey != nil && mtime.Equal(p.cacheMt) {
		return p.cacheKey, p.cacheID, nil
	}

	raw, err := os.ReadFile(p.path)
	if err != nil {
		return nil, "", fmt.Errorf("read master key file %q: %w", p.path, err)
	}
	key, err := decodeHexKey(string(raw))
	if err != nil {
		return nil, "", err
	}
	id := os.Getenv(EnvMasterKeyID)
	if id == "" {
		id = DefaultFileKeyID
	}
	p.cacheKey = key
	p.cacheID = id
	p.cacheMt = mtime
	return key, id, nil
}
