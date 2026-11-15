package identityadapter

import (
	"container/heap"
	"time"
)

type credentialExpiry[K comparable] struct {
	key K
	at  time.Time
}

// Entries contain identifiers and deadlines, never passwords or fingerprints.
// The owning map's mutex protects this queue. Deadlines are immutable; replay
// and dispatch-state updates must not append another entry or renew retention.
type credentialExpiryQueue[K comparable] []credentialExpiry[K]

func (q credentialExpiryQueue[K]) Len() int           { return len(q) }
func (q credentialExpiryQueue[K]) Less(i, j int) bool { return q[i].at.Before(q[j].at) }
func (q credentialExpiryQueue[K]) Swap(i, j int)      { q[i], q[j] = q[j], q[i] }
func (q *credentialExpiryQueue[K]) Push(value any) {
	*q = append(*q, value.(credentialExpiry[K]))
}
func (q *credentialExpiryQueue[K]) Pop() any {
	last := len(*q) - 1
	value := (*q)[last]
	(*q)[last] = credentialExpiry[K]{}
	*q = (*q)[:last]
	return value
}

func (q *credentialExpiryQueue[K]) add(key K, at time.Time) {
	heap.Push(q, credentialExpiry[K]{key: key, at: at})
}

// inclusive preserves the preparation map's <= boundary; vault references and
// operation bindings retain their existing strict-before cleanup boundary.
func (q *credentialExpiryQueue[K]) takeExpired(now time.Time, inclusive bool) (credentialExpiry[K], bool) {
	if len(*q) == 0 {
		return credentialExpiry[K]{}, false
	}
	at := (*q)[0].at
	if at.After(now) || !inclusive && at.Equal(now) {
		return credentialExpiry[K]{}, false
	}
	return heap.Pop(q).(credentialExpiry[K]), true
}
