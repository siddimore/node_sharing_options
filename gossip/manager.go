package gossip

import (
	"encoding/json"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/memberlist"
)

// NodeMeta contains metadata broadcast via gossip
type NodeMeta struct {
	ID           string   `json:"id"`
	Load         int64    `json:"load"`
	Healthy      bool     `json:"healthy"`
	Timestamp    int64    `json:"timestamp"`
	CachedLabels []string `json:"cached_labels,omitempty"`
}

// Manager handles gossip protocol operations
type Manager struct {
	id         string
	list       *memberlist.Memberlist
	broadcasts *memberlist.TransmitLimitedQueue

	mu           sync.RWMutex
	peers        map[string]*NodeMeta
	selfLoad     int64
	cachedLabels []string

	// Metrics for tracking gossip overhead
	msgSent      int64
	msgRecv      int64
	bytesSent    int64
	bytesRecv    int64
	startTime    time.Time
}

// GossipMetrics contains gossip network overhead statistics
type GossipMetrics struct {
	MessagesSent     int64   `json:"messages_sent"`
	MessagesRecv     int64   `json:"messages_recv"`
	BytesSent        int64   `json:"bytes_sent"`
	BytesRecv        int64   `json:"bytes_recv"`
	UptimeSeconds    float64 `json:"uptime_seconds"`
	MsgPerSecSent    float64 `json:"msg_per_sec_sent"`
	MsgPerSecRecv    float64 `json:"msg_per_sec_recv"`
	BytesPerSecSent  float64 `json:"bytes_per_sec_sent"`
	BytesPerSecRecv  float64 `json:"bytes_per_sec_recv"`
	GossipIntervalMs int     `json:"gossip_interval_ms"`
	ProbeIntervalMs  int     `json:"probe_interval_ms"`
	BroadcastIntervalMs int  `json:"broadcast_interval_ms"`
}

// Delegate implements memberlist.Delegate interface
type Delegate struct {
	mgr *Manager
}

func (d *Delegate) NodeMeta(limit int) []byte {
	d.mgr.mu.RLock()
	defer d.mgr.mu.RUnlock()

	meta := NodeMeta{
		ID:           d.mgr.id,
		Load:         d.mgr.selfLoad,
		Healthy:      true,
		Timestamp:    time.Now().UnixMilli(),
		CachedLabels: d.mgr.cachedLabels,
	}

	data, _ := json.Marshal(meta)
	atomic.AddInt64(&d.mgr.bytesSent, int64(len(data)))
	atomic.AddInt64(&d.mgr.msgSent, 1)
	return data
}

func (d *Delegate) NotifyMsg(msg []byte) {
	atomic.AddInt64(&d.mgr.bytesRecv, int64(len(msg)))
	atomic.AddInt64(&d.mgr.msgRecv, 1)

	var meta NodeMeta
	if err := json.Unmarshal(msg, &meta); err != nil {
		return
	}

	d.mgr.mu.Lock()
	d.mgr.peers[meta.ID] = &meta
	d.mgr.mu.Unlock()
}

func (d *Delegate) GetBroadcasts(overhead, limit int) [][]byte {
	if d.mgr.broadcasts == nil {
		return nil
	}
	msgs := d.mgr.broadcasts.GetBroadcasts(overhead, limit)
	for _, msg := range msgs {
		atomic.AddInt64(&d.mgr.bytesSent, int64(len(msg)))
		atomic.AddInt64(&d.mgr.msgSent, 1)
	}
	return msgs
}

func (d *Delegate) LocalState(join bool) []byte {
	d.mgr.mu.RLock()
	defer d.mgr.mu.RUnlock()

	meta := NodeMeta{
		ID:           d.mgr.id,
		Load:         d.mgr.selfLoad,
		Healthy:      true,
		Timestamp:    time.Now().UnixMilli(),
		CachedLabels: d.mgr.cachedLabels,
	}

	data, _ := json.Marshal(meta)
	atomic.AddInt64(&d.mgr.bytesSent, int64(len(data)))
	atomic.AddInt64(&d.mgr.msgSent, 1)
	return data
}

func (d *Delegate) MergeRemoteState(buf []byte, join bool) {
	atomic.AddInt64(&d.mgr.bytesRecv, int64(len(buf)))
	atomic.AddInt64(&d.mgr.msgRecv, 1)

	var meta NodeMeta
	if err := json.Unmarshal(buf, &meta); err != nil {
		return
	}

	d.mgr.mu.Lock()
	d.mgr.peers[meta.ID] = &meta
	d.mgr.mu.Unlock()
}

// EventDelegate handles membership events
type EventDelegate struct {
	mgr *Manager
}

func (e *EventDelegate) NotifyJoin(node *memberlist.Node) {
	if node.Name == e.mgr.id {
		return
	}
	log.Printf("[GOSSIP] Node joined: %s", node.Name)

	var meta NodeMeta
	if len(node.Meta) > 0 {
		json.Unmarshal(node.Meta, &meta)
	} else {
		meta = NodeMeta{ID: node.Name, Load: 0, Healthy: true}
	}

	e.mgr.mu.Lock()
	e.mgr.peers[node.Name] = &meta
	e.mgr.mu.Unlock()
}

func (e *EventDelegate) NotifyLeave(node *memberlist.Node) {
	log.Printf("[GOSSIP] Node left: %s", node.Name)

	e.mgr.mu.Lock()
	delete(e.mgr.peers, node.Name)
	e.mgr.mu.Unlock()
}

func (e *EventDelegate) NotifyUpdate(node *memberlist.Node) {
	if node.Name == e.mgr.id {
		return
	}

	var meta NodeMeta
	if len(node.Meta) > 0 {
		json.Unmarshal(node.Meta, &meta)
	}

	e.mgr.mu.Lock()
	e.mgr.peers[node.Name] = &meta
	e.mgr.mu.Unlock()
}

// NewManager creates a new gossip manager
func NewManager(id string, port int) (*Manager, error) {
	mgr := &Manager{
		id:        id,
		peers:     make(map[string]*NodeMeta),
		startTime: time.Now(),
	}

	config := memberlist.DefaultLANConfig()
	config.Name = id
	config.BindPort = port
	config.AdvertisePort = port

	// Faster gossip for demo
	config.GossipInterval = 200 * time.Millisecond
	config.ProbeInterval = 500 * time.Millisecond
	config.PushPullInterval = 1 * time.Second

	config.Delegate = &Delegate{mgr: mgr}
	config.Events = &EventDelegate{mgr: mgr}

	// Reduce log noise
	config.LogOutput = log.Writer()

	list, err := memberlist.Create(config)
	if err != nil {
		return nil, err
	}

	mgr.list = list
	mgr.broadcasts = &memberlist.TransmitLimitedQueue{
		NumNodes:       func() int { return list.NumMembers() },
		RetransmitMult: 3,
	}

	// Periodically broadcast our load
	go mgr.broadcastLoop()

	return mgr, nil
}

func (m *Manager) broadcastLoop() {
	ticker := time.NewTicker(300 * time.Millisecond)
	for range ticker.C {
		m.mu.RLock()
		meta := NodeMeta{
			ID:        m.id,
			Load:      m.selfLoad,
			Healthy:   true,
			Timestamp: time.Now().UnixMilli(),
		}
		m.mu.RUnlock()

		data, _ := json.Marshal(meta)
		m.broadcasts.QueueBroadcast(&broadcast{msg: data})
	}
}

type broadcast struct {
	msg []byte
}

func (b *broadcast) Invalidates(other memberlist.Broadcast) bool {
	return false
}

func (b *broadcast) Message() []byte {
	return b.msg
}

func (b *broadcast) Finished() {}

// Join connects to existing cluster members
func (m *Manager) Join(addrs []string) error {
	_, err := m.list.Join(addrs)
	return err
}

// UpdateLoad updates this node's load score
func (m *Manager) UpdateLoad(load int64) {
	m.mu.Lock()
	m.selfLoad = load
	m.mu.Unlock()

	// Trigger metadata update
	m.list.UpdateNode(time.Second)
}

// GetPeers returns current peer loads
func (m *Manager) GetPeers() map[string]int64 {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make(map[string]int64)
	for id, meta := range m.peers {
		if meta.Healthy {
			result[id] = meta.Load
		}
	}
	return result
}

// GetAllNodes returns all known nodes including self
func (m *Manager) GetAllNodes() map[string]int64 {
	result := m.GetPeers()
	m.mu.RLock()
	result[m.id] = m.selfLoad
	m.mu.RUnlock()
	return result
}

// NumMembers returns cluster size
func (m *Manager) NumMembers() int {
	return m.list.NumMembers()
}

// UpdateCachedLabels updates this node's cached labels
func (m *Manager) UpdateCachedLabels(labels []string) {
	m.mu.Lock()
	m.cachedLabels = labels
	m.mu.Unlock()

	// Trigger metadata update
	m.list.UpdateNode(time.Second)
}

// GetNodesWithLabel returns nodes that have the specified label cached, with their loads
func (m *Manager) GetNodesWithLabel(label string) map[string]int64 {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make(map[string]int64)
	
	// Check self
	for _, l := range m.cachedLabels {
		if l == label {
			result[m.id] = m.selfLoad
			break
		}
	}
	
	// Check peers
	for id, meta := range m.peers {
		if !meta.Healthy {
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

// GetAllNodesWithLabels returns all nodes with their loads and cached labels
func (m *Manager) GetAllNodesWithLabels() map[string]*NodeMeta {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make(map[string]*NodeMeta)
	
	// Add self
	result[m.id] = &NodeMeta{
		ID:           m.id,
		Load:         m.selfLoad,
		Healthy:      true,
		CachedLabels: m.cachedLabels,
	}
	
	// Add peers
	for id, meta := range m.peers {
		if meta.Healthy {
			result[id] = meta
		}
	}
	return result
}

// Shutdown gracefully leaves the cluster
func (m *Manager) Shutdown() error {
	return m.list.Shutdown()
}

// GetMetrics returns gossip network overhead statistics
func (m *Manager) GetMetrics() GossipMetrics {
	uptime := time.Since(m.startTime).Seconds()
	msgSent := atomic.LoadInt64(&m.msgSent)
	msgRecv := atomic.LoadInt64(&m.msgRecv)
	bytesSent := atomic.LoadInt64(&m.bytesSent)
	bytesRecv := atomic.LoadInt64(&m.bytesRecv)

	return GossipMetrics{
		MessagesSent:        msgSent,
		MessagesRecv:        msgRecv,
		BytesSent:           bytesSent,
		BytesRecv:           bytesRecv,
		UptimeSeconds:       uptime,
		MsgPerSecSent:       float64(msgSent) / uptime,
		MsgPerSecRecv:       float64(msgRecv) / uptime,
		BytesPerSecSent:     float64(bytesSent) / uptime,
		BytesPerSecRecv:     float64(bytesRecv) / uptime,
		GossipIntervalMs:    200,
		ProbeIntervalMs:     500,
		BroadcastIntervalMs: 300,
	}
}
