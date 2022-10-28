package sync

import (
	"math"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v3/pkg/apis/workflow/v1alpha1"
)

// TODO: maintain cache, expire entries

// RebalanceQueue implements the rebalance strategy for consumption in Semaphore
type RebalanceQueue struct {
	queue             []*itemWithRebalanceKey // standard item with an attached rebalance key
	limit             int                     // global limit for calculating rebalanced queue
	rebalanceKeyCache map[string]string       // mapping from resource name (holder) to holder's rebalance key
	semaphore         Semaphore               // reference to parent semaphore
}

type itemWithRebalanceKey struct {
	item         *item
	rebalanceKey string
}

func NewRebalanceQueue(limit int) *RebalanceQueue {
	return &RebalanceQueue{
		limit:             limit,
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
func (r *RebalanceQueue) onRelease(key Key) error {
	// we need pending + holding counts for rebalance keys, and just holding counts
	allRebalanceKeys := make(map[string]int, 0)
	holderRebalanceKeys := make(map[string]int, 0)

	for _, h := range r.semaphore.getCurrentHolders() {
		rk := r.rebalanceKeyCache[h]
		holderRebalanceKeys[rk] += 1
		allRebalanceKeys[rk] += 1
	}

	for _, p := range r.queue {
		rk := r.rebalanceKeyCache[p.item.key]
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

	delete(r.rebalanceKeyCache, key)

	r.queue = append(can, cant...)
	return nil
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
//
// if queue has key, skip
// if queue doesn't, add along with rebalance key
func (r *RebalanceQueue) add(key Key, _ int32, creationTime time.Time, syncLockRef *wfv1.Synchronization) {
	found := false
	for _, q := range r.queue {
		if q.item.key == key {
			found = true
		}
	}

	if !found {
		rebalanceKey := "<no-key-default-key>"
		if syncLockRef != nil && syncLockRef.Semaphore != nil && syncLockRef.Semaphore.RebalanceKey != nil {
			rebalanceKey = *syncLockRef.Semaphore.RebalanceKey
		}

		r.queue = append(r.queue, &itemWithRebalanceKey{
			item:         &item{key: key, creationTime: creationTime, priority: 1, index: -1},
			rebalanceKey: rebalanceKey,
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
