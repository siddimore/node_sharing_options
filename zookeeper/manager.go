package zookeeper

import (
	"encoding/json"
	"fmt"
	"log"
	"path"
	"sync"
	"time"

	"github.com/go-zookeeper/zk"
)

const (
	// ZK paths
	basePath   = "/gossip_demo"
	nodesPath  = "/gossip_demo/nodes"
	labelsPath = "/gossip_demo/labels"
)

// NodeMeta contains metadata stored in znode
type NodeMeta struct {
	ID           string   `json:"id"`
	Load         int64    `json:"load"`
	Healthy      bool     `json:"healthy"`
	Timestamp    int64    `json:"timestamp"`
	CachedLabels []string `json:"cached_labels,omitempty"`
}

// Manager handles Zookeeper coordination operations
type Manager struct {
	id         string
	conn       *zk.Conn
	zkAddr     string

	mu           sync.RWMutex
	peers        map[string]*NodeMeta
	selfLoad     int64
	cachedLabels []string

	// Metrics for tracking ZK operations
	writeOps    int64
	readOps     int64
	watchEvents int64
	startTime   time.Time

	// Control channels
	stopCh chan struct{}
}

// ZKMetrics contains Zookeeper operation statistics
type ZKMetrics struct {
	WriteOps       int64   `json:"write_ops"`
	ReadOps        int64   `json:"read_ops"`
	WatchEvents    int64   `json:"watch_events"`
	UptimeSeconds  float64 `json:"uptime_seconds"`
	WritesPerSec   float64 `json:"writes_per_sec"`
	ReadsPerSec    float64 `json:"reads_per_sec"`
	UpdateInterval int     `json:"update_interval_ms"`
}

// NewManager creates a new Zookeeper manager
func NewManager(id, zkAddr string) (*Manager, error) {
	mgr := &Manager{
		id:        id,
		zkAddr:    zkAddr,
		peers:     make(map[string]*NodeMeta),
		startTime: time.Now(),
		stopCh:    make(chan struct{}),
	}

	if err := mgr.connect(); err != nil {
		return nil, fmt.Errorf("failed to connect to Zookeeper: %v", err)
	}

	if err := mgr.ensurePaths(); err != nil {
		return nil, fmt.Errorf("failed to create ZK paths: %v", err)
	}

	// Register this node
	if err := mgr.registerNode(); err != nil {
		return nil, fmt.Errorf("failed to register node: %v", err)
	}

	// Start background tasks
	go mgr.watchNodes()
	go mgr.publishLoadPeriodically()

	log.Printf("[%s] Zookeeper manager started, connected to %s", id, zkAddr)
	return mgr, nil
}

// connect establishes connection to Zookeeper
func (m *Manager) connect() error {
	servers := []string{m.zkAddr}
	conn, _, err := zk.Connect(servers, 10*time.Second, zk.WithLogInfo(false))
	if err != nil {
		return err
	}

	// Wait for connection
	timeout := time.After(10 * time.Second)
	for {
		select {
		case <-timeout:
			conn.Close()
			return fmt.Errorf("connection timeout")
		default:
			if conn.State() == zk.StateHasSession {
				m.conn = conn
				return nil
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
}

// ensurePaths creates required ZK paths if they don't exist
func (m *Manager) ensurePaths() error {
	paths := []string{basePath, nodesPath, labelsPath}
	for _, p := range paths {
		exists, _, err := m.conn.Exists(p)
		if err != nil {
			return err
		}
		if !exists {
			_, err = m.conn.Create(p, []byte{}, 0, zk.WorldACL(zk.PermAll))
			if err != nil && err != zk.ErrNodeExists {
				return err
			}
		}
	}
	return nil
}

// registerNode creates an ephemeral znode for this node
func (m *Manager) registerNode() error {
	nodePath := path.Join(nodesPath, m.id)

	meta := NodeMeta{
		ID:        m.id,
		Load:      0,
		Healthy:   true,
		Timestamp: time.Now().UnixMilli(),
	}
	data, _ := json.Marshal(meta)

	// Create ephemeral node - automatically deleted when session ends
	_, err := m.conn.Create(nodePath, data, zk.FlagEphemeral, zk.WorldACL(zk.PermAll))
	if err != nil && err != zk.ErrNodeExists {
		return err
	}

	// If node exists (reconnect scenario), update it
	if err == zk.ErrNodeExists {
		_, err = m.conn.Set(nodePath, data, -1)
		if err != nil {
			return err
		}
	}

	m.mu.Lock()
	m.writeOps++
	m.mu.Unlock()
	return nil
}

// publishLoadPeriodically updates this node's load in Zookeeper
func (m *Manager) publishLoadPeriodically() {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			m.publishLoad()
		}
	}
}

// publishLoad updates the node's znode with current load
func (m *Manager) publishLoad() {
	m.mu.RLock()
	meta := NodeMeta{
		ID:           m.id,
		Load:         m.selfLoad,
		Healthy:      true,
		Timestamp:    time.Now().UnixMilli(),
		CachedLabels: m.cachedLabels,
	}
	m.mu.RUnlock()

	data, _ := json.Marshal(meta)
	nodePath := path.Join(nodesPath, m.id)

	_, err := m.conn.Set(nodePath, data, -1)
	if err != nil {
		// Node might not exist, try to recreate
		if err == zk.ErrNoNode {
			m.registerNode()
		} else {
			log.Printf("[%s] Failed to update ZK node: %v", m.id, err)
		}
		return
	}

	m.mu.Lock()
	m.writeOps++
	m.mu.Unlock()
}

// watchNodes watches for changes in the nodes path
func (m *Manager) watchNodes() {
	for {
		select {
		case <-m.stopCh:
			return
		default:
		}

		children, _, eventCh, err := m.conn.ChildrenW(nodesPath)
		if err != nil {
			log.Printf("[%s] Failed to watch nodes: %v", m.id, err)
			time.Sleep(1 * time.Second)
			continue
		}

		// Fetch all node data
		m.fetchAllNodes(children)

		// Wait for change event
		select {
		case <-m.stopCh:
			return
		case event := <-eventCh:
			m.mu.Lock()
			m.watchEvents++
			m.mu.Unlock()

			if event.Type == zk.EventNodeChildrenChanged {
				log.Printf("[%s] ZK nodes changed, refreshing...", m.id)
			}
		}
	}
}

// fetchAllNodes reads all node metadata from Zookeeper
func (m *Manager) fetchAllNodes(children []string) {
	newPeers := make(map[string]*NodeMeta)

	for _, child := range children {
		nodePath := path.Join(nodesPath, child)
		data, _, err := m.conn.Get(nodePath)
		if err != nil {
			continue
		}

		m.mu.Lock()
		m.readOps++
		m.mu.Unlock()

		var meta NodeMeta
		if err := json.Unmarshal(data, &meta); err != nil {
			continue
		}

		newPeers[child] = &meta
	}

	m.mu.Lock()
	m.peers = newPeers
	m.mu.Unlock()
}

// UpdateLoad updates this node's load score
func (m *Manager) UpdateLoad(load int64) {
	m.mu.Lock()
	m.selfLoad = load
	m.mu.Unlock()
}

// UpdateCachedLabels updates the cached labels for this node
func (m *Manager) UpdateCachedLabels(labels []string) {
	m.mu.Lock()
	m.cachedLabels = labels
	m.mu.Unlock()
}

// GetAllNodes returns all nodes with their loads
func (m *Manager) GetAllNodes() map[string]int64 {
	result := make(map[string]int64)

	m.mu.RLock()
	defer m.mu.RUnlock()

	for id, meta := range m.peers {
		// Check if node is stale (>2 seconds old)
		if time.Now().UnixMilli()-meta.Timestamp < 2000 {
			result[id] = meta.Load
		}
	}

	return result
}

// GetNodesWithLabel returns nodes that have a specific label cached
func (m *Manager) GetNodesWithLabel(label string) map[string]int64 {
	result := make(map[string]int64)

	m.mu.RLock()
	defer m.mu.RUnlock()

	for id, meta := range m.peers {
		// Check if node is stale
		if time.Now().UnixMilli()-meta.Timestamp >= 2000 {
			continue
		}

		for _, l := range meta.CachedLabels {
			if l == label {
				result[id] = meta.Load
				break
			}
		}
	}

	return result
}

// GetPeers returns all known peers with their loads
func (m *Manager) GetPeers() map[string]int64 {
	return m.GetAllNodes()
}

// NumMembers returns the number of known cluster members
func (m *Manager) NumMembers() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.peers)
}

// GetMetrics returns Zookeeper operation metrics
func (m *Manager) GetMetrics() ZKMetrics {
	m.mu.RLock()
	defer m.mu.RUnlock()

	uptime := time.Since(m.startTime).Seconds()

	return ZKMetrics{
		WriteOps:       m.writeOps,
		ReadOps:        m.readOps,
		WatchEvents:    m.watchEvents,
		UptimeSeconds:  uptime,
		WritesPerSec:   float64(m.writeOps) / uptime,
		ReadsPerSec:    float64(m.readOps) / uptime,
		UpdateInterval: 200,
	}
}

// Close shuts down the Zookeeper manager
func (m *Manager) Close() {
	close(m.stopCh)

	// Delete our ephemeral node explicitly
	nodePath := path.Join(nodesPath, m.id)
	m.conn.Delete(nodePath, -1)

	m.conn.Close()
}
