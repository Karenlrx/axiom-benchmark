package main

import (
	"context"
	"fmt"
	"math/big"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Rican7/retry"
	"github.com/Rican7/retry/strategy"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/meshplus/go-eth-client/utils"
	"github.com/sirupsen/logrus"
)

type Request struct {
	client     *ClientWrapper
	txsCh      chan []*types.Transaction
	txSet      []*types.Transaction
	txSetDone  chan struct{}
	avCnt      int64
	avLatency  int64
	queryCnt   int64
	latency    int64
	maxLatency int64
	totalTps   []int64
	ctx        context.Context
	cancel     context.CancelFunc
	logger     *logrus.Logger
}

const (
	gasPrice = 5000000000000
	to       = "0xc0Ff2e0b3189132D815b8eb325bE17285AC898f8"
)

func (req *Request) listenTxSet() {
	setNum := 1

	for {
		select {
		case batch := <-req.txsCh:
			var wg sync.WaitGroup
			wg.Add(len(batch))
			now := time.Now()
			for _, tx := range batch {
				go func(tx *types.Transaction) {
					defer wg.Done()
					current := time.Now()
					if err := retry.Retry(func(attempt uint) error {
						_, err := req.client.cli.EthSendRawTransaction(tx)
						if err != nil {
							req.logger.Errorf("send transaction err:%s", err)
							return err
						}
						return nil
					}, strategy.Wait(1*time.Second)); err != nil {
						req.logger.Panicf("send transaction err:%s", err)
					}
					req.handleCnt(current)
				}(tx)
			}
			wg.Wait()
			req.txSetDone <- struct{}{}
			req.logger.WithFields(logrus.Fields{"time": time.Since(now), "setNum": setNum, "size": len(batch)}).Info("end handle txSet")
			setNum++
		case <-req.ctx.Done():
			return
		}
	}
}

func (req *Request) generateTx(nonce int64) (*types.Transaction, error) {
	price := big.NewInt(gasPrice)
	tx := utils.NewTransaction(uint64(nonce), common.HexToAddress(to), uint64(10000000), price, nil, big.NewInt(0))
	signTx, err := types.SignTx(tx, types.NewEIP155Signer(req.client.cli.EthGetChainId()), req.client.account.PrivateKey)
	return signTx, err
}

func (req *Request) limitSendTps() {
	go req.listenTxSet()

	ticker := time.NewTicker(1 * time.Second)
	nonce, err := req.client.cli.EthGetTransactionCount(req.client.account.Address, big.NewInt(-1))
	if err != nil {
		req.logger.Panicf("get nonce err:%s", err)
	}
	index := 0
	size := tps
	totalCount := goroutines * round
	for {
		select {
		case <-req.ctx.Done():
			return
		case <-ticker.C:
			// size too large
			if index+size >= totalCount {
				size = goroutines*round + 1
				batch := make([]*types.Transaction, 0)
				for i := index; i < index+size; i++ {
					tx, err := req.generateTx(int64(i) + int64(nonce))
					if err != nil {
						req.logger.Panicf("generate tx err:%s", err)
					}
					batch = append(batch, tx)
				}
				<-req.txSetDone
				req.txsCh <- batch
				req.cancel()
				return
			}
			batch := make([]*types.Transaction, 0)
			for i := index; i < index+size; i++ {
				tx, err := req.generateTx(int64(i) + int64(nonce))
				if err != nil {
					req.logger.Panicf("generate tx err:%s", err)
				}
				batch = append(batch, tx)
			}
			<-req.txSetDone
			req.txsCh <- batch
			index += size
		}
	}
}

func (req *Request) listen(tk *time.Ticker) {
	for {
		select {
		case <-req.ctx.Done():
			return
		case <-tk.C:
			averageTps := req.avCnt
			averageLatency := float64(req.avLatency) / float64(req.avCnt)
			req.totalTps = append(req.totalTps, req.avCnt)

			atomic.StoreInt64(&req.avCnt, 0)
			atomic.StoreInt64(&req.avLatency, 0)
			req.logger.Infof("average tps: %d, average latency: %g", averageTps, averageLatency)
		}
	}
}

func (req *Request) sendTransaction(wg *sync.WaitGroup, nonce int64) {
	for i := 1; i <= goroutines; i++ {
		go func(i int) {
			select {
			case <-req.ctx.Done():
				return
			default:
				defer wg.Done()
				for j := 0; j < round; j++ {
					var err error
					now := time.Now()
					tx, err := req.generateTx(atomic.AddInt64(&nonce, 1) - 1)
					if err != nil {
						req.logger.Panicf("generate tx err:%s", err)
						continue
					}
					_, err = req.client.cli.EthSendRawTransaction(tx)
					if err != nil {
						req.logger.Errorf("send tx err:%s", err)
						continue
					}
					req.handleCnt(now)
				}
			}
		}(i)
	}
}

func (req *Request) handleCnt(now time.Time) {
	txLatency := time.Since(now).Milliseconds()
	for {
		currentMax := atomic.LoadInt64(&req.maxLatency)
		if txLatency > currentMax {
			if atomic.CompareAndSwapInt64(&req.maxLatency, currentMax, txLatency) {
				break
			} else {
				continue
			}
		} else {
			break
		}
	}

	atomic.AddInt64(&req.latency, txLatency)
	atomic.AddInt64(&req.avLatency, txLatency)
	atomic.AddInt64(&req.queryCnt, 1)
	atomic.AddInt64(&req.avCnt, 1)
}

func (req *Request) handleShutdown(cancel context.CancelFunc) {
	var stop = make(chan os.Signal)
	signal.Notify(stop, syscall.SIGTERM)
	signal.Notify(stop, syscall.SIGINT)
	go func() {
		<-stop
		fmt.Println("received interrupt signal, shutting down...")
		cancel()
		close(req.txSetDone)
		os.Exit(0)
	}()
}
