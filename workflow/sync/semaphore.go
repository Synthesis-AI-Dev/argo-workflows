package sync

import (
	"fmt"
	"strings"
	"sync"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v3/pkg/apis/workflow/v1alpha1"
	"github.com/aws/smithy-go/ptr"
	log "github.com/sirupsen/logrus"
	sema "golang.org/x/sync/semaphore"
)

type PrioritySemaphore struct {
	name         string
	limit        int
	pending      *priorityQueue
	semaphore    *sema.Weighted
	lockHolder   map[string]bool
	lock         *sync.Mutex
	nextWorkflow NextWorkflow
	log          *log.Entry
	getWorkflow  GetWorkflow
}

var _ Semaphore = &PrioritySemaphore{}

func NewSemaphore(name string, limit int, nextWorkflow NextWorkflow, lockType string, getWorkflow GetWorkflow) *PrioritySemaphore {
	return &PrioritySemaphore{
		name:         name,
		limit:        limit,
		pending:      &priorityQueue{itemByKey: make(map[string]*item)},
		semaphore:    sema.NewWeighted(int64(limit)),
		lockHolder:   make(map[string]bool),
		lock:         &sync.Mutex{},
		nextWorkflow: nextWorkflow,
		getWorkflow:  getWorkflow,
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
	for _, item := range s.pending.items {
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
		if s.pending.Len() > 0 {
			s.notifyWaiters()
		}
	}
	return true
}

// notifyWaiters enqueues the next N workflows who are waiting for the semaphore to the workqueue,
// where N is the availability of the semaphore. If semaphore is out of capacity, this does nothing.
func (s *PrioritySemaphore) notifyWaiters() {
	totalTriggers := s.limit - len(s.lockHolder)
	countTriggers := 0

	holding := s.getCurrentHolders()
	pending := s.getCurrentPending()

	for idx := 0; idx < s.pending.Len(); idx++ {
		item := s.pending.items[idx]
		shouldTrigger, err := s.checkStrategy(holding, pending, item.key)
		if err != nil {
			log.Errorf("got an error while checking strategy: %s", err)
		}
		if shouldTrigger {
			wfKey := workflowKey(item)
			s.log.Debugf("enqueue the workflow %s", wfKey)
			s.nextWorkflow(wfKey)
			countTriggers += 1
			if countTriggers == totalTriggers {
				break
			}

			// adjust pending and holding
			for i, v := range pending {
				if v == item.key {
					pending = append(pending[:i], pending[i+1:]...)
				}
			}
			holding = append(holding, item.key)
		}
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

func getRebalanceKey(wf *wfv1.Workflow, lockRefKey string) *string {
	for _, t := range wf.Spec.Templates {
		if t.Synchronization != nil && t.Synchronization.Semaphore != nil && t.Synchronization.Semaphore.ConfigMapKeyRef.Key == lockRefKey {
			return t.Synchronization.Semaphore.RebalanceKey
		}
	}
	return nil
}

// checkStrategy tells us if we can proceed to try and acquire using primitive semaphore
func (s *PrioritySemaphore) checkStrategy(holding, pending []string, requesterKey string) (bool, error) {
	items := strings.Split(requesterKey, "/")
	wfKey := items[0] + "/" + items[1]
	lockNameItems := strings.Split(s.name, "/")
	lockRefKey := lockNameItems[len(lockNameItems)-1]

	wf, err := s.getWorkflow(wfKey)
	if err != nil {
		return false, fmt.Errorf("could not find workflow from key %s", wfKey)
	}

	// if rebalance key does not exist, always proceed (try acquire next)
	thisRebalanceKey := getRebalanceKey(wf, lockRefKey)
	if thisRebalanceKey == nil {
		return true, nil
	}

	// count the number of active rebalance keys equaling "thisRebalanceKey"
	thisRebalanceKeyCount := 0

	// nUsers : determine all rebalance keys (pending + holding + this wf)
	allRebalanceKeys := []string{*thisRebalanceKey}
	for _, resourceKey := range pending {
		items := strings.Split(resourceKey, "/")
		wfKey := items[0] + "/" + items[1]
		wf, err := s.getWorkflow(wfKey)
		if err != nil {
			return false, fmt.Errorf("could not find workflow %s but it should exist", wfKey)
		}

		rk := getRebalanceKey(wf, lockRefKey)
		if rk == nil {
			rk = ptr.String("nil")
		}
		foundKey := false
		for _, k := range allRebalanceKeys {
			if k == *rk {
				foundKey = true
			}
		}
		if !foundKey {
			allRebalanceKeys = append(allRebalanceKeys, *rk)
		}
	}
	for _, resourceKey := range holding {
		items := strings.Split(resourceKey, "/")
		wfKey := items[0] + "/" + items[1]
		wf, err := s.getWorkflow(wfKey)
		if err != nil {
			return false, fmt.Errorf("could not find workflow %s but it should exist", wfKey)
		}

		rk := getRebalanceKey(wf, lockRefKey)
		if rk == nil {
			rk = ptr.String("nil")
		}
		if *rk == *thisRebalanceKey {
			thisRebalanceKeyCount += 1
		}
		foundKey := false
		for _, k := range allRebalanceKeys {
			if k == *rk {
				foundKey = true
			}
		}
		if !foundKey {
			allRebalanceKeys = append(allRebalanceKeys, *rk)
		}
	}

	nLocksPerUser := float64(s.limit) / float64(len(allRebalanceKeys))

	return float64(thisRebalanceKeyCount) < nLocksPerUser, nil
}

func (s *PrioritySemaphore) tryAcquire(holderKey string) (bool, string) {
	s.lock.Lock()
	defer s.lock.Unlock()

	if _, ok := s.lockHolder[holderKey]; ok {
		s.log.Debugf("%s is already holding a lock", holderKey)
		return true, ""
	}

	waitingMsg := fmt.Sprintf("Waiting for %s lock. Lock status: %d/%d", s.name, s.limit-len(s.lockHolder), s.limit)

	wfKey := workflowKey(&item{key: holderKey})
	wf, err := s.getWorkflow(wfKey)
	if err != nil {
		return false, fmt.Sprintf("could not find requesting workflow: %s", err.Error())
	}
	lockNameItems := strings.Split(s.name, "/")
	lockRefKey := lockNameItems[len(lockNameItems)-1]

	proceed := true
	if getRebalanceKey(wf, lockRefKey) == nil {
		// Check whether requested holdkey is in front of priority queue.
		// If it is in front position, it will allow to acquire lock.
		// If it is not a front key, it needs to wait for its turn.
		if s.pending.Len() > 0 {
			item := s.pending.peek()
			if holderKey != "" && !isSameWorkflowNodeKeys(holderKey, item.key) {
				// Enqueue the front workflow if lock is available
				if len(s.lockHolder) < s.limit {
					s.nextWorkflow(workflowKey(item))
				}
				return false, waitingMsg
			}
		}
	} else {
		proceed, err = s.checkStrategy(s.getCurrentHolders(), s.getCurrentPending(), holderKey)
		if err != nil {
			return false, fmt.Sprintf("could not check strategy: %s", err.Error())
		}
	}

	if proceed && s.acquire(holderKey) {
		s.pending.pop()
		s.log.Infof("%s acquired by %s. Lock availability: %d/%d", s.name, holderKey, s.limit-len(s.lockHolder), s.limit)
		s.notifyWaiters()
		return true, ""
	}
	s.log.Debugf("Current semaphore Holders. %v", s.lockHolder)
	return false, waitingMsg
}
