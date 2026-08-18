package api

import (
	"encoding/hex"
	"net/http"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/gin-gonic/gin"
)

type walletGenerateResponse struct {
	Address    string `json:"address"`
	PrivateKey string `json:"private_key"`
}

// handleWalletGenerate generates a fresh EVM keypair. Used by the Hyperliquid
// wallet-connect flow to create the NOFX agent address + private key needed
// for Hyperliquid agent authorization (NOT a claw402 payment wallet).
func (s *Server) handleWalletGenerate(c *gin.Context) {
	// Generate new EVM wallet
	privateKey, err := crypto.GenerateKey()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate wallet"})
		return
	}

	address := crypto.PubkeyToAddress(privateKey.PublicKey)
	privKeyHex := "0x" + hex.EncodeToString(crypto.FromECDSA(privateKey))

	c.JSON(http.StatusOK, walletGenerateResponse{
		Address:    address.Hex(),
		PrivateKey: privKeyHex,
	})
}
