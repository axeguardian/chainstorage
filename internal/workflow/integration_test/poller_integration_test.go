package integration

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/suite"
	"go.temporal.io/sdk/worker"
	"go.uber.org/fx"
	"go.uber.org/zap"

	"github.com/coinbase/chainstorage/internal/blockchain/client"
	"github.com/coinbase/chainstorage/internal/blockchain/endpoints"
	"github.com/coinbase/chainstorage/internal/blockchain/jsonrpc"
	"github.com/coinbase/chainstorage/internal/blockchain/parser"
	"github.com/coinbase/chainstorage/internal/blockchain/restapi"
	"github.com/coinbase/chainstorage/internal/cadence"
	"github.com/coinbase/chainstorage/internal/config"
	"github.com/coinbase/chainstorage/internal/dlq"
	"github.com/coinbase/chainstorage/internal/s3"
	"github.com/coinbase/chainstorage/internal/storage/blobstorage"
	"github.com/coinbase/chainstorage/internal/storage/metastorage"
	storage_utils "github.com/coinbase/chainstorage/internal/storage/utils"
	"github.com/coinbase/chainstorage/internal/utils/pointer"
	"github.com/coinbase/chainstorage/internal/utils/testapp"
	"github.com/coinbase/chainstorage/internal/utils/testutil"
	"github.com/coinbase/chainstorage/internal/workflow"
	"github.com/coinbase/chainstorage/protos/coinbase/c3/common"
	api "github.com/coinbase/chainstorage/protos/coinbase/chainstorage"
)

type PollerIntegrationTestSuite struct {
	backfillerDependentTestSuite
}

type pollerDependencies struct {
	fx.In
	Poller          *workflow.Poller
	BlobStorage     blobstorage.BlobStorage
	MetaStorage     metastorage.MetaStorage
	Parser          parser.Parser
	MasterEndpoints endpoints.EndpointProvider `name:"master"`
	SlaveEndpoints  endpoints.EndpointProvider `name:"slave"`
}

type pollerTestParam struct {
	blockchain                     common.Blockchain
	network                        common.Network
	startHeight                    uint64
	tag                            uint32
	failover                       bool
	transactionIndexingParallelism int
	consensusValidation            *bool
	consensusValidationMuted       *bool
}

func TestIntegrationPollerTestSuite(t *testing.T) {
	suite.Run(t, new(PollerIntegrationTestSuite))
}

func (s *PollerIntegrationTestSuite) TestPollerIntegration() {
	tag := uint32(1)
	startHeight := uint64(17035140)
	endHeight := uint64(17035145)
	s.backfillData(startHeight, endHeight, tag, common.Blockchain_BLOCKCHAIN_ETHEREUM, common.Network_NETWORK_ETHEREUM_MAINNET)
	s.testPoller(&pollerTestParam{
		startHeight:              endHeight,
		tag:                      tag,
		failover:                 false,
		consensusValidation:      pointer.Ref(true),
		consensusValidationMuted: pointer.Ref(false),
	})
}

func (s *PollerIntegrationTestSuite) TestPollerIntegration_SessionEnabled() {
	tag := uint32(1)
	// endHeight - startHeight must be larger than irreversible_distance (400).
	startHeight := uint64(15373070)
	endHeight := uint64(15373488)
	s.backfillData(startHeight, endHeight, tag, common.Blockchain_BLOCKCHAIN_POLYGON, common.Network_NETWORK_POLYGON_MAINNET)
	s.testPoller_SessionEnabled(&pollerTestParam{
		startHeight:              endHeight,
		tag:                      tag,
		consensusValidation:      pointer.Ref(true),
		consensusValidationMuted: pointer.Ref(false),
	})
}

type testConfig struct {
	blockChain            common.Blockchain
	network               common.Network
	tag                   uint32
	backfillerStartHeight uint64
	backfillerEndHeight   uint64
	pollerStartHeight     uint64
}

func (s *PollerIntegrationTestSuite) TestPollerIntegration_UseFailoverEndpoints() {
	s.T().Skip("disable the test due to no support of failover cluster in dev")
	testConfigs := []*testConfig{
		{
			blockChain:            common.Blockchain_BLOCKCHAIN_ETHEREUM,
			network:               common.Network_NETWORK_ETHEREUM_MAINNET,
			tag:                   1,
			backfillerStartHeight: 17035140,
			backfillerEndHeight:   17035145,
			pollerStartHeight:     17035145,
		},
	}

	for _, cfg := range testConfigs {
		s.backfillData(cfg.backfillerStartHeight, cfg.backfillerEndHeight, cfg.tag, cfg.blockChain, cfg.network)
		s.testPollerWithFailoverEndpoints(cfg)
	}
}

func (s *PollerIntegrationTestSuite) TestPollerIntegration_WithFailover() {
	s.T().Skip("disable the test due to no support of failover cluster in dev")
	tag := uint32(1)
	startHeight := uint64(17035140)
	endHeight := uint64(17035145)
	s.backfillData(startHeight, endHeight, tag, common.Blockchain_BLOCKCHAIN_ETHEREUM, common.Network_NETWORK_ETHEREUM_MAINNET)
	s.testPoller(&pollerTestParam{
		blockchain:  common.Blockchain_BLOCKCHAIN_ETHEREUM,
		network:     common.Network_NETWORK_ETHEREUM_MAINNET,
		startHeight: endHeight,
		tag:         tag,
		failover:    true,
	})
}

func (s *PollerIntegrationTestSuite) TestPollerIntegration_WithFastSync() {
	tag := uint32(2)
	startHeight := uint64(204_800_230)
	endHeight := uint64(204_800_240)
	s.backfillData(startHeight, endHeight, tag, common.Blockchain_BLOCKCHAIN_SOLANA, common.Network_NETWORK_SOLANA_MAINNET)
	s.testPoller(&pollerTestParam{
		blockchain:  common.Blockchain_BLOCKCHAIN_SOLANA,
		network:     common.Network_NETWORK_SOLANA_MAINNET,
		startHeight: endHeight,
		tag:         tag,
		failover:    false,
	})
}

func (s *PollerIntegrationTestSuite) TestPollerIntegration_WithTransactionIndexing() {
	tag := uint32(2)
	s.T().Skip("disable the test as transaction indexing no longer can finish in time with recent blocks")
	startHeight := uint64(10_287_714)
	endHeight := uint64(10_287_724)
	s.backfillData(startHeight, endHeight, tag, common.Blockchain_BLOCKCHAIN_SOLANA, common.Network_NETWORK_SOLANA_MAINNET)
	s.testPollerWithTransactionIndexing(&pollerTestParam{
		blockchain:                     common.Blockchain_BLOCKCHAIN_SOLANA,
		network:                        common.Network_NETWORK_SOLANA_MAINNET,
		startHeight:                    endHeight,
		tag:                            tag,
		failover:                       false,
		transactionIndexingParallelism: 20,
	})
}

func (s *PollerIntegrationTestSuite) testPoller(param *pollerTestParam) {
	startHeight := param.startHeight
	tag := param.tag
	failover := param.failover
	consensusValidation := param.consensusValidation
	consensusValidationMuted := param.consensusValidationMuted

	maxBlocksToSync := uint64(10)
	checkpointSize := uint64(2)

	require := testutil.Require(s.T())
	cfg, err := config.New(config.WithBlockchain(param.blockchain), config.WithNetwork(param.network))
	require.NoError(err)
	cfg.Workflows.Poller.CheckpointSize = checkpointSize
	cfg.Workflows.Poller.MaxBlocksToSyncPerCycle = maxBlocksToSync
	cfg.Workflows.Poller.FailoverEnabled = failover
	cfg.Workflows.Poller.LivenessCheckEnabled = true

	pollerDeps := &pollerDependencies{}

	pollerEnv := cadence.NewTestEnv(s)
	pollerEnv.SetTestTimeout(10 * time.Minute)
	pollerEnv.SetWorkerOptions(worker.Options{
		EnableSessionWorker: true,
	})

	app := testapp.New(
		s.T(),
		testapp.WithFunctional(),
		fx.Provide(func() metastorage.MetaStorage { return s.backfillDependencies.MetaStorage }),
		fx.Provide(func() blobstorage.BlobStorage { return s.backfillDependencies.BlobStorage }),
		cadence.WithTestEnv(pollerEnv),
		testapp.WithConfig(cfg),
		workflow.Module,
		client.Module,
		jsonrpc.Module,
		restapi.Module,
		s3.Module,
		parser.Module,
		dlq.Module,
		fx.Populate(pollerDeps),
	)
	defer app.Close()

	_, err = pollerDeps.Poller.Execute(context.Background(), &workflow.PollerRequest{
		Tag:                      tag,
		MaxBlocksToSync:          maxBlocksToSync,
		Parallelism:              4,
		Failover:                 failover,
		ConsensusValidation:      consensusValidation,
		ConsensusValidationMuted: consensusValidationMuted,
	})

	require.NotNil(err)
	require.True(workflow.IsContinueAsNewError(err))

	for i := startHeight; i < startHeight+maxBlocksToSync*checkpointSize; i++ {
		app.Logger().Info("verifying blocks", zap.Uint64("height", i))
		metadata, err := pollerDeps.MetaStorage.GetBlockByHeight(context.Background(), tag, i)
		require.NoError(err)

		require.Equal(tag, metadata.Tag)
		require.Equal(i, metadata.Height)
		require.Equal(i-1, metadata.ParentHeight)
		require.NotEmpty(metadata.Hash)
		require.NotEmpty(metadata.ParentHash)
		require.NotEmpty(metadata.ObjectKeyMain)
		require.Equal(storage_utils.GetCompressionType(metadata.ObjectKeyMain), api.Compression_GZIP)
		require.False(metadata.Skipped)
		require.NotNil(metadata.Timestamp)

		rawBlock, err := pollerDeps.BlobStorage.Download(context.Background(), metadata)
		require.NoError(err)
		require.Equal(metadata.Tag, rawBlock.Metadata.Tag)
		require.Equal(metadata.Hash, rawBlock.Metadata.Hash)
		require.Equal(metadata.ParentHash, rawBlock.Metadata.ParentHash)
		require.Equal(metadata.Height, rawBlock.Metadata.Height)
		require.Equal(metadata.ParentHeight, rawBlock.Metadata.ParentHeight)
		require.NotEmpty(rawBlock.Metadata.ObjectKeyMain)
		require.Equal(storage_utils.GetCompressionType(rawBlock.Metadata.ObjectKeyMain), api.Compression_GZIP)
		require.False(rawBlock.Metadata.Skipped)
	}
}

func (s *PollerIntegrationTestSuite) testPollerWithTransactionIndexing(param *pollerTestParam) {
	startHeight := param.startHeight
	tag := param.tag
	failover := param.failover

	maxBlocksToSync := uint64(10)
	checkpointSize := uint64(2)

	require := testutil.Require(s.T())
	cfg, err := config.New(config.WithBlockchain(param.blockchain), config.WithNetwork(param.network))
	require.NoError(err)
	cfg.Workflows.Poller.CheckpointSize = checkpointSize
	cfg.Workflows.Poller.MaxBlocksToSyncPerCycle = maxBlocksToSync
	cfg.Workflows.Poller.FailoverEnabled = failover

	pollerDeps := &pollerDependencies{}

	pollerEnv := cadence.NewTestEnv(s)
	pollerEnv.SetTestTimeout(10 * time.Minute)
	pollerEnv.SetWorkerOptions(worker.Options{
		EnableSessionWorker: true,
	})

	app := testapp.New(
		s.T(),
		testapp.WithFunctional(),
		fx.Provide(func() metastorage.MetaStorage { return s.backfillDependencies.MetaStorage }),
		fx.Provide(func() blobstorage.BlobStorage { return s.backfillDependencies.BlobStorage }),
		cadence.WithTestEnv(pollerEnv),
		testapp.WithConfig(cfg),
		workflow.Module,
		client.Module,
		jsonrpc.Module,
		restapi.Module,
		s3.Module,
		parser.Module,
		dlq.Module,
		fx.Populate(pollerDeps),
	)
	defer app.Close()

	_, err = pollerDeps.Poller.Execute(context.Background(), &workflow.PollerRequest{
		Tag:                          tag,
		MaxBlocksToSync:              maxBlocksToSync,
		Parallelism:                  4,
		Failover:                     failover,
		TransactionsWriteParallelism: param.transactionIndexingParallelism,
	})

	require.NotNil(err)
	require.True(workflow.IsContinueAsNewError(err))

	for i := startHeight; i < startHeight+maxBlocksToSync*checkpointSize; i++ {
		app.Logger().Info("verifying blocks", zap.Uint64("height", i))
		metadata, err := pollerDeps.MetaStorage.GetBlockByHeight(context.Background(), tag, i)
		require.NoError(err)

		require.Equal(tag, metadata.Tag)
		require.Equal(i, metadata.Height)
		require.Equal(i-1, metadata.ParentHeight)
		require.NotEmpty(metadata.Hash)
		require.NotEmpty(metadata.ParentHash)
		require.NotEmpty(metadata.ObjectKeyMain)
		require.Equal(storage_utils.GetCompressionType(metadata.ObjectKeyMain), api.Compression_GZIP)
		require.False(metadata.Skipped)

		rawBlock, err := pollerDeps.BlobStorage.Download(context.Background(), metadata)
		require.NoError(err)
		require.Equal(metadata.Tag, rawBlock.Metadata.Tag)
		require.Equal(metadata.Hash, rawBlock.Metadata.Hash)
		require.Equal(metadata.ParentHash, rawBlock.Metadata.ParentHash)
		require.Equal(metadata.Height, rawBlock.Metadata.Height)
		require.Equal(metadata.ParentHeight, rawBlock.Metadata.ParentHeight)
		require.NotEmpty(rawBlock.Metadata.ObjectKeyMain)
		require.Equal(storage_utils.GetCompressionType(rawBlock.Metadata.ObjectKeyMain), api.Compression_GZIP)
		require.False(rawBlock.Metadata.Skipped)
		require.NotEmpty(rawBlock.TransactionMetadata)
		require.Greater(len(rawBlock.TransactionMetadata.Transactions), 10)

		transactions, err := pollerDeps.MetaStorage.GetTransaction(context.Background(), tag, "GhmVaP6unThZvqh5Su9hBJVNA4eFtMcRyBzFR79h4Y7WSRcnbA3ryVWLuC55pMGqSwyuUBj55ranzVYe9vaA4Vt")
		require.NoError(err)
		require.NotNil(transactions)
		require.Equal(uint64(10287734), transactions[0].BlockNumber)
		require.Equal("2YDhuzQVafibpUxnvJzkEpSNAznUb3iwkGLetk97ckGN", transactions[0].BlockHash)
		require.Equal(tag, transactions[0].BlockTag)
	}
}

func (s *PollerIntegrationTestSuite) testPoller_SessionEnabled(param *pollerTestParam) {
	maxBlocksToSync := uint64(10)
	checkpointSize := uint64(2)

	startHeight := param.startHeight
	tag := param.tag
	consensusValidation := param.consensusValidation
	consensusValidationMuted := param.consensusValidationMuted

	require := testutil.Require(s.T())
	cfg, err := config.New(
		config.WithBlockchain(common.Blockchain_BLOCKCHAIN_POLYGON),
		config.WithNetwork(common.Network_NETWORK_POLYGON_MAINNET),
	)
	require.NoError(err)
	cfg.Workflows.Poller.CheckpointSize = checkpointSize
	cfg.Workflows.Poller.MaxBlocksToSyncPerCycle = maxBlocksToSync

	pollerDeps := &pollerDependencies{}

	pollerEnv := cadence.NewTestEnv(s)
	pollerEnv.SetTestTimeout(10 * time.Minute)
	pollerEnv.SetWorkerOptions(worker.Options{
		EnableSessionWorker: true,
	})

	app := testapp.New(
		s.T(),
		testapp.WithFunctional(),
		fx.Provide(func() metastorage.MetaStorage { return s.backfillDependencies.MetaStorage }),
		fx.Provide(func() blobstorage.BlobStorage { return s.backfillDependencies.BlobStorage }),
		cadence.WithTestEnv(pollerEnv),
		testapp.WithConfig(cfg),
		workflow.Module,
		client.Module,
		jsonrpc.Module,
		restapi.Module,
		s3.Module,
		parser.Module,
		dlq.Module,
		fx.Populate(pollerDeps),
	)
	defer app.Close()

	_, err = pollerDeps.Poller.Execute(context.Background(), &workflow.PollerRequest{
		Tag:                      tag,
		MaxBlocksToSync:          maxBlocksToSync,
		Parallelism:              4,
		ConsensusValidation:      consensusValidation,
		ConsensusValidationMuted: consensusValidationMuted,
	})

	require.NotNil(err)
	require.True(workflow.IsContinueAsNewError(err))

	for i := startHeight; i < startHeight+maxBlocksToSync*checkpointSize; i++ {
		app.Logger().Info("verifying blocks", zap.Uint64("height", i))
		metadata, err := pollerDeps.MetaStorage.GetBlockByHeight(context.Background(), tag, i)
		require.NoError(err)

		require.Equal(tag, metadata.Tag)
		require.Equal(i, metadata.Height)
		require.Equal(i-1, metadata.ParentHeight)
		require.NotEmpty(metadata.Hash)
		require.NotEmpty(metadata.ParentHash)
		require.NotEmpty(metadata.ObjectKeyMain)
		require.Equal(storage_utils.GetCompressionType(metadata.ObjectKeyMain), api.Compression_GZIP)
		require.False(metadata.Skipped)

		rawBlock, err := pollerDeps.BlobStorage.Download(context.Background(), metadata)
		require.NoError(err)
		require.Equal(metadata.Tag, rawBlock.Metadata.Tag)
		require.Equal(metadata.Hash, rawBlock.Metadata.Hash)
		require.Equal(metadata.ParentHash, rawBlock.Metadata.ParentHash)
		require.Equal(metadata.Height, rawBlock.Metadata.Height)
		require.Equal(metadata.ParentHeight, rawBlock.Metadata.ParentHeight)
		require.NotEmpty(rawBlock.Metadata.ObjectKeyMain)
		require.Equal(storage_utils.GetCompressionType(rawBlock.Metadata.ObjectKeyMain), api.Compression_GZIP)
		require.False(rawBlock.Metadata.Skipped)
	}
}

func (s *PollerIntegrationTestSuite) testPollerWithFailoverEndpoints(testCfg *testConfig) {
	maxBlocksToSync := uint64(2)
	checkpointSize := uint64(2)

	require := testutil.Require(s.T())
	cfg, err := config.New(
		config.WithBlockchain(testCfg.blockChain),
		config.WithNetwork(testCfg.network),
	)
	require.NoError(err)
	cfg.Workflows.Poller.CheckpointSize = checkpointSize
	cfg.Workflows.Poller.MaxBlocksToSyncPerCycle = maxBlocksToSync
	cfg.Chain.Client.Master.EndpointGroup.UseFailover = true
	cfg.Chain.Client.Slave.EndpointGroup.UseFailover = true

	pollerDeps := &pollerDependencies{}

	pollerEnv := cadence.NewTestEnv(s)
	pollerEnv.SetTestTimeout(10 * time.Minute)
	pollerEnv.SetWorkerOptions(worker.Options{
		EnableSessionWorker: true,
	})

	app := testapp.New(
		s.T(),
		testapp.WithFunctional(),
		fx.Provide(func() metastorage.MetaStorage { return s.backfillDependencies.MetaStorage }),
		fx.Provide(func() blobstorage.BlobStorage { return s.backfillDependencies.BlobStorage }),
		cadence.WithTestEnv(pollerEnv),
		testapp.WithConfig(cfg),
		workflow.Module,
		client.Module,
		jsonrpc.Module,
		restapi.Module,
		s3.Module,
		parser.Module,
		dlq.Module,
		fx.Populate(pollerDeps),
	)
	defer app.Close()

	masterEndpointsFailover := cfg.Chain.Client.Master.EndpointGroup.EndpointsFailover
	masterEndpointsFailoverPtr := make([]*config.Endpoint, len(masterEndpointsFailover))
	for i := range masterEndpointsFailover {
		masterEndpointsFailoverPtr[i] = &masterEndpointsFailover[i]
	}
	slaveEndpointsFailover := cfg.Chain.Client.Slave.EndpointGroup.EndpointsFailover
	slaveEndpointsFailoverPtr := make([]*config.Endpoint, len(slaveEndpointsFailover))
	for i := range slaveEndpointsFailover {
		slaveEndpointsFailoverPtr[i] = &slaveEndpointsFailover[i]
	}

	ctx := context.Background()
	require.ElementsMatch(masterEndpointsFailoverPtr, getEndpointConfigs(ctx, pollerDeps.MasterEndpoints))
	require.ElementsMatch(slaveEndpointsFailoverPtr, getEndpointConfigs(ctx, pollerDeps.SlaveEndpoints))

	_, err = pollerDeps.Poller.Execute(ctx, &workflow.PollerRequest{
		Tag:             testCfg.tag,
		MaxBlocksToSync: maxBlocksToSync,
		Parallelism:     4,
	})

	require.NotNil(err)
	require.True(workflow.IsContinueAsNewError(err))

	for i := testCfg.pollerStartHeight; i < testCfg.pollerStartHeight+maxBlocksToSync*checkpointSize; i++ {
		app.Logger().Info("verifying blocks", zap.Uint64("height", i))
		metadata, err := pollerDeps.MetaStorage.GetBlockByHeight(ctx, testCfg.tag, i)
		require.NoError(err)

		require.Equal(testCfg.tag, metadata.Tag)
		require.Equal(i, metadata.Height)
		require.Equal(i-1, metadata.ParentHeight)
		require.NotEmpty(metadata.Hash)
		require.NotEmpty(metadata.ParentHash)
		require.NotEmpty(metadata.ObjectKeyMain)
		require.False(metadata.Skipped)
		require.NotNil(metadata.Timestamp)

		rawBlock, err := pollerDeps.BlobStorage.Download(ctx, metadata)
		require.NoError(err)
		require.Equal(metadata.Tag, rawBlock.Metadata.Tag)
		require.Equal(metadata.Hash, rawBlock.Metadata.Hash)
		require.Equal(metadata.ParentHash, rawBlock.Metadata.ParentHash)
		require.Equal(metadata.Height, rawBlock.Metadata.Height)
		require.Equal(metadata.ParentHeight, rawBlock.Metadata.ParentHeight)
		require.NotEmpty(rawBlock.Metadata.ObjectKeyMain)
		require.False(rawBlock.Metadata.Skipped)
	}
}

func getEndpointConfigs(ctx context.Context, endpointProvider endpoints.EndpointProvider) []*config.Endpoint {
	endpoints := endpointProvider.GetActiveEndpoints(ctx)
	res := make([]*config.Endpoint, len(endpoints))
	for i, endpoint := range endpoints {
		res[i] = endpoint.Config
	}
	return res
}
