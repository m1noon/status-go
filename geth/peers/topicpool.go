package peers

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/discover"
	"github.com/ethereum/go-ethereum/p2p/discv5"
	"github.com/status-im/status-go/geth/params"
)

// NewTopicPool returns instance of TopicPool
func NewTopicPool(topic discv5.Topic, limits params.Limits, slowSync, fastSync time.Duration) *TopicPool {
	return &TopicPool{
		topic:    topic,
		limits:   limits,
		slowSync: slowSync,
		fastSync: fastSync,
		peers:    map[discv5.NodeID]*peerInfo{},
	}
}

// TopicPool manages peers for topic.
type TopicPool struct {
	topic    discv5.Topic
	limits   params.Limits
	slowSync time.Duration
	fastSync time.Duration

	quit    chan struct{}
	running int32

	mu         sync.RWMutex
	discvWG    sync.WaitGroup
	consumerWG sync.WaitGroup
	connected  int
	peers      map[discv5.NodeID]*peerInfo
	period     chan time.Duration

	cache *Cache
}

// SearchRunning returns true if search is running
func (t *TopicPool) SearchRunning() bool {
	return atomic.LoadInt32(&t.running) == 1
}

// MaxReached returns true if we connected with max number of peers.
func (t *TopicPool) MaxReached() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.connected == t.limits[1]
}

// BelowMin returns true if current number of peers is below min limit.
func (t *TopicPool) BelowMin() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.connected < t.limits[0]
}

// ConfirmAdded called when peer was added by p2p Server
func (t *TopicPool) ConfirmAdded(server *p2p.Server, nodeID discover.NodeID) {
	t.mu.Lock()
	defer t.mu.Unlock()
	// inbound connection
	peer, exist := t.peers[discv5.NodeID(nodeID)]
	if !exist {
		return
	}
	// established connection means that the node is a viable candidate for a connection and can be cached
	if t.cache != nil {
		if err := t.cache.AddPeer(peer.node, t.topic); err != nil {
			log.Error("failed to persist a peer", "error", err)
		}
	}
	// when max limit is reached drop every peer after
	if t.connected == t.limits[1] {
		log.Debug("max limit is reached drop the peer", "ID", nodeID, "topic", t.topic)
		peer.requested = true
		t.removePeer(server, peer)
		return
	}
	// don't count same peer twice
	if !peer.connected {
		log.Debug("marking as connected", "ID", nodeID)
		peer.connected = true
		t.connected++
	}
	if t.SearchRunning() && t.connected == t.limits[0] {
		t.period <- t.slowSync
	}
	return
}

// ConfirmDropped called when server receives drop event
func (t *TopicPool) ConfirmDropped(server *p2p.Server, nodeID discover.NodeID, reason string) (info *peerInfo, ignored bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	// either inbound or connected from another topic
	peer, exist := t.peers[discv5.NodeID(nodeID)]
	if !exist {
		return nil, true
	}
	log.Debug("disconnect reason", "ID", nodeID, "reason", reason)
	if peer.requested {
		return nil, true
	}
	if t.SearchRunning() && t.connected == t.limits[0] {
		t.period <- t.fastSync
	}
	t.connected--
	t.removePeer(server, peer)
	delete(t.peers, discv5.NodeID(nodeID))
	if t.cache != nil {
		if err := t.cache.RemovePeer(discv5.NodeID(nodeID), t.topic); err != nil {
			log.Error("failed to remove peer from cache", "error", err)
		}
	}
	// TODO use a heap queue and always get a peer that was discovered recently
	for _, peer := range t.peers {
		if !peer.connected && mclock.Now() < peer.discoveredTime+mclock.AbsTime(expirationPeriod) {
			t.addPeer(server, peer)
			return peer, false
		}
	}
	return nil, false
}

// StartSearch creates discv5 queries and runs a loop to consume found peers.
func (t *TopicPool) StartSearch(server *p2p.Server) error {
	if atomic.LoadInt32(&t.running) == 1 {
		return nil
	}
	if server.DiscV5 == nil {
		return ErrDiscv5NotRunning
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	atomic.StoreInt32(&t.running, 1)
	t.quit = make(chan struct{})
	t.period = make(chan time.Duration, 2)
	t.quit = make(chan struct{})
	found := make(chan *discv5.Node, 10)
	lookup := make(chan bool, 100)
	if t.cache != nil {
		for _, peer := range t.cache.GetPeersRange(t.topic, 5) {
			log.Debug("adding a peer from cache", "peer", peer)
			found <- peer
		}
	}
	t.discvWG.Add(1)
	go func() {
		server.DiscV5.SearchTopic(t.topic, t.period, found, lookup)
		t.discvWG.Done()
	}()
	t.consumerWG.Add(1)
	go func() {
		t.handleFoundPeers(server, found, lookup)
		t.consumerWG.Done()
	}()
	return nil
}

func (t *TopicPool) handleFoundPeers(server *p2p.Server, found <-chan *discv5.Node, lookup <-chan bool) {
	if t.connected >= t.limits[0] {
		t.period <- t.slowSync
	} else {
		t.period <- t.fastSync
	}
	selfID := discv5.NodeID(server.Self().ID)
	for {
		select {
		case <-t.quit:
			return
		case <-lookup:
		case node := <-found:
			if node.ID != selfID {
				t.processFoundNode(server, node)
			}
		}
	}
}

// processFoundNode called when node is discovered by kademlia search query
// 2 important conditions
// 1. every time when node is processed we need to update discoveredTime and reset dropped boolean.
//    peer will be considered as valid later only if it was discovered < 60m ago and wasn't dropped recently
// 2. if peer is connected or if max limit is reached we are not a adding peer to p2p server
func (t *TopicPool) processFoundNode(server *p2p.Server, node *discv5.Node) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if info, exist := t.peers[node.ID]; exist {
		info.discoveredTime = mclock.Now()
	} else {
		t.peers[node.ID] = &peerInfo{
			discoveredTime: mclock.Now(),
			node:           node,
		}
	}
	if t.connected < t.limits[1] && !t.peers[node.ID].connected {
		log.Debug("peer found", "ID", node.ID, "topic", t.topic)
		t.addPeer(server, t.peers[node.ID])
	}
	return
}

func (t *TopicPool) addPeer(server *p2p.Server, info *peerInfo) {
	server.AddPeer(discover.NewNode(
		discover.NodeID(info.node.ID),
		info.node.IP,
		info.node.UDP,
		info.node.TCP,
	))
}

func (t *TopicPool) removePeer(server *p2p.Server, info *peerInfo) {
	server.RemovePeer(discover.NewNode(
		discover.NodeID(info.node.ID),
		info.node.IP,
		info.node.UDP,
		info.node.TCP,
	))
}

// StopSearch stops the closes stop
func (t *TopicPool) StopSearch() {
	if !t.SearchRunning() {
		return
	}
	if t.quit == nil {
		return
	}
	select {
	case <-t.quit:
		return
	default:
		log.Debug("stoping search", "topic", t.topic)
		close(t.quit)
	}
	t.consumerWG.Wait()
	atomic.StoreInt32(&t.running, 0)
	close(t.period)
	t.discvWG.Wait()
}
