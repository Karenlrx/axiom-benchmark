package main

import (
	"context"
	"fmt"
	"math/big"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Rican7/retry"
	"github.com/Rican7/retry/strategy"
	"github.com/ethereum/go-ethereum/accounts/abi"
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

	contractAddr string
}

const (
	gasPrice = 10000000000000
	to       = "0xa2f28344131970356c4a112d1e634e51589aa57c"
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

func (req *Request) generateTransferTx(nonce uint64) (*types.Transaction, error) {
	var err error
	privateKey := req.client.account.PrivateKey
	price := big.NewInt(gasPrice)
	if randomAccount {
		privateKey, _, err = utils.NewAccount()
		if err != nil {
			return nil, err
		}
		price = big.NewInt(0)
		nonce = 0
	}
	tx := utils.NewTransaction(nonce, common.HexToAddress(to), uint64(10000000), price, nil, big.NewInt(0))
	signTx, err := types.SignTx(tx, types.NewEIP155Signer(req.client.cli.EthGetChainId()), privateKey)
	return signTx, err
}

func (req *Request) limitSendTps(typ string) {
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
					var tx *types.Transaction
					switch typ {
					case transfer:
						tx, err = req.generateTransferTx(uint64(i) + nonce)
						if err != nil {
							req.logger.Panicf("generate transfer tx err:%s", err)
						}
					case contract:
						tx, err = req.generateContractTx(uint64(i)+nonce, req.contractAddr, uint64(0))
						if err != nil {
							req.logger.Panicf("generate Contract tx err:%s", err)
						}
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
				tx, err := req.generateTransferTx(uint64(i) + nonce)
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

func (req *Request) deployContract(nonce uint64) error {
	bytecode := common.Hex2Bytes(ContractBIN)
	tx := types.NewTx(&types.LegacyTx{
		Nonce:    atomic.AddUint64(&nonce, 1) - 1,
		Gas:      uint64(5000000),
		GasPrice: big.NewInt(gasPrice),
		Data:     bytecode,
	})
	signTx, err := types.SignTx(tx, types.NewEIP155Signer(req.client.cli.EthGetChainId()), req.client.account.PrivateKey)
	if err != nil {
		return fmt.Errorf("sign tx err:%s", err)
	}
	txHash, err := req.client.cli.EthSendRawTransaction(signTx)
	if err != nil {
		return fmt.Errorf("send tx err:%s", err)
	}
	time.Sleep(1 * time.Second)
	receipt, err := req.client.cli.EthGetTransactionReceipt(txHash)
	if err != nil {
		return fmt.Errorf("get receipt err:%s", err)
	}
	if receipt.Status != types.ReceiptStatusSuccessful {
		return fmt.Errorf("deploy contract error")
	}

	req.contractAddr = receipt.ContractAddress.String()
	return nil
}

func (req *Request) sendTransaction(wg *sync.WaitGroup, nonce uint64, typ string) {
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
					var tx *types.Transaction
					if typ == transfer {
						tx, err = req.generateTransferTx(atomic.AddUint64(&nonce, 1) - 1)
						if err != nil {
							req.logger.Panicf("generate tx err:%s", err)
							continue
						}
					} else if typ == contract {
						tx, err = req.generateContractTx(atomic.AddUint64(&nonce, 1)-1, req.contractAddr, uint64(0))
						if err != nil {
							req.logger.Panicf("generate tx err:%s", err)
							continue
						}
					}
					if err := retry.Retry(func(attempt uint) error {
						_, err = req.client.cli.EthSendRawTransaction(tx)
						if err != nil {
							return err
						}
						return nil
					}, strategy.Wait(100*time.Millisecond)); err != nil {
						req.logger.Errorf("send tx err:%s", err)
						continue
					}
					req.handleCnt(now)
				}
			}
		}(i)
	}
}

func (req *Request) getBalance(wg *sync.WaitGroup) {
	for i := 1; i <= goroutines; i++ {
		go func(i int) {
			select {
			case <-req.ctx.Done():
				return
			default:
				defer wg.Done()
				for j := 0; j < round; j++ {
					now := time.Now()
					_, err := req.client.cli.EthGetBalance(req.client.account.Address, big.NewInt(-1))
					if err != nil {
						req.logger.Errorf("get balance err:%s", err)
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

const ContractABI = "[{\"inputs\":[],\"name\":\"retrieve\",\"outputs\":[{\"internalType\":\"uint64\",\"name\":\"\",\"type\":\"uint64\"}],\"stateMutability\":\"view\",\"type\":\"function\"},{\"inputs\":[{\"internalType\":\"uint64\",\"name\":\"num\",\"type\":\"uint64\"}],\"name\":\"store\",\"outputs\":[],\"stateMutability\":\"nonpayable\",\"type\":\"function\"}]"
const ContractBIN = "608060405234801561001057600080fd5b50610186806100206000396000f3fe608060405234801561001057600080fd5b50600436106100365760003560e01c80631d9a3bdd1461003b5780632e64cec114610057575b600080fd5b610055600480360381019061005091906100d2565b610075565b005b61005f6100a0565b60405161006c919061010a565b60405180910390f35b806000806101000a81548167ffffffffffffffff021916908367ffffffffffffffff16021790555050565b60008060009054906101000a900467ffffffffffffffff16905090565b6000813590506100cc81610139565b92915050565b6000602082840312156100e457600080fd5b60006100f2848285016100bd565b91505092915050565b61010481610125565b82525050565b600060208201905061011f60008301846100fb565b92915050565b600067ffffffffffffffff82169050919050565b61014281610125565b811461014d57600080fd5b5056fea26469706673582212204691849347a2f1bef4241dc7ceacb8f17c8556dad30e2a1f78e8450c908986a764736f6c63430008040033"

func (req *Request) generateContractTx(nonce uint64, contractAddr string, value uint64) (*types.Transaction, error) {
	loadABI, err := abi.JSON(strings.NewReader(ContractABI))
	if err != nil {
		return nil, err
	}
	pack, err := loadABI.Pack("store", value)
	if err != nil {
		return nil, err
	}
	if err != nil {
		return nil, err
	}

	gasLimit := uint64(210000)

	toAddress := common.HexToAddress(contractAddr)

	price := big.NewInt(gasPrice)

	privateKey := req.client.account.PrivateKey
	if randomAccount {
		privateKey, _, err = utils.NewAccount()
		if err != nil {
			return nil, err
		}
		price = big.NewInt(0)
		nonce = 0
	}

	tx := types.NewTx(&types.LegacyTx{
		To:       &toAddress,
		Nonce:    nonce,
		Gas:      gasLimit,
		GasPrice: price,
		Data:     pack,
	})

	signTx, err := types.SignTx(tx, types.NewEIP155Signer(req.client.cli.EthGetChainId()), privateKey)
	if err != nil {
		return nil, err
	}
	return signTx, nil
}
