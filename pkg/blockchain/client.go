package blockchain

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"log"
	"math/big"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/fahedafzaal/go-integration/contracts"
	"github.com/fahedafzaal/go-integration/internal/config"
)

type Client struct {
	ethClient       *ethclient.Client
	contract        *contracts.EthJobEscrow
	contractAddress common.Address
	privateKey      *ecdsa.PrivateKey
	publicAddress   common.Address
	config          *config.Config
}

type JobDetails struct {
	Client      common.Address
	Freelancer  common.Address
	USDAmount   *big.Int
	ETHAmount   *big.Int
	IsCompleted bool
	IsPaid      bool
}

type TransactionResult struct {
	TxHash      string
	BlockNumber uint64
	GasUsed     uint64
	Success     bool
	Error       error
}

// NewClient creates a new blockchain client instance
func NewClient(cfg *config.Config) (*Client, error) {
	// Connect to Ethereum client
	ethClient, err := ethclient.Dial(cfg.EthereumRPCURL)
	if err != nil {
		return nil, err
	}

	// Parse private key
	privateKey, err := crypto.HexToECDSA(cfg.PrivateKey)
	if err != nil {
		return nil, err
	}

	// Get public address from private key
	publicKey := privateKey.Public()
	publicKeyECDSA, ok := publicKey.(*ecdsa.PublicKey)
	if !ok {
		return nil, errors.New("cannot assert type: publicKey is not of type *ecdsa.PublicKey")
	}
	publicAddress := crypto.PubkeyToAddress(*publicKeyECDSA)

	// Connect to smart contract
	contractAddress := common.HexToAddress(cfg.ContractAddress)
	contract, err := contracts.NewEthJobEscrow(contractAddress, ethClient)
	if err != nil {
		return nil, err
	}

	return &Client{
		ethClient:       ethClient,
		contract:        contract,
		contractAddress: contractAddress,
		privateKey:      privateKey,
		publicAddress:   publicAddress,
		config:          cfg,
	}, nil
}

// GetAuth creates a new transactor for sending transactions
func (c *Client) GetAuth(ctx context.Context) (*bind.TransactOpts, error) {
	nonce, err := c.ethClient.PendingNonceAt(ctx, c.publicAddress)
	if err != nil {
		return nil, err
	}

	gasPrice, err := c.ethClient.SuggestGasPrice(ctx)
	if err != nil {
		return nil, err
	}

	chainID, err := c.ethClient.NetworkID(ctx)
	if err != nil {
		return nil, err
	}

	auth, err := bind.NewKeyedTransactorWithChainID(c.privateKey, chainID)
	if err != nil {
		return nil, err
	}

	auth.Nonce = big.NewInt(int64(nonce))
	auth.Value = big.NewInt(0)
	auth.GasLimit = c.config.GasLimit
	auth.GasPrice = gasPrice

	return auth, nil
}

// PostJob creates a new job on the blockchain
func (c *Client) PostJob(ctx context.Context, jobID uint64, freelancer common.Address, usdAmount *big.Int, client common.Address) (*TransactionResult, error) {
	// Check if job already exists in smart contract
	exists, err := c.JobExists(ctx, jobID)
	if err != nil {
		return nil, fmt.Errorf("failed to check if job exists: %w", err)
	}
	if exists {
		log.Printf("Job %d already exists in smart contract", jobID)

		// Return an error instead of success to maintain clear semantics
		return nil, fmt.Errorf("job %d already exists in smart contract", jobID)
	}

	// Get current ETH price and calculate required ETH
	ethAmount, err := c.contract.ConvertUsdToEth(&bind.CallOpts{Context: ctx}, usdAmount)
	if err != nil {
		return nil, err
	}

	// Get transaction options
	auth, err := c.GetAuth(ctx)
	if err != nil {
		return nil, err
	}

	// Set the value to send (ETH amount)
	auth.Value = ethAmount

	// Execute transaction
	tx, err := c.contract.PostJob(auth, big.NewInt(int64(jobID)), freelancer, usdAmount, client)
	if err != nil {
		return &TransactionResult{
			Success: false,
			Error:   err,
		}, err
	}

	// Wait for transaction confirmation with enhanced error checking
	return c.waitForTransactionWithRetry(ctx, tx, 3)
}

// MarkJobCompleted marks a job as completed and releases payment
func (c *Client) MarkJobCompleted(ctx context.Context, jobID uint64) (*TransactionResult, error) {
	auth, err := c.GetAuth(ctx)
	if err != nil {
		return nil, err
	}

	tx, err := c.contract.MarkJobCompleted(auth, big.NewInt(int64(jobID)))
	if err != nil {
		return &TransactionResult{
			Success: false,
			Error:   err,
		}, err
	}

	return c.waitForTransaction(ctx, tx)
}

// CancelJob cancels a job and refunds the client
func (c *Client) CancelJob(ctx context.Context, jobID uint64) (*TransactionResult, error) {
	auth, err := c.GetAuth(ctx)
	if err != nil {
		return nil, err
	}

	tx, err := c.contract.CancelJob(auth, big.NewInt(int64(jobID)))
	if err != nil {
		return &TransactionResult{
			Success: false,
			Error:   err,
		}, err
	}

	return c.waitForTransaction(ctx, tx)
}

// GetJobDetails retrieves job information from the blockchain
func (c *Client) GetJobDetails(ctx context.Context, jobID uint64) (*JobDetails, error) {
	result, err := c.contract.GetJobDetails(
		&bind.CallOpts{Context: ctx},
		big.NewInt(int64(jobID)),
	)
	if err != nil {
		return nil, err
	}

	return &JobDetails{
		Client:      result.Client,
		Freelancer:  result.Freelancer,
		USDAmount:   result.UsdAmount,
		ETHAmount:   result.EthAmount,
		IsCompleted: result.IsCompleted,
		IsPaid:      result.IsPaid,
	}, nil
}

// GetETHUSDPrice gets the current ETH/USD price from Chainlink
func (c *Client) GetETHUSDPrice(ctx context.Context) (*big.Int, error) {
	return c.contract.GetLatestEthUsd(&bind.CallOpts{Context: ctx})
}

// ConvertUSDToETH converts USD amount to ETH using current price
func (c *Client) ConvertUSDToETH(ctx context.Context, usdAmount *big.Int) (*big.Int, error) {
	return c.contract.ConvertUsdToEth(&bind.CallOpts{Context: ctx}, usdAmount)
}

// waitForTransaction waits for transaction confirmation and returns result
func (c *Client) waitForTransaction(ctx context.Context, tx *types.Transaction) (*TransactionResult, error) {
	log.Printf("Transaction sent: %s", tx.Hash().Hex())

	// Wait for transaction to be mined
	receipt, err := bind.WaitMined(ctx, c.ethClient, tx)
	if err != nil {
		return &TransactionResult{
			TxHash:  tx.Hash().Hex(),
			Success: false,
			Error:   err,
		}, err
	}

	success := receipt.Status == types.ReceiptStatusSuccessful

	return &TransactionResult{
		TxHash:      tx.Hash().Hex(),
		BlockNumber: receipt.BlockNumber.Uint64(),
		GasUsed:     receipt.GasUsed,
		Success:     success,
		Error:       nil,
	}, nil
}

// GetBalance gets ETH balance for an address
func (c *Client) GetBalance(ctx context.Context, address common.Address) (*big.Int, error) {
	return c.ethClient.BalanceAt(ctx, address, nil)
}

// Close closes the Ethereum client connection
func (c *Client) Close() {
	c.ethClient.Close()
}

// JobExists checks if a job already exists in the smart contract
func (c *Client) JobExists(ctx context.Context, jobID uint64) (bool, error) {
	// Try to get job details - if it exists, this will succeed
	_, err := c.contract.GetJobDetails(&bind.CallOpts{Context: ctx}, big.NewInt(int64(jobID)))
	if err != nil {
		// If error contains "job does not exist" or similar, return false
		// Otherwise, it's a real error
		errStr := err.Error()
		if contains(errStr, "job does not exist") || contains(errStr, "Job not found") || contains(errStr, "execution reverted") {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// contains checks if a string contains a substring (case-insensitive)
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr ||
		(len(s) > len(substr) && s[:len(substr)] == substr) ||
		(len(s) > len(substr) && s[len(s)-len(substr):] == substr) ||
		indexOf(s, substr) >= 0)
}

// indexOf finds the index of substr in s
func indexOf(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

// waitForTransactionWithRetry waits for transaction confirmation with retry logic
func (c *Client) waitForTransactionWithRetry(ctx context.Context, tx *types.Transaction, maxRetries int) (*TransactionResult, error) {
	log.Printf("Transaction sent: %s", tx.Hash().Hex())

	var lastErr error
	for attempt := 1; attempt <= maxRetries; attempt++ {
		// Wait for transaction to be mined
		receipt, err := bind.WaitMined(ctx, c.ethClient, tx)
		if err != nil {
			lastErr = err
			if attempt < maxRetries {
				log.Printf("Attempt %d failed, retrying: %v", attempt, err)
				continue
			}
			return &TransactionResult{
				TxHash:  tx.Hash().Hex(),
				Success: false,
				Error:   err,
			}, err
		}

		// Check if transaction succeeded
		if receipt.Status == types.ReceiptStatusFailed {
			// Get revert reason if possible
			revertReason := c.getRevertReason(ctx, tx.Hash())
			revertErr := fmt.Errorf("transaction reverted: %s", revertReason)

			return &TransactionResult{
				TxHash:      tx.Hash().Hex(),
				BlockNumber: receipt.BlockNumber.Uint64(),
				GasUsed:     receipt.GasUsed,
				Success:     false,
				Error:       revertErr,
			}, revertErr
		}

		// Transaction succeeded
		return &TransactionResult{
			TxHash:      tx.Hash().Hex(),
			BlockNumber: receipt.BlockNumber.Uint64(),
			GasUsed:     receipt.GasUsed,
			Success:     true,
			Error:       nil,
		}, nil
	}

	// All retries failed
	return &TransactionResult{
		TxHash:  tx.Hash().Hex(),
		Success: false,
		Error:   lastErr,
	}, lastErr
}

// getRevertReason attempts to get the revert reason for a failed transaction
func (c *Client) getRevertReason(ctx context.Context, txHash common.Hash) string {
	// This is a simplified implementation
	// In a production system, you'd want to replay the transaction to get the actual revert reason
	return "execution reverted"
}
