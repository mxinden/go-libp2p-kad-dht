// Package dual provides an implementaiton of a split or "dual" dht, where two parallel instances
// are maintained for the global internet and the local LAN respectively.
package dual

import (
	"context"
	"sync"

	dht "github.com/libp2p/go-libp2p-kad-dht"

	"github.com/ipfs/go-cid"
	ci "github.com/libp2p/go-libp2p-core/crypto"
	"github.com/libp2p/go-libp2p-core/host"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/libp2p/go-libp2p-core/protocol"
	"github.com/libp2p/go-libp2p-core/routing"
	kb "github.com/libp2p/go-libp2p-kbucket"
	helper "github.com/libp2p/go-libp2p-routing-helpers"
	ma "github.com/multiformats/go-multiaddr"

	"github.com/hashicorp/go-multierror"
)

// DHT implements the routing interface to provide two concrete DHT implementationts for use
// in IPFS that are used to support both global network users and disjoint LAN usecases.
type DHT struct {
	WAN *dht.IpfsDHT
	LAN *dht.IpfsDHT
}

// LanExtension is used to differentiate local protocol requests from those on the WAN DHT.
const LanExtension protocol.ID = "/lan"

// Assert that IPFS assumptions about interfaces aren't broken. These aren't a
// guarantee, but we can use them to aid refactoring.
var (
	_ routing.ContentRouting = (*DHT)(nil)
	_ routing.Routing        = (*DHT)(nil)
	_ routing.PeerRouting    = (*DHT)(nil)
	_ routing.PubKeyFetcher  = (*DHT)(nil)
	_ routing.ValueStore     = (*DHT)(nil)
)

// New creates a new DualDHT instance. Options provided are forwarded on to the two concrete
// IpfsDHT internal constructions, modulo additional options used by the Dual DHT to enforce
// the LAN-vs-WAN distinction.
// Note: query or routing table functional options provided as arguments to this function
// will be overriden by this constructor.
func New(ctx context.Context, h host.Host, options ...dht.Option) (*DHT, error) {
	wanOpts := append(options,
		dht.QueryFilter(dht.PublicQueryFilter),
		dht.RoutingTableFilter(dht.PublicRoutingTableFilter),
	)
	wan, err := dht.New(ctx, h, wanOpts...)
	if err != nil {
		return nil, err
	}

	// Unless overridden by user supplied options, the LAN DHT should default
	// to 'AutoServer' mode.
	lanOpts := append(options,
		dht.ProtocolExtension(LanExtension),
		dht.QueryFilter(dht.PrivateQueryFilter),
		dht.RoutingTableFilter(dht.PrivateRoutingTableFilter),
	)
	if wan.Mode() != dht.ModeClient {
		lanOpts = append(lanOpts, dht.Mode(dht.ModeServer))
	}
	lan, err := dht.New(ctx, h, lanOpts...)
	if err != nil {
		return nil, err
	}

	impl := DHT{wan, lan}
	return &impl, nil
}

// Close closes the DHT context.
func (dht *DHT) Close() error {
	return combineErrors(dht.WAN.Close(), dht.LAN.Close())
}

// WANActive returns true when the WAN DHT is active (has peers).
func (dht *DHT) WANActive() bool {
	return dht.WAN.RoutingTable().Size() > 0
}

// Provide adds the given cid to the content routing system.
func (dht *DHT) Provide(ctx context.Context, key cid.Cid, announce bool) error {
	if dht.WANActive() {
		return dht.WAN.Provide(ctx, key, announce)
	}
	return dht.LAN.Provide(ctx, key, announce)
}

// FindProvidersAsync searches for peers who are able to provide a given key
func (dht *DHT) FindProvidersAsync(ctx context.Context, key cid.Cid, count int) <-chan peer.AddrInfo {
	reqCtx, cancel := context.WithCancel(ctx)
	outCh := make(chan peer.AddrInfo)
	wanCh := dht.WAN.FindProvidersAsync(reqCtx, key, count)
	lanCh := dht.LAN.FindProvidersAsync(reqCtx, key, count)
	zeroCount := (count == 0)
	go func() {
		defer cancel()
		defer close(outCh)

		found := make(map[peer.ID]struct{}, count)
		var pi peer.AddrInfo
		for (zeroCount || count > 0) && (wanCh != nil || lanCh != nil) {
			var ok bool
			select {
			case pi, ok = <-wanCh:
				if !ok {
					wanCh = nil
					continue
				}
			case pi, ok = <-lanCh:
				if !ok {
					lanCh = nil
					continue
				}
			}
			// already found
			if _, ok = found[pi.ID]; ok {
				continue
			}

			select {
			case outCh <- pi:
				found[pi.ID] = struct{}{}
				count--
			case <-ctx.Done():
				return
			}
		}
	}()
	return outCh
}

// FindPeer searches for a peer with given ID
// Note: with signed peer records, we can change this to short circuit once either DHT returns.
func (dht *DHT) FindPeer(ctx context.Context, pid peer.ID) (peer.AddrInfo, error) {
	var wg sync.WaitGroup
	wg.Add(2)
	var wanInfo, lanInfo peer.AddrInfo
	var wanErr, lanErr error
	go func() {
		defer wg.Done()
		wanInfo, wanErr = dht.WAN.FindPeer(ctx, pid)
	}()
	go func() {
		defer wg.Done()
		lanInfo, lanErr = dht.LAN.FindPeer(ctx, pid)
	}()

	wg.Wait()

	// Combine addresses. Try to avoid doing unnecessary work while we're at
	// it. Note: We're ignoring the errors for now as many of our DHT
	// commands can return both a result and an error.
	ai := peer.AddrInfo{ID: pid}
	if len(wanInfo.Addrs) == 0 {
		ai.Addrs = lanInfo.Addrs
	} else if len(lanInfo.Addrs) == 0 {
		ai.Addrs = wanInfo.Addrs
	} else {
		// combine addresses
		deduped := make(map[string]ma.Multiaddr, len(wanInfo.Addrs)+len(lanInfo.Addrs))
		for _, addr := range wanInfo.Addrs {
			deduped[string(addr.Bytes())] = addr
		}
		for _, addr := range lanInfo.Addrs {
			deduped[string(addr.Bytes())] = addr
		}
		ai.Addrs = make([]ma.Multiaddr, 0, len(deduped))
		for _, addr := range deduped {
			ai.Addrs = append(ai.Addrs, addr)
		}
	}

	// If one of the commands succeeded, don't return an error.
	if wanErr == nil || lanErr == nil {
		return ai, nil
	}

	// Otherwise, return what we have _and_ return the error.
	return ai, combineErrors(wanErr, lanErr)
}

func combineErrors(erra, errb error) error {
	// if the errors are the same, just return one.
	if erra == errb {
		return erra
	}

	// If one of the errors is a kb lookup failure (no peers in routing
	// table), return the other.
	if erra == kb.ErrLookupFailure {
		return errb
	} else if errb == kb.ErrLookupFailure {
		return erra
	}
	return multierror.Append(erra, errb).ErrorOrNil()
}

// Bootstrap allows callers to hint to the routing system to get into a
// Boostrapped state and remain there.
func (dht *DHT) Bootstrap(ctx context.Context) error {
	erra := dht.WAN.Bootstrap(ctx)
	errb := dht.LAN.Bootstrap(ctx)
	return combineErrors(erra, errb)
}

// PutValue adds value corresponding to given Key.
func (dht *DHT) PutValue(ctx context.Context, key string, val []byte, opts ...routing.Option) error {
	if dht.WANActive() {
		return dht.WAN.PutValue(ctx, key, val, opts...)
	}
	return dht.LAN.PutValue(ctx, key, val, opts...)
}

// GetValue searches for the value corresponding to given Key.
func (d *DHT) GetValue(ctx context.Context, key string, opts ...routing.Option) ([]byte, error) {
	lanCtx, cancelLan := context.WithCancel(ctx)
	defer cancelLan()

	var (
		lanVal    []byte
		lanErr    error
		lanWaiter sync.WaitGroup
	)
	lanWaiter.Add(1)
	go func() {
		defer lanWaiter.Done()
		lanVal, lanErr = d.LAN.GetValue(lanCtx, key, opts...)
	}()

	wanVal, wanErr := d.WAN.GetValue(ctx, key, opts...)
	if wanErr == nil {
		cancelLan()
	}
	lanWaiter.Wait()
	if wanErr == nil {
		return wanVal, nil
	}
	if lanErr == nil {
		return lanVal, nil
	}
	return nil, combineErrors(wanErr, lanErr)
}

// SearchValue searches for better values from this value
func (dht *DHT) SearchValue(ctx context.Context, key string, opts ...routing.Option) (<-chan []byte, error) {
	p := helper.Parallel{Routers: []routing.Routing{dht.WAN, dht.LAN}, Validator: dht.WAN.Validator}
	return p.SearchValue(ctx, key, opts...)
}

// GetPublicKey returns the public key for the given peer.
func (dht *DHT) GetPublicKey(ctx context.Context, pid peer.ID) (ci.PubKey, error) {
	p := helper.Parallel{Routers: []routing.Routing{dht.WAN, dht.LAN}, Validator: dht.WAN.Validator}
	return p.GetPublicKey(ctx, pid)
}
