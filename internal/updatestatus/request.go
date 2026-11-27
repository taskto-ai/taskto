package updatestatus

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const RequestFile = "update-now"

const ExpireAfter = 3 * time.Hour

var (
	ErrNoDir   = errors.New("updatestatus: no request directory")
	ErrPending = errors.New("updatestatus: an update is already requested")
)

func Pending(dir string, now time.Time) (since time.Time, pending bool, err error) {
	if dir == "" {
		return time.Time{}, false, nil
	}
	info, err := os.Lstat(filepath.Join(dir, RequestFile))
	if errors.Is(err, fs.ErrNotExist) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	if now.Sub(info.ModTime()) > ExpireAfter {
		return time.Time{}, false, nil
	}
	return info.ModTime().UTC(), true, nil
}

type Prepared struct {
	dir, tmp string
	now      time.Time
}

func Prepare(dir string, now time.Time) (*Prepared, error) {
	if dir == "" {
		return nil, ErrNoDir
	}
	final := filepath.Join(dir, RequestFile)
	if _, pending, err := Pending(dir, now); err != nil {
		return nil, err
	} else if pending {
		return nil, ErrPending
	}
	if info, err := os.Lstat(final); err == nil && now.Sub(info.ModTime()) > ExpireAfter {
		_ = os.Remove(final)
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(dir, ".request-*")
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
		return nil, ErrNoDir
	}
	if err != nil {
		return nil, fmt.Errorf("updatestatus: %w", err)
	}
	_, err = fmt.Fprintf(tmp, "%s %s\n", now.UTC().Format(time.RFC3339), hex.EncodeToString(nonce[:]))
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
		return nil, fmt.Errorf("updatestatus: %w", err)
	}
	return &Prepared{dir: dir, tmp: tmp.Name(), now: now}, nil
}

func (p *Prepared) Publish() error {
	defer os.Remove(p.tmp)
	if err := os.Link(p.tmp, filepath.Join(p.dir, RequestFile)); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return ErrPending
		}
		return fmt.Errorf("updatestatus: %w", err)
	}
	sweep(p.dir, p.now)
	return nil
}

func (p *Prepared) Discard() { _ = os.Remove(p.tmp) }

func Request(dir string, now time.Time) error {
	p, err := Prepare(dir, now)
	if err != nil {
		return err
	}
	return p.Publish()
}

func sweep(dir string, now time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), ".request-") {
			continue
		}
		if info, err := e.Info(); err == nil && now.Sub(info.ModTime()) > time.Hour {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
