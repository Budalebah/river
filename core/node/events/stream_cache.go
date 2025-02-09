package events

import (
	"context"
	"sync"
	"time"

	. "github.com/river-build/river/core/node/base"
	"github.com/river-build/river/core/node/config"
	"github.com/river-build/river/core/node/contracts"
	"github.com/river-build/river/core/node/crypto"
	"github.com/river-build/river/core/node/dlog"
	. "github.com/river-build/river/core/node/nodes"
	. "github.com/river-build/river/core/node/protocol"
	"github.com/river-build/river/core/node/registries"
	. "github.com/river-build/river/core/node/shared"
	"github.com/river-build/river/core/node/storage"
)

const (
	// MiniblockCandidateBatchSize keep track the max number of new miniblocks that are registered in the StreamRegistry
	// in a single transaction.
	MiniblockCandidateBatchSize = 50
)

type StreamCacheParams struct {
	Storage      storage.StreamStorage
	Wallet       *crypto.Wallet
	Riverchain   *crypto.Blockchain
	Registry     *registries.RiverRegistryContract
	StreamConfig *config.StreamConfig
}

type StreamCache interface {
	GetStream(ctx context.Context, streamId StreamId) (SyncStream, StreamView, error)
	CreateStream(ctx context.Context, streamId StreamId) (SyncStream, StreamView, error)
	ForceFlushAll(ctx context.Context)
	GetLoadedViews(ctx context.Context) []StreamView
	OnNewBlock(ctx context.Context)
}

type streamCacheImpl struct {
	params *StreamCacheParams

	// streamId -> *streamImpl
	// cache is populated by getting all streams that should be on local node from River chain.
	// streamImpl can be in unloaded state, in which case it will be loaded on first GetStream call.
	cache sync.Map

	// New miniblock production in triggered when there is new block on River chain.
	onNewBlockMutex sync.Mutex

	// registerMiniBlocksBatched is a feature gate that when set to true new mini-blocks are registered with a batched
	// transaction instead of one by one. This can be deleted once the StreamRegistry facet is updated to allow for
	// batch registrations.
	registerMiniBlocksBatched bool
}

var _ StreamCache = (*streamCacheImpl)(nil)

func NewStreamCache(
	ctx context.Context,
	params *StreamCacheParams,
	appliedBlockNum crypto.BlockNumber,
	chainMonitor crypto.ChainMonitor,
) (*streamCacheImpl, error) {
	s := &streamCacheImpl{
		params:                    params,
		registerMiniBlocksBatched: true,
	}

	streams, err := params.Registry.GetAllStreams(ctx, appliedBlockNum)
	if err != nil {
		return nil, err
	}

	// TODO: read stream state from storage and schedule required reconciliations.

	for _, stream := range streams {
		nodes := NewStreamNodes(stream.Nodes, params.Wallet.Address)
		if nodes.IsLocal() {
			s.cache.Store(stream.StreamId, &streamImpl{
				params:   params,
				streamId: stream.StreamId,
				nodes:    nodes,
			})
		}
	}

	// TODO: setup monitor for stream updates and update records accordingly.

	chainMonitor.OnBlock(func(ctx context.Context, _ crypto.BlockNumber) { s.OnNewBlock(ctx) })

	go s.cacheCleanup(ctx, params.StreamConfig.CacheExpirationPollInterval, params.StreamConfig.CacheExpiration)

	return s, nil
}

// polls the cache every pollInterval and evicts streams from the cache that have not been accessed in expiration.
func (s *streamCacheImpl) cacheCleanup(ctx context.Context, pollInterval time.Duration, expiration time.Duration) {
	log := dlog.FromCtx(ctx)

	if expiration <= 0 {
		log.Warn("stream cache cache cleanup disabled", "expiration", expiration)
		return
	}
	if pollInterval <= 0 {
		pollInterval = expiration / 10
	}

	log.Debug("stream cache cache cleanup", "expiration", expiration, "poll", pollInterval)

	for {
		select {
		case <-time.After(pollInterval):
			s.cache.Range(func(streamID, streamVal any) bool {
				if stream := streamVal.(*streamImpl); stream.tryCleanup(expiration) {
					log.Debug("stream view evicted from cache", "streamId", stream.streamId)
				}
				return true
			})
		case <-ctx.Done():
			log.Debug("stream cache cache cleanup shutdown")
			return
		}
	}
}

func (s *streamCacheImpl) tryLoadStreamRecord(ctx context.Context, streamId StreamId) (SyncStream, StreamView, error) {
	// Same code is called for GetStream and CreateStream.
	// For GetStream the fact that record is not in cache means that there is race to get it during creation:
	// Blockchain record is already created, but this fact is not reflected yet in local storage.
	// This may happen if somebody observes record allocation on blockchain and tries to get stream
	// while local storage is being initialized.
	record, _, mb, err := s.params.Registry.GetStreamWithGenesis(ctx, streamId)
	if err != nil {
		return nil, nil, err
	}

	nodes := NewStreamNodes(record.Nodes, s.params.Wallet.Address)
	if !nodes.IsLocal() {
		return nil, nil, RiverError(
			Err_INTERNAL,
			"tryLoadStreamRecord: Stream is not local",
			"streamId", streamId,
			"nodes", record.Nodes,
			"localNode", s.params.Wallet,
		)
	}

	if record.LastMiniblockNum > 0 {
		// TODO: reconcile from other nodes.
		return nil, nil, RiverError(
			Err_INTERNAL,
			"tryLoadStreamRecord: Stream is past genesis",
			"streamId",
			streamId,
			"record",
			record,
		)
	}

	stream := &streamImpl{
		params:           s.params,
		streamId:         streamId,
		nodes:            nodes,
		lastAccessedTime: time.Now(),
	}

	// Lock stream, so parallel creators have to wait for the stream to be intialized.
	stream.mu.Lock()
	defer stream.mu.Unlock()

	entry, loaded := s.cache.LoadOrStore(streamId, stream)
	if !loaded {
		// Our stream won the race, put into storage.
		err := s.params.Storage.CreateStreamStorage(ctx, streamId, mb)
		if err != nil {
			if AsRiverError(err).Code == Err_ALREADY_EXISTS {
				// Attempt to load stream from storage. Might as well do it while under lock.
				err = stream.loadInternal(ctx)
				if err == nil {
					return stream, stream.view, nil
				}
			}
			return nil, nil, err
		}

		// Successfully put data into storage, init stream view.
		view, err := MakeStreamView(&storage.ReadStreamFromLastSnapshotResult{
			StartMiniblockNumber: 0,
			Miniblocks:           [][]byte{mb},
		})
		if err != nil {
			return nil, nil, err
		}
		stream.view = view
		return stream, view, nil
	} else {
		// There was another record in the cache, use it.
		if entry == nil {
			return nil, nil, RiverError(Err_INTERNAL, "tryLoadStreamRecord: Cache corruption", "streamId", streamId)
		}
		stream = entry.(*streamImpl)
		view, err := stream.GetView(ctx)
		if err != nil {
			return nil, nil, err
		}
		return stream, view, nil
	}
}

func (s *streamCacheImpl) GetStream(ctx context.Context, streamId StreamId) (SyncStream, StreamView, error) {
	entry, _ := s.cache.Load(streamId)
	if entry == nil {
		return s.tryLoadStreamRecord(ctx, streamId)
	}
	stream := entry.(*streamImpl)

	streamView, err := stream.GetView(ctx)

	if err == nil {
		return stream, streamView, nil
	} else {
		// TODO: if stream is not present in local storage, schedule reconciliation.
		return nil, nil, err
	}
}

func (s *streamCacheImpl) CreateStream(
	ctx context.Context,
	streamId StreamId,
) (SyncStream, StreamView, error) {
	// Same logic as in GetStream: read from blockchain, create if present.
	return s.GetStream(ctx, streamId)
}

func (s *streamCacheImpl) ForceFlushAll(ctx context.Context) {
	s.cache.Range(func(key, value interface{}) bool {
		stream := value.(*streamImpl)
		stream.ForceFlush(ctx)
		return true
	})
}

func (s *streamCacheImpl) GetLoadedViews(ctx context.Context) []StreamView {
	var result []StreamView
	s.cache.Range(func(key, value interface{}) bool {
		stream := value.(*streamImpl)
		view := stream.tryGetView()
		if view != nil {
			result = append(result, view)
		}
		return true
	})
	return result
}

// OnNewBlock loops over streams and determines if it needs to produce a new mini block.
// For every stream that is eligible to produce a new mini block it creates a new mini block candidate.
// It bundles candidates in a batch.
// If the batch is full it submits the batch to the RiverRegistry#stream facet for registration and parses the resulting
// logs to determine which mini block candidate was registered and which are not. For each registered mini block
// candidate it applies the candidate to the stream.
func (s *streamCacheImpl) OnNewBlock(ctx context.Context) {
	log := dlog.FromCtx(ctx)

	// Log at level below debug, otherwise it's too chatty.
	log.Log(ctx, -8, "OnNewBlock: ENTER producing new miniblocks")

	// Try lock to have only one invocation at a time. Previous onNewBlock may still be running.
	if !s.onNewBlockMutex.TryLock() {
		return
	}

	// don't block the chain monitor
	go func() {
		defer s.onNewBlockMutex.Unlock()

		// switch over to batch commits when StreamRegistry facet is updated to allow batch sets
		if s.registerMiniBlocksBatched {
			s.onNewBlockBatch(ctx)
		} else {
			s.onNewBlockSingle(ctx)
		}

		// Log at level below debug, otherwise it's too chatty.
		log.Log(ctx, -8, "onNewBlock: EXIT produced new miniblocks")
	}()
}

// s.onNewBlockMutex must be claimed
func (s *streamCacheImpl) onNewBlockSingle(ctx context.Context) {
	s.cache.Range(func(key, value interface{}) bool {
		stream := value.(*streamImpl)
		if stream.canCreateMiniblock() {
			// TODO: use worker pool here?
			go stream.MakeMiniblock(ctx)
		}
		return true
	})
}

// s.onNewBlockMutex must be claimed
func (s *streamCacheImpl) onNewBlockBatch(ctx context.Context) {
	var (
		log        = dlog.FromCtx(ctx)
		candidates = map[StreamId]*streamImpl{}
		tasks      sync.WaitGroup
	)

	// TODO: visiting every stream here is not efficient,
	// most streams are cold. Add data structure to keep track of streams
	// that are eligible for miniblock production.
	s.cache.Range(func(key, value interface{}) bool {
		stream := value.(*streamImpl)
		if stream.canCreateMiniblock() {
			candidates[stream.streamId] = stream
			if len(candidates) == MiniblockCandidateBatchSize {
				tasks.Add(1)
				go s.processMiniblockProposalBatch(ctx, candidates, tasks.Done)
				candidates = map[StreamId]*streamImpl{}
			}
		}
		return true
	})

	s.processMiniblockProposalBatch(ctx, candidates, nil)

	tasks.Wait()

	// Log at level below debug, otherwise it's too chatty.
	log.Log(ctx, -8, "onNewBlock: EXIT produced new miniblocks")
}

func (s *streamCacheImpl) processMiniblockProposalBatch(
	ctx context.Context,
	candidates map[StreamId]*streamImpl,
	onDone func(),
) {
	if onDone != nil {
		defer onDone()
	}
	if len(candidates) == 0 {
		return
	}

	log := dlog.FromCtx(ctx)
	var err error

	miniblocks := make([]contracts.SetMiniblock, 0, len(candidates))
	proposals := map[StreamId]*MiniblockInfo{}
	for _, s := range candidates {
		// Test also creates miniblocks on demand.
		// Miniblock production code is going to be hardened to be able to handle multiple concurrent calls.
		// But this is not the case yet, to make tests stable do not attempt to create miniblock if
		// another one is already in progress.
		if !s.makeMiniblockMutex.TryLock() {
			continue
		}
		defer s.makeMiniblockMutex.Unlock()

		proposal, err := s.ProposeNextMiniblock(ctx, false)
		if err != nil {
			log.Error("processMiniblockProposalBatch: Error creating new miniblock proposal", "streamId", s.streamId, "err", err)
			continue
		}
		if proposal == nil {
			log.Debug("processMiniblockProposalBatch: No miniblock to produce", "streamId", s.streamId)
			continue
		}
		miniblocks = append(
			miniblocks,
			contracts.SetMiniblock{
				StreamId:          s.streamId,
				PrevMiniBlockHash: *proposal.headerEvent.PrevMiniblockHash,
				LastMiniblockHash: proposal.headerEvent.Hash,
				LastMiniblockNum:  uint64(proposal.Num),
				IsSealed:          false,
			},
		)
		proposals[s.streamId] = proposal
	}

	if len(miniblocks) == 0 {
		return
	}

	var success []StreamId
	// SetStreamLastMiniblock is more efficient when registering a single block
	if len(miniblocks) == 1 {
		mb := miniblocks[0]
		err = s.params.Registry.SetStreamLastMiniblock(
			ctx, mb.StreamId, mb.PrevMiniBlockHash, mb.LastMiniblockHash, mb.LastMiniblockNum, false)
		if err != nil {
			log.Error("processMiniblockProposalBatch: Error registering miniblock", "streamId", mb.StreamId, "err", err)
			return
		}
		success = append(success, mb.StreamId)
	} else {
		var failed []StreamId
		success, failed, err = s.params.Registry.SetStreamLastMiniblockBatch(ctx, miniblocks)
		if err != nil {
			log.Error("processMiniblockProposalBatch: Error registering miniblock batch", "err", err)
			return
		}
		if len(failed) > 0 {
			log.Error("processMiniblockProposalBatch: Failed to register some miniblocks", "failed", failed)
		}
	}

	for _, streamId := range success {
		err = candidates[streamId].ApplyMiniblock(ctx, proposals[streamId])
		if err != nil {
			log.Error("processMiniblockProposalBatch: Error applying miniblock", "streamId", streamId, "err", err)
		}
	}
}
