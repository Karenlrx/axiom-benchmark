package main

import (
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"flag"
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/accounts/keystore"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/crypto/secp256k1"
	client "github.com/meshplus/go-eth-client"
	"github.com/pelletier/go-toml"
	"github.com/sirupsen/logrus"
)

var (
	goroutines int
	round      int
	limitTps   bool
	tps        int
)

type Config struct {
	PrivateKey string   `toml:"private_key"`
	IP         []string `toml:"ip"`
}

func loadConfig(filePath string) (*Config, error) {
	config := &Config{}
	tree, err := toml.LoadFile(filePath)
	if err != nil {
		return nil, err
	}

	if err := tree.Unmarshal(config); err != nil {
		return nil, err
	}

	return config, nil
}

func decodeAccount(privStr string) (*keystore.Key, error) {
	privateKeyBytes, err := hex.DecodeString(privStr)
	if err != nil {
		return nil, err
	}

	// convert to ecdsa.PrivateKey
	privateKey := new(ecdsa.PrivateKey)
	privateKey.Curve = secp256k1.S256() // use secp256k1
	privateKey.D = new(big.Int).SetBytes(privateKeyBytes)

	// generate public key
	privateKey.X, privateKey.Y = secp256k1.S256().ScalarBaseMult(privateKey.D.Bytes())

	addr := crypto.PubkeyToAddress(privateKey.PublicKey)
	fmt.Println("address:", addr)
	adminAccount := &keystore.Key{
		Address:    crypto.PubkeyToAddress(privateKey.PublicKey),
		PrivateKey: privateKey,
	}
	return adminAccount, nil
}

type ClientWrapper struct {
	cli     client.Client
	account *keystore.Key
}

func NewClient() (*ClientWrapper, error) {
	conf, err := loadConfig("config.toml")
	if err != nil {
		return nil, err
	}

	cli, err := client.New(
		client.WithUrls(conf.IP),
	)
	if err != nil {
		return nil, err
	}

	account, err := decodeAccount(conf.PrivateKey)
	if err != nil {
		return nil, err
	}
	return &ClientWrapper{
		cli:     cli,
		account: account,
	}, nil
}

func Init() {
	flag.IntVar(&goroutines, "g", 200,
		"The number of concurrent go routines to send transaction to axiom")
	flag.IntVar(&round, "r", 10000,
		"The round of concurrent go routines to send transaction from axiom")

	flag.BoolVar(&limitTps, "limit_tps", false, "limit send transaction tps for client")
	flag.IntVar(&tps, "tps", 500, "average send transaction tps for client")
}

func main() {
	logger := logrus.New()
	Init()
	flag.Parse()
	logger.WithFields(logrus.Fields{
		"goroutines": goroutines,
		"round":      round,
		"limitTps":   limitTps,
		"tps":        tps}).Info("input param")

	client, err := NewClient()
	if err != nil {
		fmt.Println(err)
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	req := &Request{
		client:    client,
		totalTps:  make([]int64, 0),
		ctx:       ctx,
		cancel:    cancel,
		logger:    logger,
		txSet:     make([]*types.Transaction, 0),
		txsCh:     make(chan []*types.Transaction, 1024000),
		txSetDone: make(chan struct{}, 1),
	}
	req.txSetDone <- struct{}{}
	req.handleShutdown(cancel)

	// listen average second tps
	tk := time.NewTicker(time.Second)
	go req.listen(tk)

	if limitTps {
		req.limitSendTps()
	} else {
		nonce, err := req.client.cli.EthGetTransactionCount(req.client.account.Address, big.NewInt(-1))
		if err != nil {
			req.logger.Panicf("get nonce err:%s", err)
		}
		wg := &sync.WaitGroup{}
		wg.Add(goroutines)
		req.sendTransaction(wg, int64(nonce))
		wg.Wait()
		req.cancel()
	}

	skip := len(req.totalTps) / 8
	begin := skip
	end := len(req.totalTps) - skip
	total := int64(0)
	for _, v := range req.totalTps[begin:end] {
		total += v
	}
	logger.Infof("End Test, total send tx count is %d , average tps is %f , average latency is %fms",
		goroutines*round, float64(total)/float64(end-begin),
		float64(req.latency)/float64(req.queryCnt))
}
