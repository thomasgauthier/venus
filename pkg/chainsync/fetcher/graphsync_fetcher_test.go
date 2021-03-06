package fetcher_test

import (
	"bytes"
	"context"
	"fmt"
	"github.com/filecoin-project/venus/pkg/config"
	emptycid "github.com/filecoin-project/venus/pkg/testhelpers/empty_cid"
	"github.com/filecoin-project/venus/pkg/util/blockstoreutil"
	"github.com/filecoin-project/venus/pkg/vm/gas"
	blocks "github.com/ipfs/go-block-format"
	"io"
	"reflect"
	"testing"
	"time"

	"github.com/filecoin-project/go-address"
	"github.com/ipfs/go-datastore"
	dss "github.com/ipfs/go-datastore/sync"
	"github.com/ipfs/go-graphsync"
	graphsyncimpl "github.com/ipfs/go-graphsync/impl"
	gsnet "github.com/ipfs/go-graphsync/network"
	gsstoreutil "github.com/ipfs/go-graphsync/storeutil"
	bstore "github.com/ipfs/go-ipfs-blockstore"
	cbor "github.com/ipfs/go-ipld-cbor"
	format "github.com/ipfs/go-ipld-format"
	"github.com/ipld/go-ipld-prime"
	cidlink "github.com/ipld/go-ipld-prime/linking/cid"
	basicnode "github.com/ipld/go-ipld-prime/node/basic"
	"github.com/ipld/go-ipld-prime/traversal/selector"
	selectorbuilder "github.com/ipld/go-ipld-prime/traversal/selector/builder"
	"github.com/libp2p/go-libp2p-core/peer"
	mocknet "github.com/libp2p/go-libp2p/p2p/net/mock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/filecoin-project/venus/pkg/block"
	"github.com/filecoin-project/venus/pkg/chain"
	"github.com/filecoin-project/venus/pkg/chainsync/fetcher"
	"github.com/filecoin-project/venus/pkg/clock"
	"github.com/filecoin-project/venus/pkg/consensus"
	"github.com/filecoin-project/venus/pkg/constants"
	"github.com/filecoin-project/venus/pkg/discovery"
	th "github.com/filecoin-project/venus/pkg/testhelpers"
	tf "github.com/filecoin-project/venus/pkg/testhelpers/testflags"
	"github.com/filecoin-project/venus/pkg/types"
)

const visitsPerBlock = 18

func TestGraphsyncFetcher(t *testing.T) {
	tf.UnitTest(t)
	priceSched := gas.NewPricesSchedule(config.DefaultForkUpgradeParam)
	ctx := context.Background()
	fc, chainClock := clock.NewFakeChain(1234567890, 5*time.Second, time.Second, time.Now().Unix())
	bv := consensus.NewDefaultBlockValidator(chainClock, nil, nil, priceSched)
	msgV := &consensus.FakeMessageValidator{}
	syntax := consensus.WrappedSyntaxValidator{
		BlockSyntaxValidator:   bv,
		MessageSyntaxValidator: msgV,
	}

	pid0 := th.RequireIntPeerID(t, 0)
	builder := chain.NewBuilderWithDeps(t, address.Undef, &chain.FakeStateBuilder{}, chain.NewClockTimestamper(chainClock))
	keys := types.MustGenerateKeyInfo(2, 42)
	mm := types.NewMessageMaker(t, keys)
	notDecodable := &fetcher.NotDecodable{Num: 5, Message: "applesauce"}
	notDecodableBytes, err := cbor.WrapObject(notDecodable, constants.DefaultHashFunction, -1)
	require.NoError(t, err)
	notDecodableBlock, err := cbor.Decode(notDecodableBytes.RawData(), constants.DefaultHashFunction, -1)
	require.NoError(t, err)
	basicBlock, err := blocks.NewBlockWithCid(notDecodableBytes.RawData(), notDecodableBlock.Cid())
	require.NoError(t, err)
	err = builder.BlockStore().Put(basicBlock)
	require.NoError(t, err)
	alice := mm.Addresses()[0]
	bob := mm.Addresses()[1]

	ssb := selectorbuilder.NewSelectorSpecBuilder(basicnode.Prototype.Any)

	amtSelector := ssb.ExploreIndex(2,
		ssb.ExploreRecursive(selector.RecursionLimitDepth(10),
			ssb.ExploreUnion(
				ssb.ExploreIndex(1, ssb.ExploreAll(ssb.ExploreRecursiveEdge())),
				ssb.ExploreIndex(2, ssb.ExploreAll(ssb.Matcher())))))

	layer1Selector, err := ssb.ExploreIndex(block.IndexMessagesField,
		ssb.ExploreRange(0, 2, amtSelector),
	).Selector()

	require.NoError(t, err)

	recursiveSelector := func(levels int) selector.Selector {
		s, err := ssb.ExploreRecursive(selector.RecursionLimitDepth(levels), ssb.ExploreIndex(block.IndexParentsField,
			ssb.ExploreUnion(
				ssb.ExploreAll(
					ssb.ExploreIndex(block.IndexMessagesField,
						ssb.ExploreRange(0, 2, amtSelector),
					)),
				ssb.ExploreIndex(0, ssb.ExploreRecursiveEdge()),
			))).Selector()
		require.NoError(t, err)
		return s
	}

	pid1 := th.RequireIntPeerID(t, 1)
	pid2 := th.RequireIntPeerID(t, 2)

	bs := builder.BlockStore()
	genesisBs := bstore.NewBlockstore(datastore.NewMapDatastore())
	err = blockstoreutil.CopyBlockstore(ctx, bs, genesisBs)
	require.NoError(t, err)
	msgStore := builder.Mstore()
	doneAt := func(tsKey *block.TipSet) func(*block.TipSet) (bool, error) {
		return func(ts *block.TipSet) (bool, error) {
			if ts.Key().Equals(tsKey.Key()) {
				return true, nil
			}
			return false, nil
		}
	}
	withMessageBuilder := func(b *chain.BlockBuilder) {
		b.AddMessages(
			[]*types.SignedMessage{mm.NewSignedMessage(alice, 1)},
			[]*types.UnsignedMessage{&mm.NewSignedMessage(bob, 1).Message},
		)
	}
	withMessageEachBuilder := func(b *chain.BlockBuilder, i int) {
		withMessageBuilder(b)
	}

	verifyMessagesFetched := func(t *testing.T, ts *block.TipSet) {
		for i := 0; i < ts.Len(); i++ {
			blk := ts.At(i)

			// use fetcher blockstore to retrieve messages
			secpMsgs, blsMsgs, err := msgStore.LoadMetaMessages(ctx, blk.Messages)
			require.NoError(t, err)

			// get expected messages from builders block store
			expectedSecpMessages, expectedBLSMsgs, err := builder.LoadMetaMessages(ctx, blk.Messages)
			require.NoError(t, err)

			require.True(t, reflect.DeepEqual(secpMsgs, expectedSecpMessages))
			require.True(t, reflect.DeepEqual(blsMsgs, expectedBLSMsgs))
		}
	}

	loader := successLoader(ctx, builder)
	t.Run("happy path returns correct tipsets", func(t *testing.T) {
		gen := builder.Genesis()
		final := builder.BuildOn(gen, 3, withMessageEachBuilder)
		chain0 := block.NewChainInfo(pid0, pid0, final)

		bakeBs := bstore.NewBlockstore(datastore.NewMapDatastore())
		err = blockstoreutil.CopyBlockstore(ctx, genesisBs, bakeBs)
		require.NoError(t, err)
		mgs := newMockableGraphsync(ctx, bakeBs, fc, t)
		mgs.stubResponseWithLoader(pid0, layer1Selector, loader, final.Key().Cids()...)
		mgs.stubResponseWithLoader(pid0, recursiveSelector(1), loader, final.At(0).Cid())

		fetcher := fetcher.NewGraphSyncFetcher(ctx, mgs, bakeBs, syntax, fc, newFakePeerTracker(chain0))
		done := doneAt(gen)

		ts, err := fetcher.FetchTipSets(ctx, final.Key(), pid0, done)
		require.NoError(t, err, "the request completes successfully")
		mgs.verifyReceivedRequestCount(4)
		require.Equal(t, 2, len(ts), "the right number of tipsets is returned")
		require.True(t, final.Key().Equals(ts[0].Key()), "the initial tipset is correct")
		require.True(t, gen.Key().Equals(ts[1].Key()), "the remaining tipsets are correct")
	})

	t.Run("initial request fails on a block but fallback peer succeeds", func(t *testing.T) {
		gen := builder.Genesis()
		final := builder.BuildOn(gen, 3, withMessageEachBuilder)
		chain0 := block.NewChainInfo(pid0, pid0, final)
		chain1 := block.NewChainInfo(pid1, pid1, final)
		chain2 := block.NewChainInfo(pid2, pid2, final)
		pt := newFakePeerTracker(chain0, chain1, chain2)

		pid0Loader := errorOnCidsLoader(loader, final.At(1).Cid(), final.At(2).Cid())
		pid1Loader := errorOnCidsLoader(loader, final.At(2).Cid())

		bakeBs := bstore.NewBlockstore(datastore.NewMapDatastore())
		err = blockstoreutil.CopyBlockstore(ctx, genesisBs, bakeBs)
		require.NoError(t, err)
		mgs := newMockableGraphsync(ctx, bakeBs, fc, t)
		mgs.expectRequestToRespondWithLoader(pid0, layer1Selector, pid0Loader, final.Key().Cids()...)
		mgs.expectRequestToRespondWithLoader(pid1, layer1Selector, pid1Loader, final.At(1).Cid(), final.At(2).Cid())
		mgs.expectRequestToRespondWithLoader(pid2, layer1Selector, loader, final.At(2).Cid())
		mgs.expectRequestToRespondWithLoader(pid2, recursiveSelector(1), loader, final.At(0).Cid())

		fetcher := fetcher.NewGraphSyncFetcher(ctx, mgs, bakeBs, syntax, fc, pt)

		done := doneAt(gen)
		ts, err := fetcher.FetchTipSets(ctx, final.Key(), pid0, done)
		require.NoError(t, err, "the request completes successfully")
		mgs.verifyReceivedRequestCount(7)
		mgs.verifyExpectations()
		require.Equal(t, 2, len(ts), "the right number of tipsets is returned")
		require.True(t, final.Key().Equals(ts[0].Key()), "the initial tipset is correct")
		require.True(t, gen.Key().Equals(ts[1].Key()), "the remaining tipsets are correct")
	})

	t.Run("initial request fails and no other peers succeed", func(t *testing.T) {
		gen := builder.Genesis()
		final := builder.BuildOn(gen, 3, withMessageEachBuilder)
		chain0 := block.NewChainInfo(pid0, pid0, final)
		chain1 := block.NewChainInfo(pid1, pid1, final)
		chain2 := block.NewChainInfo(pid2, pid2, final)
		pt := newFakePeerTracker(chain0, chain1, chain2)

		bakeBs := bstore.NewBlockstore(datastore.NewMapDatastore())
		err = blockstoreutil.CopyBlockstore(ctx, genesisBs, bakeBs)
		require.NoError(t, err, "copy blockstore fail")
		mgs := newMockableGraphsync(ctx, bakeBs, fc, t)
		errorLoader := errorOnCidsLoader(loader, final.At(1).Cid(), final.At(2).Cid())
		mgs.expectRequestToRespondWithLoader(pid0, layer1Selector, errorLoader, final.Key().Cids()...)
		mgs.expectRequestToRespondWithLoader(pid1, layer1Selector, errorLoader, final.At(1).Cid(), final.At(2).Cid())
		mgs.expectRequestToRespondWithLoader(pid2, layer1Selector, errorLoader, final.At(1).Cid(), final.At(2).Cid())

		fetcher := fetcher.NewGraphSyncFetcher(ctx, mgs, bakeBs, syntax, fc, pt)

		done := doneAt(gen)

		ts, err := fetcher.FetchTipSets(ctx, final.Key(), pid0, done)
		mgs.verifyReceivedRequestCount(7)
		mgs.verifyExpectations()
		require.EqualError(t, err, fmt.Sprintf("fetching tipset: %s: Unable to find any untried peers", final.Key().String()))
		require.Nil(t, ts)
	})

	t.Run("requests fails because blocks are present but are missing messages", func(t *testing.T) {
		gen := builder.Genesis()
		final := builder.BuildOn(gen, 3, withMessageEachBuilder)
		chain0 := block.NewChainInfo(pid0, pid0, final)

		bakeBs := bstore.NewBlockstore(datastore.NewMapDatastore())
		err = blockstoreutil.CopyBlockstore(ctx, genesisBs, bakeBs)
		require.NoError(t, err, "copy blockstore fail")
		mgs := newMockableGraphsync(ctx, bakeBs, fc, t)
		final2Meta, err := builder.LoadTxMeta(ctx, final.At(2).Messages)
		require.NoError(t, err)
		errorOnMessagesLoader := errorOnCidsLoader(loader, final2Meta.SecpRoot)
		mgs.expectRequestToRespondWithLoader(pid0, layer1Selector, errorOnMessagesLoader, final.Key().Cids()...)

		fetcher := fetcher.NewGraphSyncFetcher(ctx, mgs, bakeBs, syntax, fc, newFakePeerTracker(chain0))

		done := doneAt(gen)
		ts, err := fetcher.FetchTipSets(ctx, final.Key(), pid0, done)
		mgs.verifyReceivedRequestCount(3)
		mgs.verifyExpectations()
		require.EqualError(t, err, fmt.Sprintf("fetching tipset: %s: Unable to find any untried peers", final.Key().String()))
		require.Nil(t, ts)
	})

	t.Run("partial response fail during recursive fetch recovers at fail point", func(t *testing.T) {
		gen := builder.Genesis()
		final := builder.BuildManyOn(5, gen, withMessageBuilder)
		chain0 := block.NewChainInfo(pid0, pid0, final)
		chain1 := block.NewChainInfo(pid1, pid1, final)
		chain2 := block.NewChainInfo(pid2, pid2, final)
		pt := newFakePeerTracker(chain0, chain1, chain2)

		blocks := make([]*block.Block, 4) // in fetch order
		prev := final.At(0)
		for i := 0; i < 4; i++ {
			parent := prev.Parents.Cids()[0]
			prev, err = builder.GetBlock(ctx, parent)
			require.NoError(t, err)
			blocks[i] = prev
		}

		bakeBs := bstore.NewBlockstore(datastore.NewMapDatastore())
		err = blockstoreutil.CopyBlockstore(ctx, genesisBs, bakeBs)
		require.NoError(t, err, "copy blockstore fail")
		mgs := newMockableGraphsync(ctx, bakeBs, fc, t)
		pid0Loader := errorOnCidsLoader(loader, blocks[3].Cid())
		mgs.expectRequestToRespondWithLoader(pid0, layer1Selector, pid0Loader, final.At(0).Cid())
		mgs.expectRequestToRespondWithLoader(pid0, recursiveSelector(1), pid0Loader, final.At(0).Cid())
		mgs.expectRequestToRespondWithLoader(pid0, recursiveSelector(4), pid0Loader, blocks[0].Cid())
		mgs.expectRequestToRespondWithLoader(pid1, recursiveSelector(4), loader, blocks[2].Cid())

		fetcher := fetcher.NewGraphSyncFetcher(ctx, mgs, bakeBs, syntax, fc, pt)

		done := func(ts *block.TipSet) (bool, error) {
			if ts.Key().Equals(gen.Key()) {
				return true, nil
			}
			return false, nil
		}

		ts, err := fetcher.FetchTipSets(ctx, final.Key(), pid0, done)
		require.NoError(t, err, "the request completes successfully")
		mgs.verifyReceivedRequestCount(4)
		mgs.verifyExpectations()
		require.Equal(t, 6, len(ts), "the right number of tipsets is returned")
		expectedTs := final
		for _, resultTs := range ts {
			require.True(t, expectedTs.Key().Equals(resultTs.Key()), "the initial tipset is correct")
			key, err := expectedTs.Parents()
			require.NoError(t, err)
			if !key.IsEmpty() {
				expectedTs, err = builder.GetTipSet(key)
				require.NoError(t, err)
			}
		}
	})

	t.Run("missing single block in multi block tip during recursive fetch", func(t *testing.T) {
		gen := builder.Genesis()
		multi := builder.BuildOn(gen, 3, withMessageEachBuilder)
		penultimate := builder.BuildManyOn(3, multi, withMessageBuilder)
		final := builder.BuildOn(penultimate, 1, withMessageEachBuilder)
		chain0 := block.NewChainInfo(pid0, pid0, final)

		bakeBs := bstore.NewBlockstore(datastore.NewMapDatastore())
		err = blockstoreutil.CopyBlockstore(ctx, genesisBs, bakeBs)
		require.NoError(t, err)
		mgs := newMockableGraphsync(ctx, bakeBs, fc, t)
		errorInMultiBlockLoader := errorOnCidsLoader(loader, multi.At(1).Cid())
		mgs.expectRequestToRespondWithLoader(pid0, layer1Selector, errorInMultiBlockLoader, final.At(0).Cid())
		mgs.expectRequestToRespondWithLoader(pid0, recursiveSelector(1), errorInMultiBlockLoader, final.At(0).Cid())
		mgs.expectRequestToRespondWithLoader(pid0, recursiveSelector(4), errorInMultiBlockLoader, penultimate.At(0).Cid())

		fetcher := fetcher.NewGraphSyncFetcher(ctx, mgs, bakeBs, syntax, fc, newFakePeerTracker(chain0))
		done := doneAt(gen)

		ts, err := fetcher.FetchTipSets(ctx, final.Key(), pid0, done)
		mgs.verifyReceivedRequestCount(3)
		mgs.verifyExpectations()
		require.EqualError(t, err, fmt.Sprintf("fetching tipset: %s: Unable to find any untried peers", multi.Key().String()))
		require.Nil(t, ts)
	})

	t.Run("missing single block in multi block tip during recursive fetch, recover through fallback", func(t *testing.T) {
		gen := builder.Genesis()
		multi := builder.BuildOn(gen, 3, withMessageEachBuilder)
		withMultiParent := builder.BuildOn(multi, 1, withMessageEachBuilder)
		penultimate := builder.BuildManyOn(2, withMultiParent, withMessageBuilder)
		final := builder.BuildOn(penultimate, 1, withMessageEachBuilder)
		chain0 := block.NewChainInfo(pid0, pid0, final)
		chain1 := block.NewChainInfo(pid1, pid1, final)
		chain2 := block.NewChainInfo(pid2, pid2, final)

		bakeBs := bstore.NewBlockstore(datastore.NewMapDatastore())
		err = blockstoreutil.CopyBlockstore(ctx, genesisBs, bakeBs)
		require.NoError(t, err)
		mgs := newMockableGraphsync(ctx, bakeBs, fc, t)
		errorInMultiBlockLoader := errorOnCidsLoader(loader, multi.At(1).Cid())
		mgs.expectRequestToRespondWithLoader(pid0, layer1Selector, errorInMultiBlockLoader, final.At(0).Cid())
		mgs.expectRequestToRespondWithLoader(pid0, recursiveSelector(1), errorInMultiBlockLoader, final.At(0).Cid())
		mgs.expectRequestToRespondWithLoader(pid0, recursiveSelector(4), errorInMultiBlockLoader, penultimate.At(0).Cid())
		mgs.expectRequestToRespondWithLoader(pid1, recursiveSelector(4), loader, withMultiParent.At(0).Cid())

		fetcher := fetcher.NewGraphSyncFetcher(ctx, mgs, bakeBs, syntax, fc, newFakePeerTracker(chain0, chain1, chain2))
		done := doneAt(gen)

		ts, err := fetcher.FetchTipSets(ctx, final.Key(), pid0, done)
		require.NoError(t, err, "the request completes successfully")
		mgs.verifyReceivedRequestCount(4)
		mgs.verifyExpectations()
		require.Equal(t, 6, len(ts), "the right number of tipsets is returned")
		expectedTs := final
		for _, resultTs := range ts {
			require.True(t, expectedTs.Key().Equals(resultTs.Key()), "the initial tipset is correct")
			key, err := expectedTs.Parents()
			require.NoError(t, err)
			if !key.IsEmpty() {
				expectedTs, err = builder.GetTipSet(key)
				require.NoError(t, err)
			}
		}
	})

	t.Run("stopping at edge heights in recursive fetch", func(t *testing.T) {
		gen := builder.Genesis()
		bakeBs := bstore.NewBlockstore(datastore.NewMapDatastore())
		err := blockstoreutil.CopyBlockstore(ctx, genesisBs, bakeBs)
		require.NoError(t, err)

		recursive16stop := builder.BuildManyOn(1, gen, withMessageBuilder)
		recursive16middle := builder.BuildManyOn(15, recursive16stop, withMessageBuilder)
		recursive4stop := builder.BuildManyOn(1, recursive16middle, withMessageBuilder)
		recursive4middle := builder.BuildManyOn(3, recursive4stop, withMessageBuilder)
		recursive1stop := builder.BuildManyOn(1, recursive4middle, withMessageBuilder)
		final := builder.BuildOn(recursive1stop, 1, withMessageEachBuilder)
		chain0 := block.NewChainInfo(pid0, pid0, final)
		nextKey := final.Key()

		for i := 1; i <= 22; i++ {
			tipset, err := builder.GetTipSet(nextKey)
			require.NoError(t, err)
			mgs := newMockableGraphsync(ctx, bakeBs, fc, t)
			mgs.expectRequestToRespondWithLoader(pid0, layer1Selector, loader, final.At(0).Cid())
			receivedRequestCount := 1
			if i > 1 {
				mgs.expectRequestToRespondWithLoader(pid0, recursiveSelector(1), loader, final.At(0).Cid())
				receivedRequestCount++
			}
			if i > 2 {
				mgs.expectRequestToRespondWithLoader(pid0, recursiveSelector(4), loader, recursive1stop.At(0).Cid())
				receivedRequestCount++
			}
			if i > 6 {
				mgs.expectRequestToRespondWithLoader(pid0, recursiveSelector(16), loader, recursive4stop.At(0).Cid())
				receivedRequestCount++
			}

			fetcher := fetcher.NewGraphSyncFetcher(ctx, mgs, bakeBs, syntax, fc, newFakePeerTracker(chain0))
			done := doneAt(tipset)

			ts, err := fetcher.FetchTipSets(ctx, final.Key(), pid0, done)
			require.NoError(t, err, "the request completes successfully")
			mgs.verifyReceivedRequestCount(receivedRequestCount)
			mgs.verifyExpectations()

			require.Equal(t, i, len(ts), "the right number of tipsets is returned")
			lastTs := ts[len(ts)-1]
			verifyMessagesFetched(t, lastTs)

			nextKey, err = tipset.Parents()
			require.NoError(t, err)
		}
	})

	t.Run("block returned with invalid syntax", func(t *testing.T) {
		bakeBs := bstore.NewBlockstore(datastore.NewMapDatastore())
		err = blockstoreutil.CopyBlockstore(ctx, genesisBs, bakeBs)
		require.NoError(t, err)
		mgs := newMockableGraphsync(ctx, bakeBs, fc, t)
		blk := simpleBlock()
		blk.Height = 1
		blk.Timestamp = uint64(chainClock.StartTimeOfEpoch(blk.Height).Unix())
		ts, _ := block.NewTipSet(blk)
		chain0 := block.NewChainInfo(pid0, pid0, ts)
		invalidSyntaxLoader := simpleLoader([]format.Node{blk.ToNode()})
		mgs.stubResponseWithLoader(pid0, layer1Selector, invalidSyntaxLoader, blk.Cid())
		fetcher := fetcher.NewGraphSyncFetcher(ctx, mgs, bakeBs, syntax, fc, newFakePeerTracker(chain0))
		done := doneAt(ts)
		fetchedTs, err := fetcher.FetchTipSets(ctx, ts.Key(), pid0, done)
		require.EqualError(t, err, fmt.Sprintf("invalid block %s: block %s has nil ticket", blk.Cid().String(), blk.Cid().String()))
		require.Nil(t, fetchedTs)
	})

	t.Run("blocks present but messages don't decode", func(t *testing.T) {
		//put msg to bs first
		metaCid, err := msgStore.StoreTxMeta(ctx, types.TxMeta{SecpRoot: notDecodableBlock.Cid(), BLSRoot: emptycid.EmptyMessagesCID})
		require.NoError(t, err)
		//copy a bs to bakebs contains genesis block and msg
		bakeBs := bstore.NewBlockstore(datastore.NewMapDatastore())
		err = blockstoreutil.CopyBlockstore(ctx, bs, bakeBs)
		require.NoError(t, err)
		mgs := newMockableGraphsync(ctx, bakeBs, fc, t)
		addrGet := types.NewForTestGetter()
		blk := requireSimpleValidBlock(t, 3, addrGet())
		blk.Messages = metaCid

		ts, _ := block.NewTipSet(blk)
		chain0 := block.NewChainInfo(pid0, pid0, ts)
		uMsg := types.NewMessageForTestGetter()()
		nd, err := uMsg.ToNode()
		require.NoError(t, err)
		notDecodableLoader := simpleLoader([]format.Node{blk.ToNode(), notDecodableBlock, nd})
		mgs.stubResponseWithLoader(pid0, layer1Selector, notDecodableLoader, blk.Cid())
		fetcher := fetcher.NewGraphSyncFetcher(ctx, mgs, bakeBs, syntax, fc, newFakePeerTracker(chain0))

		done := doneAt(ts)
		fetchedTs, err := fetcher.FetchTipSets(ctx, ts.Key(), pid0, done)
		require.EqualError(t, err, fmt.Sprintf("fetched data (cid %s) could not be decoded as an AMT: cbor input had wrong number of fields", notDecodableBlock.Cid().String()))
		require.Nil(t, fetchedTs)
	})

	t.Run("messages don't validate", func(t *testing.T) {
		gen := builder.Genesis()
		final := builder.BuildOn(gen, 1, withMessageEachBuilder)
		chain0 := block.NewChainInfo(pid0, pid0, final)

		bakeBs := bstore.NewBlockstore(datastore.NewMapDatastore())
		err = blockstoreutil.CopyBlockstore(ctx, genesisBs, bakeBs)
		require.NoError(t, err)
		mgs := newMockableGraphsync(ctx, bakeBs, fc, t)
		mgs.stubResponseWithLoader(pid0, layer1Selector, loader, final.Key().Cids()...)

		errorMv := mockSyntaxValidator{
			validateMessagesError: fmt.Errorf("Everything Failed"),
		}
		fetcher := fetcher.NewGraphSyncFetcher(ctx, mgs, bakeBs, errorMv, fc, newFakePeerTracker(chain0))
		done := doneAt(gen)

		ts, err := fetcher.FetchTipSets(ctx, final.Key(), pid0, done)
		require.Nil(t, ts)
		require.Error(t, err, "invalid messages for for message collection (cid %s)", final.At(0).Messages.String())
	})

	t.Run("hangup occurs during first layer fetch but recovers through fallback", func(t *testing.T) {
		gen := builder.Genesis()
		final := builder.BuildOn(gen, 3, withMessageEachBuilder)
		chain0 := block.NewChainInfo(pid0, pid0, final)
		chain1 := block.NewChainInfo(pid1, pid1, final)
		chain2 := block.NewChainInfo(pid2, pid2, final)
		pt := newFakePeerTracker(chain0, chain1, chain2)

		bakeBs := bstore.NewBlockstore(datastore.NewMapDatastore())
		err = blockstoreutil.CopyBlockstore(ctx, genesisBs, bakeBs)
		require.NoError(t, err)
		mgs := newMockableGraphsync(ctx, bakeBs, fc, t)
		mgs.expectRequestToRespondWithLoader(pid0, layer1Selector, loader, final.At(0).Cid())
		mgs.expectRequestToRespondWithHangupAfter(pid0, layer1Selector, loader, 0, final.At(1).Cid(), final.At(2).Cid())
		mgs.expectRequestToRespondWithLoader(pid1, layer1Selector, loader, final.At(1).Cid())
		mgs.expectRequestToRespondWithHangupAfter(pid1, layer1Selector, loader, 0, final.At(2).Cid())
		mgs.expectRequestToRespondWithLoader(pid2, layer1Selector, loader, final.At(2).Cid())
		mgs.expectRequestToRespondWithLoader(pid2, recursiveSelector(1), loader, final.At(0).Cid())

		fetcher := fetcher.NewGraphSyncFetcher(ctx, mgs, bakeBs, syntax, fc, pt)
		done := doneAt(gen)

		ts, err := fetcher.FetchTipSets(ctx, final.Key(), pid0, done)
		require.NoError(t, err, "the request completes successfully")
		mgs.verifyReceivedRequestCount(7)
		mgs.verifyExpectations()
		require.Equal(t, 2, len(ts), "the right number of tipsets is returned")
		require.True(t, final.Key().Equals(ts[0].Key()), "the initial tipset is correct")
		require.True(t, gen.Key().Equals(ts[1].Key()), "the remaining tipsets are correct")
	})

	t.Run("initial request hangs up and no other peers succeed", func(t *testing.T) {
		gen := builder.Genesis()
		final := builder.BuildOn(gen, 3, withMessageEachBuilder)
		chain0 := block.NewChainInfo(pid0, pid0, final)
		chain1 := block.NewChainInfo(pid1, pid1, final)
		chain2 := block.NewChainInfo(pid2, pid2, final)
		pt := newFakePeerTracker(chain0, chain1, chain2)

		bakeBs := bstore.NewBlockstore(datastore.NewMapDatastore())
		err = blockstoreutil.CopyBlockstore(ctx, genesisBs, bakeBs)
		require.NoError(t, err)
		mgs := newMockableGraphsync(ctx, bakeBs, fc, t)
		mgs.expectRequestToRespondWithLoader(pid0, layer1Selector, loader, final.At(0).Cid())
		mgs.expectRequestToRespondWithHangupAfter(pid0, layer1Selector, loader, 0, final.At(1).Cid(), final.At(2).Cid())
		mgs.expectRequestToRespondWithHangupAfter(pid1, layer1Selector, loader, 0, final.At(1).Cid(), final.At(2).Cid())
		mgs.expectRequestToRespondWithHangupAfter(pid2, layer1Selector, loader, 0, final.At(1).Cid(), final.At(2).Cid())

		fetcher := fetcher.NewGraphSyncFetcher(ctx, mgs, bakeBs, syntax, fc, pt)
		done := doneAt(gen)
		ts, err := fetcher.FetchTipSets(ctx, final.Key(), pid0, done)

		mgs.verifyReceivedRequestCount(7)
		mgs.verifyExpectations()
		require.EqualError(t, err, fmt.Sprintf("fetching tipset: %s: Unable to find any untried peers", final.Key().String()))
		require.Nil(t, ts)
	})

	t.Run("partial response hangs up during recursive fetch recovers at hang up point", func(t *testing.T) {
		gen := builder.Genesis()
		final := builder.BuildManyOn(5, gen, withMessageBuilder)
		chain0 := block.NewChainInfo(pid0, pid0, final)
		chain1 := block.NewChainInfo(pid1, pid1, final)
		chain2 := block.NewChainInfo(pid2, pid2, final)
		pt := newFakePeerTracker(chain0, chain1, chain2)

		blocks := make([]*block.Block, 4) // in fetch order
		prev := final.At(0)
		for i := 0; i < 4; i++ {
			parent := prev.Parents.Cids()[0]
			prev, err = builder.GetBlock(ctx, parent)
			require.NoError(t, err)
			blocks[i] = prev
		}

		bakeBs := bstore.NewBlockstore(datastore.NewMapDatastore())
		err = blockstoreutil.CopyBlockstore(ctx, genesisBs, bakeBs)
		require.NoError(t, err)
		mgs := newMockableGraphsync(ctx, bakeBs, fc, t)
		mgs.expectRequestToRespondWithLoader(pid0, layer1Selector, loader, final.At(0).Cid())
		mgs.expectRequestToRespondWithLoader(pid0, recursiveSelector(1), loader, final.At(0).Cid())
		mgs.expectRequestToRespondWithHangupAfter(pid0, recursiveSelector(4), loader, 2*visitsPerBlock, blocks[0].Cid())
		mgs.expectRequestToRespondWithLoader(pid1, recursiveSelector(4), loader, blocks[2].Cid())

		fetcher := fetcher.NewGraphSyncFetcher(ctx, mgs, bakeBs, syntax, fc, pt)

		done := func(ts *block.TipSet) (bool, error) {
			if ts.Key().Equals(gen.Key()) {
				return true, nil
			}
			return false, nil
		}

		ts, err := fetcher.FetchTipSets(ctx, final.Key(), pid0, done)

		require.NoError(t, err, "the request completes successfully")
		mgs.verifyReceivedRequestCount(4)
		mgs.verifyExpectations()
		require.Equal(t, 6, len(ts), "the right number of tipsets is returned")
		expectedTs := final
		for _, resultTs := range ts {
			require.True(t, expectedTs.Key().Equals(resultTs.Key()), "the initial tipset is correct")
			key, err := expectedTs.Parents()
			require.NoError(t, err)
			if !key.IsEmpty() {
				expectedTs, err = builder.GetTipSet(key)
				require.NoError(t, err)
			}
		}
	})

	t.Run("hangs up on single block in multi block tip during recursive fetch", func(t *testing.T) {
		gen := builder.Genesis()
		multi := builder.BuildOn(gen, 3, withMessageEachBuilder)
		penultimate := builder.BuildManyOn(3, multi, withMessageBuilder)
		final := builder.BuildOn(penultimate, 1, withMessageEachBuilder)
		chain0 := block.NewChainInfo(pid0, pid0, final)

		bakeBs := bstore.NewBlockstore(datastore.NewMapDatastore())
		err = blockstoreutil.CopyBlockstore(ctx, genesisBs, bakeBs)
		require.NoError(t, err)
		mgs := newMockableGraphsync(ctx, bakeBs, fc, t)
		mgs.expectRequestToRespondWithLoader(pid0, layer1Selector, loader, final.At(0).Cid())
		mgs.expectRequestToRespondWithLoader(pid0, recursiveSelector(1), loader, final.At(0).Cid())
		mgs.expectRequestToRespondWithHangupAfter(pid0, recursiveSelector(4), loader, 2*visitsPerBlock, penultimate.At(0).Cid())

		fetcher := fetcher.NewGraphSyncFetcher(ctx, mgs, bakeBs, syntax, fc, newFakePeerTracker(chain0))
		done := doneAt(gen)

		ts, err := fetcher.FetchTipSets(ctx, final.Key(), pid0, done)

		mgs.verifyReceivedRequestCount(3)
		mgs.verifyExpectations()
		require.EqualError(t, err, fmt.Sprintf("fetching tipset: %s: Unable to find any untried peers", multi.Key().String()))
		require.Nil(t, ts)
	})

	t.Run("hangs up on single block in multi block tip during recursive fetch, recover through fallback", func(t *testing.T) {
		gen := builder.Genesis()
		multi := builder.BuildOn(gen, 3, withMessageEachBuilder)
		withMultiParent := builder.BuildOn(multi, 1, withMessageEachBuilder)
		penultimate := builder.BuildManyOn(2, withMultiParent, withMessageBuilder)
		final := builder.BuildOn(penultimate, 1, withMessageEachBuilder)
		chain0 := block.NewChainInfo(pid0, pid0, final)
		chain1 := block.NewChainInfo(pid1, pid1, final)
		chain2 := block.NewChainInfo(pid2, pid2, final)

		bakeBs := bstore.NewBlockstore(datastore.NewMapDatastore())
		err = blockstoreutil.CopyBlockstore(ctx, genesisBs, bakeBs)
		require.NoError(t, err)
		mgs := newMockableGraphsync(ctx, bakeBs, fc, t)
		mgs.expectRequestToRespondWithLoader(pid0, layer1Selector, loader, final.At(0).Cid())
		mgs.expectRequestToRespondWithLoader(pid0, recursiveSelector(1), loader, final.At(0).Cid())
		mgs.expectRequestToRespondWithHangupAfter(pid0, recursiveSelector(4), loader, 2*visitsPerBlock, penultimate.At(0).Cid())
		mgs.expectRequestToRespondWithLoader(pid1, recursiveSelector(4), loader, withMultiParent.At(0).Cid())

		fetcher := fetcher.NewGraphSyncFetcher(ctx, mgs, bakeBs, syntax, fc, newFakePeerTracker(chain0, chain1, chain2))
		done := doneAt(gen)

		ts, err := fetcher.FetchTipSets(ctx, final.Key(), pid0, done)

		require.NoError(t, err, "the request completes successfully")
		mgs.verifyReceivedRequestCount(4)
		mgs.verifyExpectations()
		require.Equal(t, 6, len(ts), "the right number of tipsets is returned")
		expectedTs := final
		for _, resultTs := range ts {
			require.True(t, expectedTs.Key().Equals(resultTs.Key()), "the initial tipset is correct")
			key, err := expectedTs.Parents()
			require.NoError(t, err)
			if !key.IsEmpty() {
				expectedTs, err = builder.GetTipSet(key)
				require.NoError(t, err)
			}
		}
	})
}

func TestHeadersOnlyGraphsyncFetch(t *testing.T) {
	tf.UnitTest(t)
	priceSched := gas.NewPricesSchedule(config.DefaultForkUpgradeParam)
	ctx := context.Background()
	fc := clock.NewFake(time.Now())
	genTime := uint64(1234567890)
	chainClock := clock.NewChainClockFromClock(genTime, 5*time.Second, time.Second, fc)
	bv := consensus.NewDefaultBlockValidator(chainClock, nil, nil, priceSched)
	msgV := &consensus.FakeMessageValidator{}
	syntax := consensus.WrappedSyntaxValidator{
		BlockSyntaxValidator:   bv,
		MessageSyntaxValidator: msgV,
	}
	pid0 := th.RequireIntPeerID(t, 0)
	builder := chain.NewBuilderWithDeps(t, address.Undef, &chain.FakeStateBuilder{}, chain.NewClockTimestamper(chainClock))
	keys := types.MustGenerateKeyInfo(1, 42)
	mm := types.NewMessageMaker(t, keys)
	notDecodableBlock, err := cbor.WrapObject(&fetcher.NotDecodable{Num: 5, Message: "applebutter"}, constants.DefaultHashFunction, -1)
	require.NoError(t, err)

	alice := mm.Addresses()[0]

	ssb := selectorbuilder.NewSelectorSpecBuilder(basicnode.Prototype.Any)
	layer1Selector, err := ssb.Matcher().Selector()
	require.NoError(t, err)

	bs := builder.BlockStore()
	genesisBs := bstore.NewBlockstore(datastore.NewMapDatastore())
	err = blockstoreutil.CopyBlockstore(ctx, bs, genesisBs)
	require.NoError(t, err)

	recursiveSelector := func(levels int) selector.Selector {
		s, err := ssb.ExploreRecursive(selector.RecursionLimitDepth(levels), ssb.ExploreIndex(block.IndexParentsField,
			ssb.ExploreUnion(
				ssb.ExploreAll(
					ssb.Matcher(),
				),
				ssb.ExploreIndex(0, ssb.ExploreRecursiveEdge()),
			))).Selector()
		require.NoError(t, err)
		return s
	}

	doneAt := func(tsKey *block.TipSet) func(*block.TipSet) (bool, error) {
		return func(ts *block.TipSet) (bool, error) {
			if ts.Key().Equals(tsKey.Key()) {
				return true, nil
			}
			return false, nil
		}
	}
	withMessageBuilder := func(b *chain.BlockBuilder) {
		b.AddMessages(
			[]*types.SignedMessage{mm.NewSignedMessage(alice, 1)},
			[]*types.UnsignedMessage{},
		)
	}
	withMessageEachBuilder := func(b *chain.BlockBuilder, i int) {
		withMessageBuilder(b)
	}

	verifyNoMessages := func(t *testing.T, fetchBs blockstoreutil.Blockstore, ts *block.TipSet) {
		for i := 0; i < ts.Len(); i++ {
			blk := ts.At(i)
			stored, err := fetchBs.Has(blk.Messages)
			require.NoError(t, err)
			require.False(t, stored)
		}
	}

	t.Run("happy path returns correct tipsets", func(t *testing.T) {
		gen := builder.Genesis()
		bakeBs := bstore.NewBlockstore(datastore.NewMapDatastore())
		err = blockstoreutil.CopyBlockstore(ctx, genesisBs, bakeBs)
		require.NoError(t, err)
		final := builder.BuildOn(gen, 3, withMessageEachBuilder)
		chain0 := block.NewChainInfo(pid0, pid0, final)

		mgs := newMockableGraphsync(ctx, bakeBs, fc, t)
		loader := successHeadersLoader(ctx, builder)
		mgs.stubResponseWithLoader(pid0, layer1Selector, loader, final.Key().Cids()...)
		mgs.stubResponseWithLoader(pid0, recursiveSelector(1), loader, final.At(0).Cid())

		fetcher := fetcher.NewGraphSyncFetcher(ctx, mgs, bakeBs, syntax, fc, newFakePeerTracker(chain0))
		done := doneAt(gen)

		ts, err := fetcher.FetchTipSetHeaders(ctx, final.Key(), pid0, done)
		require.NoError(t, err, "the request completes successfully")
		mgs.verifyReceivedRequestCount(4)
		require.Equal(t, 2, len(ts), "the right number of tipsets is returned")
		require.True(t, final.Key().Equals(ts[0].Key()), "the initial tipset is correct")
		require.True(t, gen.Key().Equals(ts[1].Key()), "the remaining tipsets are correct")
		verifyNoMessages(t, bakeBs, ts[0])
	})

	t.Run("fetch succeeds when messages don't decode", func(t *testing.T) {
		bakeBs := bstore.NewBlockstore(datastore.NewMapDatastore())
		err = blockstoreutil.CopyBlockstore(ctx, genesisBs, bakeBs)
		require.NoError(t, err)
		mgs := newMockableGraphsync(ctx, bakeBs, fc, t)
		blk := requireSimpleValidBlock(t, 3, address.Undef)
		metaCid, err := builder.StoreTxMeta(ctx, types.TxMeta{SecpRoot: notDecodableBlock.Cid(), BLSRoot: emptycid.EmptyMessagesCID})
		require.NoError(t, err)
		blk.Messages = metaCid
		blk.Miner = types.NewForTestGetter()()
		ts, _ := block.NewTipSet(blk)
		chain0 := block.NewChainInfo(pid0, pid0, ts)
		uMsg := types.NewMessageForTestGetter()()
		nd, err := uMsg.ToNode()
		require.NoError(t, err)
		notDecodableLoader := simpleLoader([]format.Node{blk.ToNode(), notDecodableBlock, nd})
		mgs.stubResponseWithLoader(pid0, layer1Selector, notDecodableLoader, blk.Cid())
		fetcher := fetcher.NewGraphSyncFetcher(ctx, mgs, bakeBs, syntax, fc, newFakePeerTracker(chain0))

		done := doneAt(ts)
		fetchedTs, err := fetcher.FetchTipSetHeaders(ctx, ts.Key(), pid0, done)
		assert.NoError(t, err)
		require.Equal(t, 1, len(fetchedTs))
		assert.NoError(t, err)
		assert.Equal(t, ts.Key(), fetchedTs[0].Key())
	})
}

func TestRealWorldGraphsyncFetchOnlyHeaders(t *testing.T) {
	tf.IntegrationTest(t)
	ctx := context.Background()
	priceSched := gas.NewPricesSchedule(config.DefaultForkUpgradeParam)
	// setup a chain
	fc, chainClock := clock.NewFakeChain(1234567890, 5*time.Second, time.Second, time.Now().Unix())
	builder := chain.NewBuilderWithDeps(t, address.Undef, &chain.FakeStateBuilder{}, chain.NewClockTimestamper(chainClock))
	gen := builder.Genesis()

	keys := types.MustGenerateKeyInfo(2, 42)
	mm := types.NewMessageMaker(t, keys)
	alice := mm.Addresses()[0]
	bob := mm.Addresses()[1]

	// count > 64 force multiple layers in amts
	messageCount := uint64(100)

	secpMessages := make([]*types.SignedMessage, messageCount)
	blsMessages := make([]*types.UnsignedMessage, messageCount)
	for i := uint64(0); i < messageCount; i++ {
		secpMessages[i] = mm.NewSignedMessage(alice, i)
		blsMessages[i] = &mm.NewSignedMessage(bob, i).Message
	}

	tipCount := 32
	final := builder.BuildManyOn(tipCount, gen, func(b *chain.BlockBuilder) {
		b.AddMessages(secpMessages, blsMessages)
	})

	// setup network
	mn := mocknet.New(ctx)

	host1, err := mn.GenPeer()
	if err != nil {
		t.Fatal("error generating host")
	}
	host2, err := mn.GenPeer()
	if err != nil {
		t.Fatal("error generating host")
	}
	err = mn.LinkAll()
	if err != nil {
		t.Fatal("error linking hosts")
	}

	gsnet1 := gsnet.NewFromLibp2pHost(host1)

	// setup receiving peer to just record message coming in
	gsnet2 := gsnet.NewFromLibp2pHost(host2)

	// setup a graphsync fetcher and a graphsync responder

	bs := bstore.NewBlockstore(dss.MutexWrap(datastore.NewMapDatastore()))

	bv := consensus.NewDefaultBlockValidator(chainClock, nil, nil, priceSched)
	msgV := &consensus.FakeMessageValidator{}
	syntax := consensus.WrappedSyntaxValidator{BlockSyntaxValidator: bv,
		MessageSyntaxValidator: msgV,
	}
	pt := discovery.NewPeerTracker(peer.ID(""))
	pt.Track(block.NewChainInfo(host2.ID(), host2.ID(), block.UndefTipSet))

	localLoader := gsstoreutil.LoaderForBlockstore(bs)
	localStorer := gsstoreutil.StorerForBlockstore(bs)

	localGraphsync := graphsyncimpl.New(ctx, gsnet1, localLoader, localStorer)

	fetcher := fetcher.NewGraphSyncFetcher(ctx, localGraphsync, bs, syntax, fc, pt)

	remoteLoader := func(lnk ipld.Link, lnkCtx ipld.LinkContext) (io.Reader, error) {
		cid := lnk.(cidlink.Link).Cid
		b, err := builder.GetBlockstoreValue(ctx, cid)
		if err != nil {
			return nil, err
		}
		return bytes.NewBuffer(b.RawData()), nil
	}
	graphsyncimpl.New(ctx, gsnet2, remoteLoader, nil)

	tipsets, err := fetcher.FetchTipSetHeaders(ctx, final.Key(), host2.ID(), func(ts *block.TipSet) (bool, error) {
		if ts.Key().Equals(gen.Key()) {
			return true, nil
		}
		return false, nil
	})
	require.NoError(t, err)

	require.Equal(t, tipCount+1, len(tipsets))

	// Check the headers are in the store.
	// Check that the messages and receipts are NOT in the store.
	expectedTips := builder.RequireTipSets(final.Key(), tipCount+1)
	for _, ts := range expectedTips {
		stored, err := bs.Has(ts.At(0).Cid())
		require.NoError(t, err)
		assert.True(t, stored)

		stored, err = bs.Has(ts.At(0).Messages)
		require.NoError(t, err)
		assert.False(t, stored)

		stored, err = bs.Has(ts.At(0).ParentMessageReceipts)
		require.NoError(t, err)
		assert.False(t, stored)
	}
}

func TestRealWorldGraphsyncFetchAcrossNetwork(t *testing.T) {
	tf.IntegrationTest(t)
	ctx := context.Background()
	// setup a chain
	builder := chain.NewBuilder(t, address.Undef)
	keys := types.MustGenerateKeyInfo(1, 42)
	mm := types.NewMessageMaker(t, keys)
	alice := mm.Addresses()[0]
	gen := builder.Genesis()
	i := uint64(0)
	tipCount := 32
	final := builder.BuildManyOn(tipCount, gen, func(b *chain.BlockBuilder) {
		b.AddMessages(
			[]*types.SignedMessage{mm.NewSignedMessage(alice, i)},
			[]*types.UnsignedMessage{},
		)
	})

	// setup network
	mn := mocknet.New(ctx)

	host1, err := mn.GenPeer()
	if err != nil {
		t.Fatal("error generating host")
	}
	host2, err := mn.GenPeer()
	if err != nil {
		t.Fatal("error generating host")
	}
	err = mn.LinkAll()
	if err != nil {
		t.Fatal("error linking hosts")
	}

	gsnet1 := gsnet.NewFromLibp2pHost(host1)

	// setup receiving peer to just record message coming in
	gsnet2 := gsnet.NewFromLibp2pHost(host2)

	// setup a graphsync fetcher and a graphsync responder

	bs := bstore.NewBlockstore(dss.MutexWrap(datastore.NewMapDatastore()))
	bv := th.NewFakeBlockValidator()
	msgV := &consensus.FakeMessageValidator{}
	syntax := consensus.WrappedSyntaxValidator{
		BlockSyntaxValidator:   bv,
		MessageSyntaxValidator: msgV,
	}
	fc := clock.NewFake(time.Now())
	pt := discovery.NewPeerTracker(peer.ID(""))
	pt.Track(block.NewChainInfo(host2.ID(), host2.ID(), block.UndefTipSet))

	localLoader := gsstoreutil.LoaderForBlockstore(bs)
	localStorer := gsstoreutil.StorerForBlockstore(bs)

	localGraphsync := graphsyncimpl.New(ctx, gsnet1, localLoader, localStorer)
	gsFetcher := fetcher.NewGraphSyncFetcher(ctx, localGraphsync, bs, syntax, fc, pt)

	remoteLoader := func(lnk ipld.Link, lnkCtx ipld.LinkContext) (io.Reader, error) {
		cid := lnk.(cidlink.Link).Cid
		node, err := tryBlockstoreValue(ctx, builder, cid)
		if err != nil {
			return nil, err
		}
		return bytes.NewBuffer(node.RawData()), nil
	}
	otherGraphsync := graphsyncimpl.New(ctx, gsnet2, remoteLoader, nil, graphsyncimpl.RejectAllRequestsByDefault())
	otherGraphsync.RegisterIncomingRequestHook(func(p peer.ID, requestData graphsync.RequestData, hookActions graphsync.IncomingRequestHookActions) {
		_, has := requestData.Extension(fetcher.ChainsyncProtocolExtension)
		if has {
			hookActions.ValidateRequest()
		}
	})
	tipsets, err := gsFetcher.FetchTipSets(ctx, final.Key(), host2.ID(), func(ts *block.TipSet) (bool, error) {
		if ts.Key().Equals(gen.Key()) {
			return true, nil
		}
		return false, nil
	})
	require.NoError(t, err)

	require.Equal(t, tipCount+1, len(tipsets))

	// Check the headers and messages structures are in the store.
	expectedTips := builder.RequireTipSets(final.Key(), tipCount+1)
	for _, ts := range expectedTips {
		stored, err := bs.Has(ts.At(0).Cid())
		require.NoError(t, err)
		assert.True(t, stored)

		rawMeta, err := bs.Get(ts.At(0).Messages)
		require.NoError(t, err)
		var meta types.TxMeta
		err = meta.UnmarshalCBOR(bytes.NewReader(rawMeta.RawData()))
		require.NoError(t, err)

		stored, err = bs.Has(meta.SecpRoot)
		require.NoError(t, err)
		assert.True(t, stored)
	}
}
