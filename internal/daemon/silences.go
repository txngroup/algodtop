package daemon

import (
	"encoding/json"
	"errors"
	"os"
	"sort"
	"sync"
	"time"

	"algod.top/internal/fsx"
	"algod.top/internal/model"
	"algod.top/internal/store"
)

// silences persists alert silences in state/silences.json.
type silences struct {
	mu      sync.Mutex
	path    string
	list    []model.Silence
	started map[string]bool
}

func openSilences(path string) *silences {
	s := &silences{path: path, started: map[string]bool{}}
	if b, err := os.ReadFile(path); err == nil {
		json.Unmarshal(b, &s.list)
	}
	now := time.Now().UnixNano()
	for _, x := range s.list {
		if x.Start <= now && now < x.End {
			s.started[x.ID] = true
		}
	}
	return s
}

func (s *silences) saveLocked() error {
	b, err := json.MarshalIndent(s.list, "", "  ")
	if err != nil {
		return err
	}
	return fsx.WriteAtomic(s.path, b, 0o600)
}

func (s *silences) all() []model.Silence {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]model.Silence(nil), s.list...)
	sort.Slice(out, func(i, j int) bool { return out[i].Start > out[j].Start })
	return out
}

func (s *silences) add(x model.Silence) (model.Silence, error) {
	if x.End <= x.Start {
		return x, errors.New("silence must end after it starts")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if x.ID == "" {
		x.ID = store.NewID()
	}
	if x.Created == 0 {
		x.Created = time.Now().UnixNano()
	}
	s.list = append(s.list, x)
	return x, s.saveLocked()
}

func (s *silences) remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, x := range s.list {
		if x.ID == id {
			// Ending a silence early keeps it in the history.
			now := time.Now().UnixNano()
			if x.End > now {
				s.list[i].End = now
			}
			return s.saveLocked()
		}
	}
	return store.ErrNotFound
}

func (s *silences) active(t int64, node, rule string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, x := range s.list {
		if x.Active(t, node, rule) {
			return true
		}
	}
	return false
}

// transitions returns silences that started or ended since the last call,
// for the silence started and ended events. Old silences are pruned after
// 90 days.
func (s *silences) transitions(now int64) (started, ended []model.Silence) {
	s.mu.Lock()
	defer s.mu.Unlock()
	keep := s.list[:0]
	changed := false
	for _, x := range s.list {
		on := x.Start <= now && now < x.End
		if on && !s.started[x.ID] {
			s.started[x.ID] = true
			started = append(started, x)
		} else if !on && s.started[x.ID] {
			delete(s.started, x.ID)
			ended = append(ended, x)
		}
		if x.End < now-int64(90*24*time.Hour) {
			changed = true
			continue
		}
		keep = append(keep, x)
	}
	s.list = keep
	if changed {
		s.saveLocked()
	}
	return started, ended
}
