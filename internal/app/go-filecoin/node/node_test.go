package node_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/filecoin-project/go-filecoin/internal/app/go-filecoin/node"
	"github.com/filecoin-project/go-filecoin/internal/app/go-filecoin/node/test"
	"github.com/filecoin-project/go-filecoin/internal/pkg/config"
	"github.com/filecoin-project/go-filecoin/internal/pkg/proofs/verification"
	"github.com/filecoin-project/go-filecoin/internal/pkg/repo"
	th "github.com/filecoin-project/go-filecoin/internal/pkg/testhelpers"
	tf "github.com/filecoin-project/go-filecoin/internal/pkg/testhelpers/testflags"
)

func TestNodeConstruct(t *testing.T) {
	tf.UnitTest(t)
	ctx := context.Background()
	builder := test.NewNodeBuilder(t)
	builder.WithGenesisInit(th.DefaultGenesis)
	builder.WithBuilderOpt(node.FakeProofVerifierBuilderOpts()...)
	nd := builder.Build(ctx)
	assert.NotNil(t, nd.Host)

	nd.Stop(context.Background())
}

func TestNodeNetworking(t *testing.T) {
	tf.UnitTest(t)

	ctx := context.Background()
	builder := test.NewNodeBuilder(t)
	builder.WithGenesisInit(th.DefaultGenesis)
	builder.WithBuilderOpt(node.FakeProofVerifierBuilderOpts()...)
	nds := builder.BuildMany(ctx, 2)
	nd1, nd2 := nds[0], nds[1]

	pinfo := peer.AddrInfo{
		ID:    nd2.Host().ID(),
		Addrs: nd2.Host().Addrs(),
	}

	err := nd1.Host().Connect(ctx, pinfo)
	assert.NoError(t, err)

	nd1.Stop(ctx)
	nd2.Stop(ctx)
}

func TestConnectsToBootstrapNodes(t *testing.T) {
	tf.UnitTest(t)

	t.Run("no bootstrap nodes no problem", func(t *testing.T) {
		ctx := context.Background()

		r := repo.NewInMemoryRepo()
		r.Config().Swarm.Address = "/ip4/0.0.0.0/tcp/0"

		require.NoError(t, node.Init(ctx, r, th.DefaultGenesis))
		r.Config().Bootstrap.Addresses = []string{}
		opts, err := node.OptionsFromRepo(r)
		require.NoError(t, err)

		nd, err := node.New(ctx, opts...)
		require.NoError(t, err)
		assert.NoError(t, nd.Start(ctx))
		defer nd.Stop(ctx)
	})

	t.Run("connects to bootstrap nodes", func(t *testing.T) {
		ctx := context.Background()

		// These are two bootstrap nodes we'll connect to.
		builder := test.NewNodeBuilder(t)
		builder.WithGenesisInit(th.DefaultGenesis)
		builder.WithBuilderOpt(node.FakeProofVerifierBuilderOpts()...)
		nds := builder.BuildMany(ctx, 2)
		node.StartNodes(t, nds)
		nd1, nd2 := nds[0], nds[1]

		// Gotta be a better way to do this?
		peer1 := fmt.Sprintf("%s/ipfs/%s", nd1.Host().Addrs()[0].String(), nd1.Host().ID().Pretty())
		peer2 := fmt.Sprintf("%s/ipfs/%s", nd2.Host().Addrs()[0].String(), nd2.Host().ID().Pretty())

		// Create a node with the nodes above as bootstrap nodes.
		r := repo.NewInMemoryRepo()
		r.Config().Swarm.Address = "/ip4/0.0.0.0/tcp/0"

		require.NoError(t, node.Init(ctx, r, th.DefaultGenesis))
		r.Config().Bootstrap.Addresses = []string{peer1, peer2}

		opts, err := node.OptionsFromRepo(r)
		require.NoError(t, err)
		nd, err := node.New(ctx, opts...)
		require.NoError(t, err)
		nd.Discovery.Bootstrapper.MinPeerThreshold = 2
		nd.Discovery.Bootstrapper.Period = 10 * time.Millisecond
		assert.NoError(t, nd.Start(ctx))
		defer nd.Stop(ctx)

		// Ensure they're connected.
		connected := false
		// poll until we are connected, to avoid flaky tests
		for i := 0; i <= 30; i++ {
			l1 := len(nd.Host().Network().ConnsToPeer(nd1.Host().ID()))
			l2 := len(nd.Host().Network().ConnsToPeer(nd2.Host().ID()))

			connected = l1 == 1 && l2 == 1
			if connected {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}

		assert.True(t, connected, "failed to connect")
	})
}

func TestNodeInit(t *testing.T) {
	tf.UnitTest(t)

	ctx := context.Background()
	builder := test.NewNodeBuilder(t)
	builder.WithGenesisInit(th.DefaultGenesis)
	builder.WithBuilderOpt(node.FakeProofVerifierBuilderOpts()...)
	builder.WithBuilderOpt(node.OfflineMode(true))

	nd := builder.Build(ctx)

	assert.NoError(t, nd.Start(ctx))

	assert.NotEqual(t, 0, nd.PorcelainAPI.ChainHeadKey().Len())
	nd.Stop(ctx)
}

func TestNodeStartMining(t *testing.T) {
	t.Skip("Skip pending storage market integration #3731")
	tf.UnitTest(t)

	ctx := context.Background()

	seed := node.MakeChainSeed(t, node.TestGenCfg)
	builder := test.NewNodeBuilder(t)
	builder.WithInitOpt(node.PeerKeyOpt(node.PeerKeys[0]))
	builder.WithGenesisInit(seed.GenesisInitFunc)
	minerNode := builder.Build(ctx)

	seed.GiveKey(t, minerNode, 0)
	seed.GiveMiner(t, minerNode, 0) // TODO: update to accommodate new go-fil-markets integration
	// Start mining give error for fail to get miner actor from the heaviest tipset stateroot
	assert.Contains(t, minerNode.StartMining(ctx).Error(), "failed to setup mining")

	assert.NoError(t, minerNode.Start(ctx))

	t.Run("Start/Stop/Start results in a MiningScheduler that is started", func(t *testing.T) {
		assert.NoError(t, minerNode.StartMining(ctx))
		defer minerNode.StopMining(ctx)
		assert.True(t, minerNode.BlockMining.MiningScheduler.IsStarted())
		minerNode.StopMining(ctx)
		assert.False(t, minerNode.BlockMining.MiningScheduler.IsStarted())
		assert.NoError(t, minerNode.StartMining(ctx))
		assert.True(t, minerNode.BlockMining.MiningScheduler.IsStarted())
	})

	t.Run("Start + Start gives an error message saying mining is already started", func(t *testing.T) {
		assert.NoError(t, minerNode.StartMining(ctx))
		defer minerNode.StopMining(ctx)
		err := minerNode.StartMining(ctx)
		assert.Error(t, err, "node is already mining")
	})
}

func TestOptionWithError(t *testing.T) {
	tf.UnitTest(t)

	ctx := context.Background()
	r := repo.NewInMemoryRepo()
	assert.NoError(t, node.Init(ctx, r, th.DefaultGenesis))

	opts, err := node.OptionsFromRepo(r)
	assert.NoError(t, err)

	scaryErr := errors.New("i am an error grrrr")
	errOpt := func(c *node.Builder) error {
		return scaryErr
	}

	opts = append(opts, errOpt)

	_, err = node.New(ctx, opts...)
	assert.Error(t, err, scaryErr)

}

func TestNodeConfig(t *testing.T) {
	tf.UnitTest(t)

	defaultCfg := config.NewDefaultConfig()

	// fake mining
	verifier := &verification.FakeVerifier{
		VerifyPoStValid: true,
	}

	configBlockTime := 99

	builderOptions := []node.BuilderOpt{
		node.VerifierConfigOption(verifier),
		node.BlockTime(time.Duration(configBlockTime)),
	}

	initOpts := []node.InitOpt{}

	builder := test.NewNodeBuilder(t)
	builder.WithGenesisInit(th.DefaultGenesis)
	builder.WithInitOpt(initOpts...)
	builder.WithBuilderOpt(builderOptions...)
	builder.WithBuilderOpt(node.OfflineMode(true))

	n := builder.Build(context.Background())
	cfg := n.Repo.Config()

	assert.Equal(t, true, n.OfflineMode)
	assert.Equal(t, defaultCfg.Mining, cfg.Mining)
	assert.Equal(t, &config.SwarmConfig{
		Address: "/ip4/127.0.0.1/tcp/0",
	}, cfg.Swarm)
}
