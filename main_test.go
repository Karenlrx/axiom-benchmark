package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	privateKeyHex = "b6477143e17f889263044f6cf463dc37177ac4526c4c39a7a344198457024a2f"
	address       = "0xc7F999b83Af6DF9e67d0a37Ee7e900bF38b3D013"
)

func TestLoadConfig(t *testing.T) {
	cnf, err := loadConfig("config.toml")
	require.Nil(t, err)
	require.NotNil(t, cnf.PrivateKey)
}

func TestDecodeAccount(t *testing.T) {
	account, err := decodeAccount(privateKeyHex)
	require.Nil(t, err)
	require.NotNil(t, account)
	require.Equal(t, address, account.Address.String())
}
