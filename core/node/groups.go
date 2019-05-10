package node

import (
	"context"
	"errors"
	"fmt"
	"time"

	blockstore "github.com/ipfs/go-ipfs-blockstore"
	"github.com/ipfs/go-ipfs-config"
	util "github.com/ipfs/go-ipfs-util"
	peer "github.com/libp2p/go-libp2p-peer"
	"github.com/libp2p/go-libp2p-peerstore/pstoremem"
	pubsub "github.com/libp2p/go-libp2p-pubsub"

	"github.com/ipfs/go-ipfs/core/node/libp2p"
	"github.com/ipfs/go-ipfs/p2p"
	"github.com/ipfs/go-ipfs/provider"
	"github.com/ipfs/go-ipfs/repo"
	"github.com/ipfs/go-ipfs/reprovide"

	offline "github.com/ipfs/go-ipfs-exchange-offline"
	offroute "github.com/ipfs/go-ipfs-routing/offline"
	"github.com/ipfs/go-path/resolver"
	uio "github.com/ipfs/go-unixfs/io"
	"go.uber.org/fx"
)

var BaseLibP2P = fx.Options(
	fx.Provide(libp2p.PNet),
	fx.Provide(libp2p.ConnectionManager),
	fx.Provide(libp2p.DefaultTransports),

	fx.Provide(libp2p.Host),

	fx.Provide(libp2p.DiscoveryHandler),

	fx.Invoke(libp2p.PNetChecker),
)

func LibP2P(bcfg *BuildCfg, cfg *config.Config) fx.Option {
	// parse ConnMgr config

	grace := config.DefaultConnMgrGracePeriod
	low := config.DefaultConnMgrHighWater
	high := config.DefaultConnMgrHighWater

	connmgr := fx.Options()

	if cfg.Swarm.ConnMgr.Type != "none" {
		switch cfg.Swarm.ConnMgr.Type {
		case "":
			// 'default' value is the basic connection manager
			break
		case "basic":
			var err error
			grace, err = time.ParseDuration(cfg.Swarm.ConnMgr.GracePeriod)
			if err != nil {
				return fx.Error(fmt.Errorf("parsing Swarm.ConnMgr.GracePeriod: %s", err))
			}

			low = cfg.Swarm.ConnMgr.LowWater
			high = cfg.Swarm.ConnMgr.HighWater
		default:
			return fx.Error(fmt.Errorf("unrecognized ConnMgr.Type: %q", cfg.Swarm.ConnMgr.Type))
		}

		connmgr = fx.Provide(libp2p.ConnectionManager(low, high, grace))
	}

	// parse PubSub config

	ps := fx.Options()
	if bcfg.getOpt("pubsub") || bcfg.getOpt("ipnsps") {
		var pubsubOptions []pubsub.Option
		if cfg.Pubsub.DisableSigning {
			pubsubOptions = append(pubsubOptions, pubsub.WithMessageSigning(false))
		}

		if cfg.Pubsub.StrictSignatureVerification {
			pubsubOptions = append(pubsubOptions, pubsub.WithStrictSignatureVerification(true))
		}

		switch cfg.Pubsub.Router {
		case "":
			fallthrough
		case "floodsub":
			ps = fx.Provide(libp2p.FloodSub(pubsubOptions...))
		case "gossipsub":
			ps = fx.Provide(libp2p.GossipSub(pubsubOptions...))
		default:
			return fx.Error(fmt.Errorf("unknown pubsub router %s", cfg.Pubsub.Router))
		}
	}

	// Gather all the options

	opts := fx.Options(
		BaseLibP2P,

		fx.Provide(libp2p.AddrFilters(cfg.Swarm.AddrFilters)),
		fx.Invoke(libp2p.SetupDiscovery(cfg.Discovery.MDNS.Enabled, cfg.Discovery.MDNS.Interval)),
		fx.Provide(libp2p.AddrsFactory(cfg.Addresses.Announce, cfg.Addresses.NoAnnounce)),
		fx.Provide(libp2p.SmuxTransport(bcfg.getOpt("mplex"))),
		fx.Provide(libp2p.Relay(cfg.Swarm.DisableRelay, cfg.Swarm.EnableRelayHop)),
		fx.Invoke(libp2p.StartListening(cfg.Addresses.Swarm)),

		fx.Provide(libp2p.Security(!bcfg.DisableEncryptedConnections, cfg.Experimental.PreferTLS)),

		fx.Provide(libp2p.Routing),
		fx.Provide(libp2p.BaseRouting),
		maybeProvide(libp2p.PubsubRouter, bcfg.getOpt("ipnsps")),

		maybeProvide(libp2p.BandwidthCounter, !cfg.Swarm.DisableBandwidthMetrics),
		maybeProvide(libp2p.NatPortMap, !cfg.Swarm.DisableNatPortMap),
		maybeProvide(libp2p.AutoRealy, cfg.Swarm.EnableAutoRelay),
		maybeProvide(libp2p.QUIC, cfg.Experimental.QUIC),
		maybeInvoke(libp2p.AutoNATService(cfg.Experimental.QUIC), cfg.Swarm.EnableAutoNATService),
		connmgr,
		ps,
	)

	return opts
}

// Storage groups units which setup datastore based persistence and blockstore layers
func Storage(bcfg *BuildCfg, cfg *config.Config) fx.Option {
	cacheOpts := blockstore.DefaultCacheOpts()
	cacheOpts.HasBloomFilterSize = cfg.Datastore.BloomFilterSize
	if !bcfg.Permanent {
		cacheOpts.HasBloomFilterSize = 0
	}

	finalBstore := fx.Provide(GcBlockstoreCtor)
	if cfg.Experimental.FilestoreEnabled || cfg.Experimental.UrlstoreEnabled {
		finalBstore = fx.Provide(FilestoreBlockstoreCtor)
	}

	return fx.Options(
		fx.Provide(repo.Repo.Config),
		fx.Provide(repo.Repo.Datastore),
		fx.Provide(BaseBlockstoreCtor(cacheOpts, bcfg.NilRepo, cfg.Datastore.HashOnRead)),
		finalBstore,
	)
}

// Identity groups units providing cryptographic identity
func Identity(cfg *config.Config) fx.Option {
	// PeerID

	cid := cfg.Identity.PeerID
	if cid == "" {
		return fx.Error(errors.New("identity was not set in config (was 'ipfs init' run?)"))
	}
	if len(cid) == 0 {
		return fx.Error(errors.New("no peer ID in config! (was 'ipfs init' run?)"))
	}

	id, err := peer.IDB58Decode(cid)
	if err != nil {
		return fx.Error(fmt.Errorf("peer ID invalid: %s", err))
	}

	// Private Key

	if cfg.Identity.PrivKey == "" {
		return fx.Options( // No PK (usually in tests)
			fx.Provide(PeerID(id)),
			fx.Provide(pstoremem.NewPeerstore),
		)
	}

	sk, err := cfg.Identity.DecodePrivateKey("passphrase todo!")
	if err != nil {
		return fx.Error(err)
	}

	return fx.Options( // Full identity
		fx.Provide(PeerID(id)),
		fx.Provide(PrivateKey(sk)),
		fx.Provide(pstoremem.NewPeerstore),

		fx.Invoke(libp2p.PstoreAddSelfKeys),
	)
}

// IPNS groups namesys related units
var IPNS = fx.Options(
	fx.Provide(RecordValidator),
)

// Providers groups units managing provider routing records
func Providers(cfg *config.Config) fx.Option {
	reproviderInterval := kReprovideFrequency
	if cfg.Reprovider.Interval != "" {
		dur, err := time.ParseDuration(cfg.Reprovider.Interval)
		if err != nil {
			return fx.Error(err)
		}

		reproviderInterval = dur
	}

	var keyProvider fx.Option
	switch cfg.Reprovider.Strategy {
	case "all":
		fallthrough
	case "":
		keyProvider = fx.Provide(reprovide.NewBlockstoreProvider)
	case "roots":
		keyProvider = fx.Provide(reprovide.NewPinnedProvider(true))
	case "pinned":
		keyProvider = fx.Provide(reprovide.NewPinnedProvider(false))
	default:
		return fx.Error(fmt.Errorf("unknown reprovider strategy '%s'", cfg.Reprovider.Strategy))
	}

	return fx.Options(
		fx.Provide(ProviderQueue),
		fx.Provide(ProviderCtor),
		fx.Provide(ReproviderCtor(reproviderInterval)),
		keyProvider,

		fx.Invoke(Reprovider),
	)
}

// Online groups online-only units
func Online(bcfg *BuildCfg, cfg *config.Config) fx.Option {

	// Namesys params

	ipnsCacheSize := cfg.Ipns.ResolveCacheSize
	if ipnsCacheSize == 0 {
		ipnsCacheSize = DefaultIpnsCacheSize
	}
	if ipnsCacheSize < 0 {
		return fx.Error(fmt.Errorf("cannot specify negative resolve cache size"))
	}

	// Republisher params

	var repubPeriod, recordLifetime time.Duration

	if cfg.Ipns.RepublishPeriod != "" {
		d, err := time.ParseDuration(cfg.Ipns.RepublishPeriod)
		if err != nil {
			return fx.Error(fmt.Errorf("failure to parse config setting IPNS.RepublishPeriod: %s", err))
		}

		if !util.Debug && (d < time.Minute || d > (time.Hour*24)) {
			return fx.Error(fmt.Errorf("config setting IPNS.RepublishPeriod is not between 1min and 1day: %s", d))
		}

		repubPeriod = d
	}

	if cfg.Ipns.RecordLifetime != "" {
		d, err := time.ParseDuration(cfg.Ipns.RecordLifetime)
		if err != nil {
			return fx.Error(fmt.Errorf("failure to parse config setting IPNS.RecordLifetime: %s", err))
		}

		recordLifetime = d
	}

	return fx.Options(
		fx.Provide(OnlineExchange),
		fx.Provide(Namesys(ipnsCacheSize)),

		fx.Invoke(IpnsRepublisher(repubPeriod, recordLifetime)),

		fx.Provide(p2p.New),

		LibP2P(bcfg, cfg),
		Providers(cfg),
	)
}

// Offline groups offline alternatives to Online units
var Offline = fx.Options(
	fx.Provide(offline.Exchange),
	fx.Provide(Namesys(0)),
	fx.Provide(offroute.NewOfflineRouter),
	fx.Provide(provider.NewOfflineProvider),
)

// Core groups basic IPFS services
var Core = fx.Options(
	fx.Provide(BlockService),
	fx.Provide(Dag),
	fx.Provide(resolver.NewBasicResolver),
	fx.Provide(Pinning),
	fx.Provide(Files),
)

func Networked(bcfg *BuildCfg, cfg *config.Config) fx.Option {
	if bcfg.Online {
		return Online(bcfg, cfg)
	}
	return Offline
}

// IPFS builds a group of fx Options based on the passed BuildCfg
func IPFS(ctx context.Context, bcfg *BuildCfg) fx.Option {
	if bcfg == nil {
		bcfg = new(BuildCfg)
	}

	bcfgOpts, cfg := bcfg.options(ctx)
	if cfg == nil {
		return bcfgOpts // error
	}

	// TEMP: setting global sharding switch here
	uio.UseHAMTSharding = cfg.Experimental.ShardingEnabled

	return fx.Options(
		bcfgOpts,

		fx.Provide(baseProcess),

		Storage(bcfg, cfg),
		Identity(cfg),
		IPNS,
		Networked(bcfg, cfg),

		Core,
	)
}

/*

// ipfsNode, err := New(...core.Option) (*core.API, error)
// var _ iface.CoreAPI = ipfsNode
// var _ *core.Node = ipfsNode.Node() // use for low-level access, a bit like .Request() in go-ipfs-http-client

// TODO: auto client mode? (like fallback-ipfs-shell), or should we keep this separate?

New() // new with defaults (offline)

New(Online()) // new online node

New(Ctx(ctx)) // with context

New(Repo(r)) // with repo, use in-repo config

New(Repo(r), Blockstore(mybstore)) // with repo, use repo config, override blockstore

import nodep2p "github.com/ipfs/go-ipfs/core/node/libp2p"
New(Repo(r), Online(LibP2P(nodep2p.RelayHop(false)))) // with repo, use repo config, force no hop

New(Repo(r, Config(cfg))) // with repo, override config

New(Invoke(funcToFxInvoke))
New(Provide(funcToFxProvide))

- Provide can't override existing stuff, use special functions like the ones
  above for that
  - Doing this would either require rather deep changes in uber/dig
  - It wouldn't be typesafe at all (if we'd change some type and users didn't notice,
    their stuff would break)
- It's flexible enough to take advantage of DI
- Doesn't expose fx on the fnterface (well, it exposes lifecycles, and might be
  quite specific, but still provides us with easier migration path if we ever need one)

 */

