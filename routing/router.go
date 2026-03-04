package routing

import (
	"hash/crc32"
	"sort"
	"sync"
)

// Router implements consistent hashing for stateless routing
type Router struct {
	mu           sync.RWMutex
	nodes        []string
	ring         []uint32
	nodeMap      map[uint32]string
	virtualNodes int
}

// NewRouter creates a consistent hash router
func NewRouter(nodes []string) *Router {
	r := &Router{
		nodes:        nodes,
		nodeMap:      make(map[uint32]string),
		virtualNodes: 50, // Virtual nodes per real node for better distribution
	}
	r.buildRing()
	return r
}

func (r *Router) buildRing() {
	r.ring = nil
	r.nodeMap = make(map[uint32]string)

	for _, node := range r.nodes {
		for i := 0; i < r.virtualNodes; i++ {
			key := hashKey(node, i)
			r.ring = append(r.ring, key)
			r.nodeMap[key] = node
		}
	}

	sort.Slice(r.ring, func(i, j int) bool {
		return r.ring[i] < r.ring[j]
	})
}

func hashKey(node string, vnode int) uint32 {
	key := []byte(node + string(rune(vnode)))
	return crc32.ChecksumIEEE(key)
}

// GetNode returns the node responsible for a key
func (r *Router) GetNode(key string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if len(r.ring) == 0 {
		return ""
	}

	hash := crc32.ChecksumIEEE([]byte(key))

	// Binary search for first node >= hash
	idx := sort.Search(len(r.ring), func(i int) bool {
		return r.ring[i] >= hash
	})

	// Wrap around
	if idx >= len(r.ring) {
		idx = 0
	}

	return r.nodeMap[r.ring[idx]]
}

// GetNodes returns all registered nodes
func (r *Router) GetNodes() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]string{}, r.nodes...)
}

// AddNode adds a node to the ring
func (r *Router) AddNode(node string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, n := range r.nodes {
		if n == node {
			return
		}
	}

	r.nodes = append(r.nodes, node)
	r.buildRing()
}

// RemoveNode removes a node from the ring
func (r *Router) RemoveNode(node string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for i, n := range r.nodes {
		if n == node {
			r.nodes = append(r.nodes[:i], r.nodes[i+1:]...)
			break
		}
	}
	r.buildRing()
}
