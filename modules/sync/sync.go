package sync

import (
	"fmt"
	"log"
	"strings"
	"github.com/spf13/viper"

	sdk "github.com/cosmos/cosmos-sdk"
	"github.com/cosmos/cosmos-sdk/modules/coin"
	"github.com/cosmos/cosmos-sdk/modules/nonce"
	"github.com/cosmos/cosmos-sdk/client/commands"
	"github.com/tendermint/go-wire/data"

	ctypes "github.com/tendermint/tendermint/rpc/core/types"

	"time"
	rpcclient "github.com/tendermint/tendermint/rpc/client"
	"context"
	"encoding/hex"
	"github.com/tendermint/tendermint/types"
	"github.com/spf13/cast"

	"github.com/irisnet/iris-explorer/modules/store"
	"github.com/irisnet/iris-explorer/modules/stake"
	"github.com/irisnet/iris-explorer/modules/tools"
)

const (
	MgoUrl     = "mgo-url"
	Subscriber = "iris-explorer"
	WithSync   = "with-sync"
)

//var (
//	SyncCmd = &cobra.Command{
//		Use:  "sync",
//		Long: `sync iris-hub tx data to mongodb`,
//		RunE: func(cmd *cobra.Command, args []string) error {
//			return StartWatch()
//		},
//	}
//)

var syncOver = false

func initServer(){
	url := viper.GetString(MgoUrl)
	store.Mgo.Init(url)
	block, err := store.Mgo.QueryLastedBlock()

	if err != nil {
		//初始化配置表
		block = store.SyncBlock{
			Height: 1,
		}
		store.Mgo.Save(block)
	}
}

func processSync(c rpcclient.Client) {
	//开始漏单查询
	go sync(c)
}

func StartWatch() error {
	initServer()
	c := tools.GetNode().Client
	if viper.GetBool(WithSync) {
		processSync(c)
	}
	processWatch(c)
	return nil
}

func processWatch(c rpcclient.Client) {
	log.Printf("watched Transactions start")

	ctx, _ := context.WithTimeout(context.Background(), 10*time.Second)
	txs := make(chan interface{})

	c.Start()
	err := c.Subscribe(ctx, Subscriber, types.EventQueryTx, txs)

	if err != nil {
		log.Println("got ", err)
	}

	go func() {
		log.Println("listening tx begin")
		for e := range txs {
			deliverTxRes := e.(types.TMEventData).Unwrap().(types.EventDataTx)
			height := deliverTxRes.Height

			txb, _ := sdk.LoadTx(deliverTxRes.Tx)
			txType, tx := parseTx(txb)
			block := queryBlock(c, height)

			if txType == "coin" {
				coinTx, _ := tx.(store.CoinTx)

				coinTx.TxHash = strings.ToUpper(hex.EncodeToString(deliverTxRes.Tx.Hash()))
				coinTx.Time = block.BlockMeta.Header.Time
				coinTx.Height = height
				if store.Mgo.Save(coinTx) != nil {
					break
				}
				log.Printf("watched coinTx,tx_hash=%s", coinTx.TxHash)
			} else if txType == "stake" {
				stakeTx, _ := tx.(store.StakeTx)
				stakeTx.TxHash = strings.ToUpper(hex.EncodeToString(deliverTxRes.Tx.Hash()))
				stakeTx.Time = block.BlockMeta.Header.Time
				stakeTx.Height = height
				if store.Mgo.Save(stakeTx) != nil {
					break
				}
				log.Printf("watched stakeTx,tx_hash=%s", stakeTx.TxHash)
			}

			// if sync process is over , update the block by watch process
			//if syncOver {
			//	b := store.SyncBlock{
			//		Height:block.Block.Height,
			//		Time : block.Block.Time,
			//		ChainID : block.Block.ChainID,
			//	}
			//	store.Mgo.UpdateBlock(b)
			//}
		}
	}()
}

func parseTx(tx sdk.Tx) (string, interface{}) {
	txl, ok := tx.Unwrap().(sdk.TxLayer)
	var txi sdk.Tx
	var coinTx store.CoinTx
	var stakeTx store.StakeTx
	var nonceAddr data.Bytes
	for ok {
		txi = txl.Next()
		switch txi.Unwrap().(type) {
		case coin.SendTx:
			ctx, _ := txi.Unwrap().(coin.SendTx)
			coinTx.From = fmt.Sprintf("%s", ctx.Inputs[0].Address.Address)
			coinTx.To = fmt.Sprintf("%s", ctx.Outputs[0].Address.Address)
			coinTx.Amount = ctx.Inputs[0].Coins
			return "coin", coinTx
		case nonce.Tx:
			ctx, _ := txi.Unwrap().(nonce.Tx)
			nonceAddr = ctx.Signers[0].Address
			break
		case stake.TxUnbond, stake.TxDelegate, stake.TxDeclareCandidacy:
			kind, _ := txi.GetKind()
			stakeTx.From = fmt.Sprintf("%s", nonceAddr)
			stakeTx.Type = strings.Replace(kind, "stake/", "", -1)
			switch kind {
			case "stake/unbond":
				ctx, _ := txi.Unwrap().(stake.TxUnbond)
				stakeTx.Amount.Denom = "fermion"
				stakeTx.Amount.Amount = int64(ctx.Shares)
				stakeTx.PubKey = fmt.Sprintf("%s", ctx.PubKey.Address())
				break
			case "stake/delegate":
				ctx, _ := txi.Unwrap().(stake.TxDelegate)
				stakeTx.Amount.Denom = ctx.Bond.Denom
				stakeTx.Amount.Amount = ctx.Bond.Amount
				stakeTx.PubKey = fmt.Sprintf("%s", ctx.PubKey.Address())
				break
			case "stake/declareCandidacy":
				ctx, _ := txi.Unwrap().(stake.TxDeclareCandidacy)
				stakeTx.Amount.Denom = ctx.BondUpdate.Bond.Denom
				stakeTx.Amount.Amount = ctx.BondUpdate.Bond.Amount
				stakeTx.PubKey = fmt.Sprintf("%s", ctx.PubKey.Address())
				break
			}
			return "stake", stakeTx
		}
		txl, ok = txi.Unwrap().(sdk.TxLayer)
	}
	return "", nil
}

func queryBlock(c rpcclient.Client, height int64) *ctypes.ResultBlock {
	h := cast.ToInt64(height)
	block, err := c.Block(&h)
	if err != nil {
		log.Printf("query block fail ,%d", height)
	}
	return block
}

func sync(c rpcclient.Client) {
	log.Printf("sync Transactions start")

	b, _ := store.Mgo.QueryLastedBlock()
	current := b.Height
	latest := int64(0)

	log.Printf("last block heigth：%d", current)

	for ok := true; ok; ok = current < latest {
		blocks, err := c.BlockchainInfo(current, current+20)
		if err != nil {
			log.Fatal(err)
		}
		for _, block := range blocks.BlockMetas {
			if block.Header.NumTxs > 0 {
				txhash := block.Header.DataHash
				prove := !viper.GetBool(commands.FlagTrustNode)
				res, _ := c.Tx(txhash, prove)
				txs, _ := sdk.LoadTx(res.Proof.Data)
				txType, tx := parseTx(txs)
				if txType == "coin" {
					coinTx, _ := tx.(store.CoinTx)
					coinTx.TxHash = strings.ToUpper(fmt.Sprintf("%s", txhash))
					coinTx.Time = block.Header.Time
					coinTx.Height = block.Header.Height
					if store.Mgo.Save(coinTx) == nil {
						log.Printf("sync coinTx,tx_hash=%s", coinTx.TxHash)
					}

				} else if txType == "stake" {
					stakeTx, _ := tx.(store.StakeTx)
					stakeTx.TxHash = strings.ToUpper(fmt.Sprintf("%s", txhash))
					stakeTx.Time = block.Header.Time
					stakeTx.Height = block.Header.Height
					if store.Mgo.Save(stakeTx) == nil {
						log.Printf("sync stakeTx,tx_hash=%s", stakeTx.TxHash)
					}
				}
			}

		}
		current = blocks.BlockMetas[0].Header.Height + 1
		latest = blocks.LastHeight
		log.Printf("current block height:%d", current-1)

		b.Height = current
		b.Time = blocks.BlockMetas[0].Header.Time
		b.ChainID = blocks.BlockMetas[0].Header.ChainID
	}

	store.Mgo.UpdateBlock(b)
	syncOver = true
	log.Printf("sync Transactions end,current block height:%d", current)
}