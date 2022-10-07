package sync

import (
	"fmt"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	sema "golang.org/x/sync/semaphore"
)

// OrderedItems is kindof like a queue, but removes the stricter set of requirements
// than the word queue would implies
type OrderedItems interface {
	pop() *item
	add(key Key, priority int32, creationTime time.Time)
	remove(key Key)
	items() []*item
}

type PrioritySemaphore struct {
	name         string
	limit        int
	pending      OrderedItems
	semaphore    *sema.Weighted
	lockHolder   map[string]bool
	lock         *sync.Mutex
	nextWorkflow NextWorkflow
	log          *log.Entry
	wrappedPeek  func(s *PrioritySemaphore, n int) []*item
	getWorkflow  GetWorkflow
}

var _ Semaphore = &PrioritySemaphore{}

func NewSemaphore(name string, limit int, nextWorkflow NextWorkflow, lockType string, orderedItems OrderedItems) *PrioritySemaphore {
	return &PrioritySemaphore{
		name:         name,
		limit:        limit,
		pending:      orderedItems,
		semaphore:    sema.NewWeighted(int64(limit)),
		lockHolder:   make(map[string]bool),
		lock:         &sync.Mutex{},
		nextWorkflow: nextWorkflow,
		log: log.WithFields(log.Fields{
			lockType: name,
		}),
	}
}

func (s *PrioritySemaphore) getName() string {
	return s.name
}

func (s *PrioritySemaphore) getLimit() int {
	return s.limit
}

func (s *PrioritySemaphore) getCurrentPending() []string {
	var keys []string
	for _, item := range s.pending.items() {
		keys = append(keys, item.key)
	}
	return keys
}

func (s *PrioritySemaphore) getCurrentHolders() []string {
	var keys []string
	for k := range s.lockHolder {
		keys = append(keys, k)
	}
	return keys
}

func (s *PrioritySemaphore) resize(n int) bool {
	s.lock.Lock()
	defer s.lock.Unlock()

	cur := len(s.lockHolder)
	// downward case, acquired n locks
	if cur > n {
		cur = n
	}

	semaphore := sema.NewWeighted(int64(n))
	status := semaphore.TryAcquire(int64(cur))
	if status {
		s.log.Infof("%s semaphore resized from %d to %d", s.name, cur, n)
		s.semaphore = semaphore
		s.limit = n
	}
	return status
}

func (s *PrioritySemaphore) release(key string) bool {
	s.lock.Lock()
	defer s.lock.Unlock()
	if _, ok := s.lockHolder[key]; ok {
		delete(s.lockHolder, key)
		// When semaphore resized downward
		// Remove the excess holders from map once the done.
		if len(s.lockHolder) >= s.limit {
			return true
		}

		s.semaphore.Release(1)
		availableLocks := s.limit - len(s.lockHolder)
		s.log.Infof("Lock has been released by %s. Available locks: %d", key, availableLocks)
		if len(s.pending.items()) > 0 {
			s.notifyWaiters()
		}
	}
	return true
}

// notifyWaiters enqueues the next N workflows who are waiting for the semaphore to the workqueue,
// where N is the availability of the semaphore. If semaphore is out of capacity, this does nothing.
func (s *PrioritySemaphore) notifyWaiters() {
	triggerCount := s.limit - len(s.lockHolder)
	if len(s.pending.items()) < triggerCount {
		triggerCount = len(s.pending.items())
	}
	items := s.wrappedPeek(s, triggerCount)
	for _, item := range items {
		wfKey := workflowKey(item)
		s.log.Debugf("Enqueue the workflow %s", wfKey)
		s.nextWorkflow(wfKey)
	}
}

// workflowKey formulates the proper workqueue key given a semaphore queue item
func workflowKey(i *item) string {
	parts := strings.Split(i.key, "/")
	if len(parts) == 3 {
		// the item is template semaphore (namespace/workflow-name/node-id) and so key must be
		// truncated to just: namespace/workflow-name
		return fmt.Sprintf("%s/%s", parts[0], parts[1])
	}
	return i.key
}

// addToQueue adds the holderkey into priority queue that maintains the priority order to acquire the lock.
func (s *PrioritySemaphore) addToQueue(holderKey string, priority int32, creationTime time.Time) {
	s.lock.Lock()
	defer s.lock.Unlock()

	if _, ok := s.lockHolder[holderKey]; ok {
		s.log.Debugf("Lock is already acquired by %s", holderKey)
		return
	}

	s.pending.add(holderKey, priority, creationTime)
	s.log.Debugf("Added into queue: %s", holderKey)
}

func (s *PrioritySemaphore) removeFromQueue(holderKey string) {
	s.lock.Lock()
	defer s.lock.Unlock()

	s.pending.remove(holderKey)
	s.log.Debugf("Removed from queue: %s", holderKey)
}

func (s *PrioritySemaphore) acquire(holderKey string) bool {
	if s.semaphore.TryAcquire(1) {
		s.lockHolder[holderKey] = true
		return true
	}
	return false
}

func isSameWorkflowNodeKeys(firstKey, secondKey string) bool {
	firstItems := strings.Split(firstKey, "/")
	secondItems := strings.Split(secondKey, "/")

	if len(firstItems) != len(secondItems) {
		return false
	}
	// compare workflow name
	return firstItems[1] == secondItems[1]
}

func (s *PrioritySemaphore) tryAcquire(holderKey string) (bool, string) {
	s.lock.Lock()
	defer s.lock.Unlock()

	if _, ok := s.lockHolder[holderKey]; ok {
		s.log.Debugf("%s is already holding a lock", holderKey)
		return true, ""
	}

	waitingMsg := fmt.Sprintf("Waiting for %s lock. Lock status: %d/%d", s.name, s.limit-len(s.lockHolder), s.limit)

	if len(s.pending.items()) > 0 {
		item := s.wrappedPeek(s, 1)[0]
		if holderKey != "" && !isSameWorkflowNodeKeys(holderKey, item.key) {
			// Enqueue the front workflow if lock is available
			if len(s.lockHolder) < s.limit {
				s.nextWorkflow(workflowKey(item))
			}
			return false, waitingMsg
		}
	}

	if s.acquire(holderKey) {
		s.pending.pop()
		s.log.Infof("%s acquired by %s. Lock availability: %d/%d", s.name, holderKey, s.limit-len(s.lockHolder), s.limit)
		s.notifyWaiters()
		return true, ""
	}
	s.log.Debugf("Current semaphore Holders. %v", s.lockHolder)
	return false, waitingMsg
}
