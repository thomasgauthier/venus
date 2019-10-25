package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/filecoin-project/go-filecoin/actor"
	"github.com/filecoin-project/go-filecoin/actor/builtin/initactor"
	"github.com/filecoin-project/go-filecoin/actor/builtin/miner"
	"github.com/filecoin-project/go-filecoin/actor/builtin/paymentbroker"
	"github.com/filecoin-project/go-filecoin/actor/builtin/storagemarket"
	"github.com/filecoin-project/go-filecoin/address"
	"github.com/filecoin-project/go-filecoin/block"
	"github.com/filecoin-project/go-filecoin/encoding/gen"
	"github.com/filecoin-project/go-filecoin/proofs/sectorbuilder"
	"github.com/filecoin-project/go-filecoin/protocol/hello"
	"github.com/filecoin-project/go-filecoin/protocol/retrieval"
	"github.com/filecoin-project/go-filecoin/protocol/storage/storagedeal"
	"github.com/filecoin-project/go-filecoin/types"
	logging "github.com/ipfs/go-log"
)

var base = "/tmp/encoding_gen"

// var base = "."

func main() {
	logging.SetAllLoggers(logging.LevelDebug)

	if err := gen.WriteToFile(filepath.Join(base, "actor/actor_encoding_gen.go"), gen.IpldCborTypeEncodingGenerator{}, "actor",
		actor.Actor{},            // actor/actor.go
		actor.FakeActorStorage{}, // actor/testing.go
	); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	if err := gen.WriteToFile(filepath.Join(base, "actor/builtin/initactor/initactor_encoding_gen.go"), gen.IpldCborTypeEncodingGenerator{}, "initactor",
		initactor.State{}, // actor/builtin/initactior/init.go
	); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	if err := gen.WriteToFile(filepath.Join(base, "actor/builtin/miner/miner_encoding_gen.go"), gen.IpldCborTypeEncodingGenerator{}, "miner",
		miner.State{}, // actor/builtin/miner/miner.go
		miner.Ask{},   // actor/builtin/miner/miner.go
	); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	// cbor.BigIntAtlasEntry,          // actor/built-in/miner.go XXX: atlas

	if err := gen.WriteToFile(filepath.Join(base, "actor/builtin/paymentbroker/paymentbroker_encoding_gen.go"), gen.IpldCborTypeEncodingGenerator{}, "paymentbroker",
		paymentbroker.PaymentChannel{}, // actor/builtin/paymentbroker/paymentbroker.go
	); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	if err := gen.WriteToFile(filepath.Join(base, "actor/builtin/storagemarket/storagemarket_encoding_gen.go"), gen.IpldCborTypeEncodingGenerator{}, "storagemarket",
		storagemarket.State{}, // actor/builtin/storagemarket/storagemarket.go
		// gen.TypeOpt{Value: struct{}{}, GenerateEncode: false, GenerateDecode: false}, // actor/builtin/storagemarket/storagemarket.go XXX: we should just have this in the encoding
	); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	if err := gen.WriteToFile(filepath.Join(base, "address/address_encoding_gen.go"), gen.IpldCborTypeEncodingGenerator{}, "address",
		gen.TypeOpt{Value: address.Address{}, Mode: gen.NewTypeStructMode},
	); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	if err := gen.WriteToFile(filepath.Join(base, "block/block_encoding_gen.go"), gen.IpldCborTypeEncodingGenerator{}, "block",
		block.Block{},  // block/block.go
		block.Ticket{}, // block/ticket.go
		// block.TipSetKey{}, // block/tipset_key.go XXX: custom
	); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	if err := gen.WriteToFile(filepath.Join(base, "proofs/sectorbuilder/sectorbuilder_encoding_gen.go"), gen.IpldCborTypeEncodingGenerator{}, "sectorbuilder",
		sectorbuilder.PieceInfo{}, // proofs/sectorbuilder/interface.go
	); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	if err := gen.WriteToFile(filepath.Join(base, "protocol/hello/hello_encoding_gen.go"), gen.IpldCborTypeEncodingGenerator{}, "hello",
		hello.Message{}, // protocol/hello/hello.go
	); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	if err := gen.WriteToFile(filepath.Join(base, "protocol/retrieval/retrieval_encoding_gen.go"), gen.IpldCborTypeEncodingGenerator{}, "retrieval",
		retrieval.RetrievePieceRequest{},  // protocol/retrieval/types.go
		retrieval.RetrievePieceResponse{}, // protocol/retrieval/types.go
		retrieval.RetrievePieceChunk{},    // protocol/retrieval/types.go
	); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	// storage.dealsAwaitingSeal{}, // protocol/storage/deals_awaiting_seal.go XXX: private struct

	if err := gen.WriteToFile(filepath.Join(base, "protocol/storage/storagedeal/storagedeal_encoding_gen.go"), gen.IpldCborTypeEncodingGenerator{}, "storagedeal",
		storagedeal.PaymentInfo{},    // protocol/storage/storagedeal/types.go
		storagedeal.Proposal{},       // protocol/storage/storagedeal/types.go
		storagedeal.SignedProposal{}, // protocol/storage/storagedeal/types.go
		storagedeal.Response{},       // protocol/storage/storagedeal/types.go
		storagedeal.SignedResponse{}, // protocol/storage/storagedeal/types.go
		storagedeal.ProofInfo{},      // protocol/storage/storagedeal/types.go
		storagedeal.QueryRequest{},   // protocol/storage/storagedeal/types.go
		storagedeal.Deal{},           // protocol/storage/storagedeal/types.go
	); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	if err := gen.WriteToFile(filepath.Join(base, "types/types_encoding_gen.go"), gen.IpldCborTypeEncodingGenerator{}, "types",
		// types.AttoFIL{}, // types/atto_file.go XXX: custom
		// types.BlockHeight{}, // types/block_height.go XXX: custom
		// types.BytesAmount{}, // types/bytes_amount.go XXX: custom
		// types.ChannelID{}, // types/channel_id.go XXX: custom
		types.Commitments{}, // types/commitments.go
		types.FaultSet{},    // types/fault_set.go
		// types.IntSet{},      // types/intset.go XXX: custom
		types.KeyInfo{},        // types/keyinfo.go
		types.MessageReceipt{}, // types/message_receipts.go
		// types.MessageCollection{}, // types/message.go // XXX: array
		// types.ReceiptCollection{}, // types/message.go // XXX: array
		types.UnsignedMessage{}, // types/message.go
		types.TxMeta{},          // types/message.go
		types.Predicate{},       // types/payment_voucher.go
		types.PaymentVoucher{},  // types/payment_voucher.go
		types.SignedMessage{},   // types/signed_message.go
		// types.SignedMessageCollection{}, // types/signed_message.go // XXX: array
		// types.Uint64(0),   // types/uint64.go XXX: CUSTOM
	); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}