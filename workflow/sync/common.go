package sync

import "time"

type Semaphore interface {
	acquire(holderKey string) bool
	tryAcquire(holderKey string) (bool, string)
	release(key string) bool
	addToQueue(holderKey string, priority int32, creationTime time.Time)
	removeFromQueue(holderKey string)
	getCurrentHolders() []string
	getCurrentPending() []string
	getName() string
	getLimit() int
	resize(n int) bool
}

type OrderedItems interface {
	reorder() error
	report()
	peek() *item
	pop() *item
	add(key Key, priority int32, creationTime time.Time)
	remove(key Key)
	all() []*item
	Len() int
}
