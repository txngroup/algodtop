package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"sort"
	"sync"

	"algod.top/internal/fsx"
	"algod.top/internal/model"
)

// annotations is a small JSON file rewritten atomically on every change.
type annotations struct {
	mu   sync.Mutex
	path string
	list []model.Annotation
}

func openAnnotations(path string) (*annotations, error) {
	a := &annotations{path: path}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return a, nil
	}
	if err != nil {
		return nil, err
	}
	if len(b) > 0 {
		if err := json.Unmarshal(b, &a.list); err != nil {
			return nil, err
		}
	}
	return a, nil
}

func (a *annotations) saveLocked() error {
	sort.Slice(a.list, func(i, j int) bool { return a.list[i].Start < a.list[j].Start })
	b, err := json.MarshalIndent(a.list, "", "  ")
	if err != nil {
		return err
	}
	return fsx.WriteAtomic(a.path, b, 0o600)
}

// NewID returns a random 12-character hex ID.
func NewID() string {
	var b [6]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// ErrNotFound is returned for a missing annotation.
var ErrNotFound = errors.New("store: not found")

// Annotations returns annotations overlapping [from, to) for a node. An
// empty node matches every node.
func (db *DB) Annotations(node string, from, to int64) []model.Annotation {
	a := db.ann
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []model.Annotation
	for _, x := range a.list {
		end := x.End
		if end == 0 {
			end = x.Start
		}
		if end < from || x.Start >= to {
			continue
		}
		if node != "" && x.Node != "" && x.Node != node {
			continue
		}
		out = append(out, x)
	}
	return out
}

// PutAnnotation adds or replaces an annotation.
func (db *DB) PutAnnotation(x model.Annotation) (model.Annotation, error) {
	a := db.ann
	a.mu.Lock()
	defer a.mu.Unlock()
	if x.ID == "" {
		x.ID = NewID()
		if x.Created == 0 {
			x.Created = db.now().UnixNano()
		}
		a.list = append(a.list, x)
		return x, a.saveLocked()
	}
	for i := range a.list {
		if a.list[i].ID == x.ID {
			if x.Created == 0 {
				x.Created = a.list[i].Created
			}
			a.list[i] = x
			return x, a.saveLocked()
		}
	}
	return x, ErrNotFound
}

// DeleteAnnotation removes an annotation.
func (db *DB) DeleteAnnotation(id string) error {
	a := db.ann
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := range a.list {
		if a.list[i].ID == id {
			a.list = append(a.list[:i], a.list[i+1:]...)
			return a.saveLocked()
		}
	}
	return ErrNotFound
}
