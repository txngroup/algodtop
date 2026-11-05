package store

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"algod.top/internal/fsx"
	"algod.top/internal/model"
)

// recLog is an append-only file of length-prefixed JSON records with a CRC32
// per record, split into one file per month or per day.
type recLog struct {
	dir   string
	daily bool
	mu    sync.Mutex
	name  string
	f     *os.File
	w     *bufio.Writer
	dirty bool
}

func newRecLog(dir string, daily bool) (*recLog, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &recLog{dir: dir, daily: daily}, nil
}

func (l *recLog) fileFor(t int64) string {
	u := time.Unix(0, t).UTC()
	if l.daily {
		return u.Format("2006-01-02") + ".log"
	}
	return u.Format("2006-01") + ".log"
}

func encodeRec(body []byte) []byte {
	out := make([]byte, 8, 8+len(body))
	binary.LittleEndian.PutUint32(out[:4], uint32(len(body)))
	binary.LittleEndian.PutUint32(out[4:], crc32.Checksum(body, crcTable))
	return append(out, body...)
}

// append writes one JSON record into the file for time t.
func (l *recLog) append(t int64, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	rec := encodeRec(body)
	l.mu.Lock()
	defer l.mu.Unlock()
	name := l.fileFor(t)
	if name != l.name {
		// Records for an older file (an agent catching up) go straight to
		// that file. The current file stays open for the common case.
		cur := l.fileFor(time.Now().UnixNano())
		if name != cur && l.f != nil {
			f, err := os.OpenFile(filepath.Join(l.dir, name), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
			if err != nil {
				return err
			}
			_, err = f.Write(rec)
			if serr := f.Sync(); err == nil {
				err = serr
			}
			f.Close()
			return err
		}
		if err := l.closeLocked(); err != nil {
			return err
		}
		f, err := os.OpenFile(filepath.Join(l.dir, name), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
		if err != nil {
			return err
		}
		l.f, l.w, l.name = f, bufio.NewWriter(f), name
	}
	_, err = l.w.Write(rec)
	l.dirty = true
	return err
}

func (l *recLog) sync() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.dirty || l.f == nil {
		return nil
	}
	l.dirty = false
	if err := l.w.Flush(); err != nil {
		return err
	}
	return l.f.Sync()
}

func (l *recLog) closeLocked() error {
	if l.f == nil {
		return nil
	}
	err := l.w.Flush()
	if serr := l.f.Sync(); err == nil {
		err = serr
	}
	if cerr := l.f.Close(); err == nil {
		err = cerr
	}
	l.f, l.w, l.name = nil, nil, ""
	return err
}

func (l *recLog) close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closeLocked()
}

// files returns the log files that may hold records in [from, to), oldest
// first.
func (l *recLog) files(from, to int64) []string {
	ents, err := os.ReadDir(l.dir)
	if err != nil {
		return nil
	}
	lo, hi := l.fileFor(from), l.fileFor(to)
	var out []string
	for _, e := range ents {
		n := e.Name()
		if !strings.HasSuffix(n, ".log") {
			continue
		}
		if n >= lo && n <= hi {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// scan calls fn with each record body in files overlapping [from, to).
// fn returns false to stop.
func (l *recLog) scan(from, to int64, fn func([]byte) bool) error {
	l.mu.Lock()
	if l.w != nil {
		l.w.Flush()
	}
	l.mu.Unlock()
	for _, name := range l.files(from, to) {
		f, err := fsx.OpenShared(filepath.Join(l.dir, name))
		if err != nil {
			continue
		}
		r := bufio.NewReaderSize(f, 64*1024)
		var hdr [8]byte
		for {
			if _, err := io.ReadFull(r, hdr[:]); err != nil {
				break
			}
			n := binary.LittleEndian.Uint32(hdr[:4])
			if n > 16<<20 {
				break
			}
			body := make([]byte, n)
			if _, err := io.ReadFull(r, body); err != nil {
				break
			}
			if crc32.Checksum(body, crcTable) != binary.LittleEndian.Uint32(hdr[4:]) {
				continue
			}
			if !fn(body) {
				f.Close()
				return nil
			}
		}
		f.Close()
	}
	return nil
}

// removeBefore deletes whole files that end before t.
func (l *recLog) removeBefore(t int64) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	ents, err := os.ReadDir(l.dir)
	if err != nil {
		return nil
	}
	var removed []string
	for _, e := range ents {
		n := e.Name()
		if !strings.HasSuffix(n, ".log") || n == l.name {
			continue
		}
		layout := "2006-01"
		if l.daily {
			layout = "2006-01-02"
		}
		start, err := time.Parse(layout, strings.TrimSuffix(n, ".log"))
		if err != nil {
			continue
		}
		var end time.Time
		if l.daily {
			end = start.AddDate(0, 0, 1)
		} else {
			end = start.AddDate(0, 1, 0)
		}
		if end.UnixNano() <= t {
			if os.Remove(filepath.Join(l.dir, n)) == nil {
				removed = append(removed, n)
			}
		}
	}
	return removed
}

// EventFilter selects events.
type EventFilter struct {
	Node  string
	Kinds []string
	From  int64
	To    int64
	Limit int
}

func (f EventFilter) match(e *model.Event) bool {
	if e.Time < f.From || e.Time >= f.To {
		return false
	}
	if f.Node != "" && e.Node != f.Node && e.Node != "" {
		return false
	}
	if len(f.Kinds) > 0 {
		ok := false
		for _, k := range f.Kinds {
			if k == e.Kind || (strings.HasSuffix(k, "*") && strings.HasPrefix(e.Kind, strings.TrimSuffix(k, "*"))) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

// scanFile calls fn for each valid record of one file.
func (l *recLog) scanFile(path string, fn func([]byte)) {
	f, err := fsx.OpenShared(path)
	if err != nil {
		return
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 64*1024)
	var hdr [8]byte
	for {
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return
		}
		n := binary.LittleEndian.Uint32(hdr[:4])
		if n > 16<<20 {
			return
		}
		body := make([]byte, n)
		if _, err := io.ReadFull(r, body); err != nil {
			return
		}
		if crc32.Checksum(body, crcTable) == binary.LittleEndian.Uint32(hdr[4:]) {
			fn(body)
		}
	}
}
