package roundrobin

import (
	"container/heap"
	"fmt"
	"github.com/golang/glog"
	"github.com/mailgun/vulcan/loadbalance"
	"hash/fnv"
)

type cursorMap struct {
	// collection of cursors identified by the ids of endpoints
	cursors map[uint32][]*cursor
	// keep expiration times in the priority queue (min heap) so we can TTL effectively
	expiryTimes *priorityQueue
}

func newCursorMap() *cursorMap {
	m := &cursorMap{
		cursors:     make(map[uint32][]*cursor),
		expiryTimes: &priorityQueue{},
	}

	heap.Init(m.expiryTimes)
	return m
}

func (cm *cursorMap) upsertCursor(endpoints []loadbalance.Endpoint, expiryTime int) *cursor {
	c := cm.getCursor(endpoints)
	if c != nil {
		// In case if the set is present, use it and update expiry seconds
		cm.expiryTimes.Update(c.item, expiryTime)
		return c
	} else {
		c = cm.addCursor(endpoints)
		// In case if we have not seen this set of endpoints before,
		// add it to the expiryTimes priority queue and the map of our endpoint set
		item := &pqItem{
			c:        c,
			priority: expiryTime,
		}
		c.item = item
		heap.Push(cm.expiryTimes, item)
		return c
	}
}

// Returns cursort for the given endpoints set, or returns nil if there's none
func (cm *cursorMap) getCursor(endpoints []loadbalance.Endpoint) *cursor {
	cursorHash := computeHash(endpoints)
	// Find if the endpoints combination we are referring to already exists
	cursors, exists := cm.cursors[cursorHash]
	if !exists {
		return nil
	}
	if len(cursors) == 1 {
		return cursors[0]
	} else {
		for _, c := range cursors {
			if c.sameEndpoints(endpoints) {
				return c
			}
		}
	}
	return nil
}

// Add a new cursor to the collection of cursors
func (cm *cursorMap) addCursor(endpoints []loadbalance.Endpoint) *cursor {
	c := newCursor(endpoints)
	cursors, exists := cm.cursors[c.hash]
	if !exists {
		cm.cursors[c.hash] = []*cursor{c}
	} else {
		cm.cursors[c.hash] = append(cursors, c)
	}
	return c
}

// Add a new cursor to the collection of cursors by cursor hash and it's index in the collection
func (cm *cursorMap) cursorIndex(c *cursor) int {
	cursors, exists := cm.cursors[c.hash]
	if !exists {
		return -1
	}
	for i, c2 := range cursors {
		if c2 == c {
			return i
		}
	}
	return -1
}

// Add a new cursor to the collection of cursors by cursor hash and it's index in the collection
func (cm *cursorMap) deleteCursor(c *cursor) error {
	cursors, exists := cm.cursors[c.hash]
	if !exists {
		return fmt.Errorf("Cursor not found")
	}
	index := cm.cursorIndex(c)
	if index == -1 {
		return fmt.Errorf("Cursor not found")
	}
	if index == 0 && len(cursors) == 1 {
		delete(cm.cursors, c.hash)
	} else {
		cm.cursors[c.hash] = append(cursors[:index], cursors[index+1:]...)
	}
	return nil
}

func computeHash(endpoints []loadbalance.Endpoint) uint32 {
	h := fnv.New32()
	for _, endpoint := range endpoints {
		h.Write([]byte(endpoint.Id()))
	}
	return h.Sum32()
}

// Determines if two endpoint sets are equal by comparing endpoint ids one by one
func endpointSetsEqual(a []loadbalance.Endpoint, b []loadbalance.Endpoint) bool {
	if len(a) != len(b) {
		return false
	}
	for i, _ := range a {
		if a[i].Id() != b[i].Id() {
			return false
		}
	}
	return true
}

func (cm *cursorMap) deleteExpiredCursors(now int) {
	glog.Infof("Gc start: %d cursors, expiry times: %d", len(cm.cursors), cm.expiryTimes.Len())
	for {
		if cm.expiryTimes.Len() == 0 {
			break
		}
		item := cm.expiryTimes.Peek()
		if item.priority > now {
			glog.Infof("Nothing to expire, earliest expiry is: Cursor(id=%s, lastAccess=%d), now is %d", item.c.hash, item.priority, now)
			break
		} else {
			glog.Infof("Cursor(id=%s, lastAccess=%d) has expired (now=%d), deleting", item.c.hash, item.priority, now)
			pitem := heap.Pop(cm.expiryTimes)
			item := pitem.(*pqItem)
			cm.deleteCursor(item.c)
		}
	}
	glog.Infof("RoundRobin gc end: %d cursors, expiry times: %d", len(cm.cursors), cm.expiryTimes.Len())
}

// Cursor represents the current position in the given endpoints sequence
type cursor struct {
	// position in the upstreams
	index       int
	hash        uint32
	item        *pqItem
	endpointIds []string
}

func newCursor(endpoints []loadbalance.Endpoint) *cursor {
	endpointIds := make([]string, len(endpoints))
	for i, endpoint := range endpoints {
		endpointIds[i] = endpoint.Id()
	}
	return &cursor{
		index:       0,
		hash:        computeHash(endpoints),
		endpointIds: endpointIds,
	}
}

func (c *cursor) sameEndpoints(endpoints []loadbalance.Endpoint) bool {
	if len(c.endpointIds) != len(endpoints) {
		return false
	}
	for i, _ := range endpoints {
		if c.endpointIds[i] != endpoints[i].Id() {
			return false
		}
	}
	return true
}

func (c *cursor) next(endpoints []loadbalance.Endpoint) (loadbalance.Endpoint, error) {
	for i := 0; i < len(endpoints); i++ {
		endpoint := endpoints[c.index]
		c.index = (c.index + 1) % len(endpoints)
		if endpoint.IsActive() {
			return endpoint, nil
		} else {
			glog.Infof("Skipping inactive endpoint: %s", endpoint.Id())
		}
	}
	// That means that we did full circle and found nothing
	return nil, fmt.Errorf("No available endpoints!")
}
