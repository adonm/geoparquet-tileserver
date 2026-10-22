// Package cache provides a small, persistent byte cache for proxy responses.
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Disk is a bounded content-addressed cache. Keys are hashed, so they can
// contain URLs, query strings, and filter expressions safely.
type Disk struct {
	root     string
	maxBytes int64
	mu       sync.Mutex
	bytes    int64
}

func Open(root string, maxMB int) (*Disk, error) {
	if root == "" || maxMB <= 0 {
		return nil, nil
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	d := &Disk{root: root, maxBytes: int64(maxMB) << 20}
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		if st, err := entry.Info(); err == nil {
			d.bytes += st.Size()
		}
		return nil
	})
	return d, nil
}

func (d *Disk) path(ns, key string) string {
	sum := sha256.Sum256([]byte(ns + "\x00" + key))
	name := hex.EncodeToString(sum[:])
	return filepath.Join(d.root, name[:2], name)
}

func (d *Disk) Get(ns, key string) ([]byte, bool) {
	if d == nil {
		return nil, false
	}
	p := d.path(ns, key)
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, false
	}
	now := time.Now()
	_ = os.Chtimes(p, now, now)
	return b, true
}

func (d *Disk) Put(ns, key string, b []byte) {
	if d == nil {
		return
	}
	p := d.path(ns, key)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return
	}
	tmp := fmt.Sprintf("%s.tmp-%d", p, time.Now().UnixNano())
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return
	}
	d.bytes += int64(len(b))
	if d.bytes > d.maxBytes {
		d.evictLocked()
	}
}

func (d *Disk) evictLocked() {
	type file struct {
		path  string
		size  int64
		mtime time.Time
	}
	var files []file
	_ = filepath.WalkDir(d.root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		st, err := entry.Info()
		if err != nil {
			return nil
		}
		if strings.Contains(filepath.Base(path), ".tmp-") {
			return nil
		}
		files = append(files, file{path: path, size: st.Size(), mtime: st.ModTime()})
		return nil
	})
	sort.Slice(files, func(i, j int) bool { return files[i].mtime.Before(files[j].mtime) })
	target := d.maxBytes * 9 / 10
	for _, f := range files {
		if d.bytes <= target {
			break
		}
		if err := os.Remove(f.path); err == nil {
			d.bytes -= f.size
		}
	}
}
