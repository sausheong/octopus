package router

import (
	"crypto/sha256"
	"sync"
	"time"
)

const (
	defaultWorkflowAffinityTTL      = time.Hour
	defaultWorkflowAffinityCapacity = 4096
)

type workflowAffinityEntry struct {
	model     string
	expiresAt time.Time
	updatedAt time.Time
}

// WorkflowAffinityStore keeps a preferred model for independently cached
// conversations that belong to the same explicitly identified workflow. Its
// keys are hashes of workflow IDs, and it deliberately contains no session or
// prompt-cache state.
type WorkflowAffinityStore struct {
	mu       sync.Mutex
	entries  map[[sha256.Size]byte]workflowAffinityEntry
	ttl      time.Duration
	capacity int
	now      func() time.Time
}

func NewWorkflowAffinityStore(ttl time.Duration, capacity int) *WorkflowAffinityStore {
	if ttl <= 0 {
		ttl = defaultWorkflowAffinityTTL
	}
	if capacity <= 0 {
		capacity = defaultWorkflowAffinityCapacity
	}
	return &WorkflowAffinityStore{
		entries:  make(map[[sha256.Size]byte]workflowAffinityEntry),
		ttl:      ttl,
		capacity: capacity,
		now:      time.Now,
	}
}

// Remember records a model only after a caller has successfully completed a
// request. Empty workflow IDs are ignored so affinity is never inferred.
func (s *WorkflowAffinityStore) Remember(workflowID, model string) {
	if s == nil || workflowID == "" || model == "" {
		return
	}
	now := s.now()
	key := sha256.Sum256([]byte(workflowID))
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeExpiredLocked(now)
	if _, exists := s.entries[key]; !exists && len(s.entries) >= s.capacity {
		s.evictOldestLocked()
	}
	s.entries[key] = workflowAffinityEntry{model: model, expiresAt: now.Add(s.ttl), updatedAt: now}
}

// Preferred returns workflow-level placement preference only. The caller must
// still check request eligibility and must use the conversation's own session
// identity for cache accounting.
func (s *WorkflowAffinityStore) Preferred(workflowID string) (string, bool) {
	if s == nil || workflowID == "" {
		return "", false
	}
	now := s.now()
	key := sha256.Sum256([]byte(workflowID))
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[key]
	if !ok {
		return "", false
	}
	if !entry.expiresAt.After(now) {
		delete(s.entries, key)
		return "", false
	}
	return entry.model, true
}

func (s *WorkflowAffinityStore) removeExpiredLocked(now time.Time) {
	for key, entry := range s.entries {
		if !entry.expiresAt.After(now) {
			delete(s.entries, key)
		}
	}
}

func (s *WorkflowAffinityStore) evictOldestLocked() {
	var oldestKey [sha256.Size]byte
	var oldestTime time.Time
	found := false
	for key, entry := range s.entries {
		if !found || entry.updatedAt.Before(oldestTime) {
			oldestKey, oldestTime, found = key, entry.updatedAt, true
		}
	}
	if found {
		delete(s.entries, oldestKey)
	}
}
