package main

import (
	"encoding/json"
	"errors"
	"os"
	"sort"
	"sync"
)

// Record links an original message to its translated copy in the other channel.
// It lets replies point at the right message and lets edits/deletes follow the original.
type Record struct {
	SourceID      string   `json:"source_id"`
	SourceChannel string   `json:"source_channel"`
	TargetChannel string   `json:"target_channel"`
	TargetIDs     []string `json:"target_ids"` // several if a long translation was split
	WebhookID     string   `json:"webhook_id"`
	Prefix        string   `json:"prefix,omitempty"` // reply quote line, re-applied on edit
	Author        string   `json:"author"`
	SourceText    string   `json:"source_text"` // short snippets for reply quotes
	TargetText    string   `json:"target_text"`
	ContentHash   string   `json:"content_hash"`
	LocalID       string   `json:"local_id,omitempty"` // bot's local-language copy in the source channel // detects real text edits vs embed updates
}

// Store is a small persistent map of message pairs. A chat bridge only needs
// recent history, so the oldest records are dropped beyond maxRecords.
type Store struct {
	path       string
	maxRecords int

	mu       sync.Mutex
	bySource map[string]*Record
	byTarget map[string]string // translated message ID -> source ID
	byLocal  map[string]string // local copy ID -> source ID
	dirty    bool
}

func OpenStore(path string) (*Store, error) {
	s := &Store{path: path, maxRecords: 20000, bySource: map[string]*Record{}, byTarget: map[string]string{}, byLocal: map[string]string{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var recs []*Record
	if err := json.Unmarshal(data, &recs); err != nil {
		return nil, err
	}
	for _, r := range recs {
		s.index(r)
	}
	return s, nil
}

func (s *Store) index(r *Record) {
	s.bySource[r.SourceID] = r
	for _, id := range r.TargetIDs {
		s.byTarget[id] = r.SourceID
	}
	if r.LocalID != "" {
		s.byLocal[r.LocalID] = r.SourceID
	}
}

func (s *Store) unindex(r *Record) {
	delete(s.bySource, r.SourceID)
	for _, id := range r.TargetIDs {
		delete(s.byTarget, id)
	}
	delete(s.byLocal, r.LocalID)
}

// Put adds or replaces a record (a copy is stored).
func (s *Store) Put(r Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.bySource[r.SourceID]; ok {
		s.unindex(old)
	}
	s.index(&r)
	s.dirty = true
	if len(s.bySource) > s.maxRecords {
		s.prune()
	}
}

// BySource returns the record for an original message.
func (s *Store) BySource(id string) (Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.bySource[id]
	if !ok {
		return Record{}, false
	}
	return *r, true
}

// ByTarget returns the record whose translation includes the given message.
func (s *Store) ByTarget(id string) (Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	src, ok := s.byTarget[id]
	if !ok {
		return Record{}, false
	}
	return *s.bySource[src], true
}

// ByLocal returns the record whose local-language copy has the given ID.
func (s *Store) ByLocal(id string) (Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	src, ok := s.byLocal[id]
	if !ok {
		return Record{}, false
	}
	return *s.bySource[src], true
}

func (s *Store) Delete(sourceID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.bySource[sourceID]; ok {
		s.unindex(r)
		s.dirty = true
	}
}

// prune drops the oldest quarter. Discord IDs are snowflakes, so they sort by time.
func (s *Store) prune() {
	ids := make([]string, 0, len(s.bySource))
	for id := range s.bySource {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return snowflakeLess(ids[i], ids[j]) })
	for _, id := range ids[:len(ids)/4] {
		s.unindex(s.bySource[id])
	}
}

func snowflakeLess(a, b string) bool {
	if len(a) != len(b) {
		return len(a) < len(b)
	}
	return a < b
}

// Flush writes the map to disk if it changed (atomically, via a temp file).
func (s *Store) Flush() error {
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return nil
	}
	recs := make([]*Record, 0, len(s.bySource))
	for _, r := range s.bySource {
		recs = append(recs, r)
	}
	data, err := json.Marshal(recs)
	s.dirty = false
	s.mu.Unlock()
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
