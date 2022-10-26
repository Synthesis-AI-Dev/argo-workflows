package sync

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// TODO: maintain cache, expire entries

// RebalanceQueue implements the rebalance strategy for consumption in Semaphore
type RebalanceQueue struct {
	queue             []*itemWithRebalanceKey // standard item with an attached rebalance key
	limit             int                     // global limit for calculating rebalanced queue
	getWorkflow       GetWorkflow             // a pointer to a function that can get a workflow from key
	rebalanceKeyCache map[string]string       // mapping from resource name (holder) to holder's rebalance key
	semaphore         Semaphore               // reference to parent semaphore
}

type itemWithRebalanceKey struct {
	item         *item
	rebalanceKey string
}

func NewRebalanceQueue(limit int, getWorkflow GetWorkflow) *RebalanceQueue {
	return &RebalanceQueue{
		limit:             limit,
		getWorkflow:       getWorkflow,
		queue:             make([]*itemWithRebalanceKey, 0),
		rebalanceKeyCache: make(map[string]string, 0),
	}
}

// references a parent because parent has needed info, so we have to set after parent is instantiated
func (r *RebalanceQueue) setParentSemaphore(s Semaphore) {
	r.semaphore = s
}

func (r *RebalanceQueue) report() {
	// 	libhoney.Init(libhoney.Config{
	// 		WriteKey: "c8ede49b44a6e909ebffe52ca386ea5b",
	// 		Dataset:  "apis-dev",
	// 	})
	// 	defer libhoney.Close() // Flush any pending calls to Honeycomb

	// 	// I want to know, for each "holder", what is the status of its corresponding workflow
	// 	log.Errorf("============= HOLDER INFO =================")
	// 	for _, k := range r.semaphore.getCurrentHolders() {
	// 		ev := libhoney.NewEvent()
	// 		rk, _ := r.getRebalanceKey(k)
	// 		items := strings.Split(k, "/")
	// 		wfKey := items[0] + "/" + items[1]

	// 		items2 := strings.Split(k, "-")

	// 		log.Errorf("* ID: %s\t(RK %s)", items2[len(items2)-1], rk)

	// 		wf, _ := r.getWorkflow(wfKey)
	// 		ev.Add(map[string]interface{}{
	// 			"resource_key":   k,
	// 			"rebalance_key":  rk,
	// 			"workflow_phase": wf.Status.Phase,
	// 		})
	// 		ev.Send()
	// 	}
}

// determines the best ordering based on currently-outstanding keys
func (r *RebalanceQueue) reorder() error {
	// we need pending + holding counts for rebalance keys, and just holding counts
	allRebalanceKeys := make(map[string]int, 0)
	holderRebalanceKeys := make(map[string]int, 0)

	for _, h := range r.semaphore.getCurrentHolders() {
		rk, err := r.getRebalanceKey(h)
		if err != nil {
			return err
		}
		holderRebalanceKeys[rk] += 1
		allRebalanceKeys[rk] += 1
	}

	for _, p := range r.queue {
		rk, err := r.getRebalanceKey(p.item.key)
		if err != nil {
			return err
		}
		allRebalanceKeys[rk] += 1
	}

	// partition r.queue into items that can / can't be scheduled
	can := make([]*itemWithRebalanceKey, 0)
	cant := make([]*itemWithRebalanceKey, 0)

	// iterate through pending queue to determine what has changed
	maxLocksPerUser := math.Floor(float64(r.limit) / float64(len(allRebalanceKeys)))
	for _, q := range r.queue {
		// the number of keys that are currently being held
		totalOutstandingKeys := 0
		for k := range holderRebalanceKeys {
			totalOutstandingKeys += holderRebalanceKeys[k]
		}

		// otherwise, try to distribute lock to this pending holder
		if float64(holderRebalanceKeys[q.rebalanceKey]) < maxLocksPerUser {
			can = append(can, q)
			holderRebalanceKeys[q.rebalanceKey] += 1
		} else {
			cant = append(cant, q)
		}
	}

	r.queue = append(can, cant...)
	return nil
}

// getRebalanceKey first checks cache for rebalance key, based on requester, or calculates it
func (r *RebalanceQueue) getRebalanceKey(requesterKey string) (Key, error) {
	if r.rebalanceKeyCache[requesterKey] != "" {
		return r.rebalanceKeyCache[requesterKey], nil
	}

	lockNameItems := strings.Split(r.semaphore.getName(), "/")
	lockRefKey := lockNameItems[len(lockNameItems)-1]

	rebalanceKey := "<no-rebalance-key>"
	items := strings.Split(requesterKey, "/")
	wfKey := items[0] + "/" + items[1]

	wf, err := r.getWorkflow(wfKey)
	if err != nil {
		return rebalanceKey, fmt.Errorf("could not find workflow from key %s", wfKey)
	}

	for _, t := range wf.Spec.Templates {
		// if rebalance key exists here
		if t.Synchronization != nil && t.Synchronization.Semaphore != nil && t.Synchronization.Semaphore.RebalanceKey != nil {
			// and the lock key refs match
			if t.Synchronization.Semaphore.ConfigMapKeyRef.Key == lockRefKey {
				rebalanceKey = *t.Synchronization.Semaphore.RebalanceKey
			}
		}
	}

	// set cache value for requester
	r.rebalanceKeyCache[requesterKey] = rebalanceKey
	return rebalanceKey, nil
}

func (r *RebalanceQueue) peek() *item {
	return r.queue[0].item
}

func (r *RebalanceQueue) pop() *item {
	first := r.queue[0].item
	r.remove(first.key)
	return first
}

// rebalance queues, at the moment, do not support priority
func (r *RebalanceQueue) add(key Key, _ int32, creationTime time.Time) {
	rk, _ := r.getRebalanceKey(key)
	found := false
	for _, q := range r.queue {
		if q.item.key == key {
			found = true
		}
	}
	// ?? for some reason TryAcquire always tries to add again, so this function (as in the priority
	// queue), must check for this key's existence
	if !found {
		r.queue = append(r.queue, &itemWithRebalanceKey{
			item:         &item{key: key, creationTime: creationTime, priority: 1, index: -1},
			rebalanceKey: rk,
		})
	}
}

func (r *RebalanceQueue) remove(key Key) {
	for i, iter := range r.queue {
		if iter.item.key == key {
			r.queue = append(r.queue[:i], r.queue[i+1:]...)
			return
		}
	}
}

func (r *RebalanceQueue) Len() int {
	return len(r.queue)
}

func (r *RebalanceQueue) all() []*item {
	items := make([]*item, len(r.queue))
	for i := range r.queue {
		items[i] = r.queue[i].item
	}
	return items
}
