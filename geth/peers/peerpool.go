package peers

import (
	"errors"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/discv5"

	"github.com/status-im/status-go/geth/params"
)

var (
	// ErrDiscv5NotRunning returned when pool is started but discover v5 is not running or not enabled.
	ErrDiscv5NotRunning = errors.New("Discovery v5 is not running")
)

const (
	// expirationPeriod is a amount of time while peer is considered as a connectable
	expirationPeriod = 60 * time.Minute
	// DefaultFastSync is a recommended value for aggressive peers search.
	DefaultFastSync = 3 * time.Second
	// DefaultSlowSync is a recommended value for slow (background) peers search.
	DefaultSlowSync = 30 * time.Minute
)

// NewPeerPool creates instance of PeerPool
func NewPeerPool(config map[discv5.Topic]params.Limits, fastSync, slowSync time.Duration, cache *Cache, stopOnMax bool) *PeerPool {
	return &PeerPool{
		config:    config,
		fastSync:  fastSync,
		slowSync:  slowSync,
		cache:     cache,
		stopOnMax: stopOnMax,
	}
}

type peerInfo struct {
	// discoveredTime last time when node was found by v5
	discoveredTime mclock.AbsTime
	// connected is true if node is added as a static peer
	connected bool
	requested bool

	node *discv5.Node
}

// PeerPool manages discovered peers and connects them to p2p server
type PeerPool struct {
	// config can be set only once per pool life cycle
	config    map[discv5.Topic]params.Limits
	fastSync  time.Duration
	slowSync  time.Duration
	cache     *Cache
	stopOnMax bool

	mu                 sync.RWMutex
	topics             []*TopicPool
	serverSubscription event.Subscription
	quit               chan struct{}

	wg sync.WaitGroup
}

// Start creates discovery search query for each topic and consumes peers found in that topic
// in separate loop.
func (p *PeerPool) Start(server *p2p.Server) error {
	if server.DiscV5 == nil {
		return ErrDiscv5NotRunning
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.quit = make(chan struct{})
	p.topics = make([]*TopicPool, 0, len(p.config))
	for topic, limits := range p.config {
		topicPool := NewTopicPool(topic, limits, p.slowSync, p.fastSync)
		if err := topicPool.StartSearch(server); err != nil {
			return err
		}
		p.topics = append(p.topics, topicPool)
	}

	events := make(chan *p2p.PeerEvent, 20)
	p.serverSubscription = server.SubscribeEvents(events)
	p.wg.Add(1)
	go func() {
		p.handleServerPeers(server, events)
		p.wg.Done()
	}()
	return nil
}

func (p *PeerPool) handleServerPeers(server *p2p.Server, events <-chan *p2p.PeerEvent) {
	var (
		toSearch    []*TopicPool
		retryDiscv5 <-chan time.Time
	)
	runListener := func() {
		if server.DiscV5 == nil {
			ntab, err := StartDiscv5(server)
			if err != nil {
				log.Error("starting discv5 failed", "error", err)
				retryDiscv5 = time.After(2 * time.Second)
				return
			}
			log.Debug("restarted discovery from peer pool")
			server.DiscV5 = ntab
		}
		for _, t := range toSearch {
			_ = t.StartSearch(server)
		}
		toSearch = nil
	}

	for {
		select {
		case <-p.quit:
			return
		case <-retryDiscv5:
			runListener()
		case event := <-events:
			if event.Type == p2p.PeerEventTypeDrop {
				p.mu.Lock()
				log.Debug("confirm peer dropped", "ID", event.Peer)
				for _, t := range p.topics {
					// if dropped peer is ignored by a topic pool we should ignore it too
					_, ignored := t.ConfirmDropped(server, event.Peer, event.Error)
					// if it was min and one peer is dropped then current connections are below limit
					log.Debug("search", "topic", t.topic, "below min", t.BelowMin(), "ignored", ignored)
					if t.BelowMin() && !ignored {
						toSearch = append(toSearch, t)
					}
				}
				if len(toSearch) != 0 && p.stopOnMax {
					runListener()
				}
				p.mu.Unlock()
			} else if event.Type == p2p.PeerEventTypeAdd {
				p.mu.Lock()
				total := 0
				log.Debug("confirm peer added", "ID", event.Peer)
				for _, t := range p.topics {
					t.ConfirmAdded(server, event.Peer)
					if p.stopOnMax && t.MaxReached() {
						total++
						t.StopSearch()
					}
				}
				if p.stopOnMax && total == len(p.config) {
					log.Debug("closing discv5 connection", "server", server.Self())
					server.DiscV5.Close()
					server.DiscV5 = nil
				}
				p.mu.Unlock()
			}
		}
	}
}

// Stop closes pool quit channel and all channels that are watched by search queries
// and waits till all goroutines will exit.
func (p *PeerPool) Stop() {
	// pool wasn't started
	if p.quit == nil {
		return
	}
	select {
	case <-p.quit:
		return
	default:
		log.Debug("started closing peer pool")
		close(p.quit)
	}
	p.serverSubscription.Unsubscribe()
	for _, t := range p.topics {
		t.StopSearch()
	}
	p.wg.Wait()
}
