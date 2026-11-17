package agentloop

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// Proposal is a validated overlay edit waiting for a human to confirm it. BaseSHA is the
// blob SHA the edit was made against, so the PR update fails instead of overwriting a
// change someone made in the meantime.
type Proposal struct {
	ID, Requester, Channel      string
	Env, Service, Path, BaseSHA string
	OldContent, NewContent      string
	Title, Rationale, PRBody    string
	Created                     time.Time
}

// Store holds proposals between the fix run and the confirm button. It is in memory: a
// restart drops pending proposals, and the user runs the fix again.
type Store struct {
	mu    sync.Mutex
	ttl   time.Duration
	now   func() time.Time
	items map[string]Proposal
}

// NewStore keeps proposals for ttl. now is the clock; nil means time.Now.
func NewStore(ttl time.Duration, now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{ttl: ttl, now: now, items: map[string]Proposal{}}
}

// Put stores p under a new random ID and returns it. The ID is in the button's value,
// which anyone in the channel can see and replay, so it must not be guessable.
func (s *Store) Put(p Proposal) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	// Proposals nobody confirms would otherwise stay forever.
	for id, q := range s.items {
		if s.expired(q, now) {
			delete(s.items, id)
		}
	}
	id := newProposalID()
	for _, taken := s.items[id]; taken; _, taken = s.items[id] {
		id = newProposalID()
	}
	p.ID, p.Created = id, now
	s.items[id] = p
	return id
}

// Get returns the proposal without consuming it, for showing it again.
func (s *Store) Get(id string) (Proposal, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.live(id)
}

// Take returns the proposal and removes it in one step, so of two clicks on the confirm
// button only one gets the proposal and opens a PR.
func (s *Store) Take(id string) (Proposal, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.live(id)
	if ok {
		delete(s.items, id)
	}
	return p, ok
}

func (s *Store) Delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.items, id)
}

// live looks id up, dropping it when it has expired. s.mu must be held.
func (s *Store) live(id string) (Proposal, bool) {
	p, ok := s.items[id]
	if !ok {
		return Proposal{}, false
	}
	if s.expired(p, s.now()) {
		delete(s.items, id)
		return Proposal{}, false
	}
	return p, true
}

func (s *Store) expired(p Proposal, now time.Time) bool {
	return !now.Before(p.Created.Add(s.ttl))
}

// newProposalID is 64 random bits as 16 hex characters. crypto/rand.Read never fails.
func newProposalID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
