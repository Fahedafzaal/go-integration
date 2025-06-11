package blockchain

import (
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
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

	// Parse private key (handle "0x" prefix)
	pk := strings.TrimPrefix(cfg.PrivateKey, "0x")
	privateKey, err := crypto.HexToECDSA(pk)
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

// GetAuth creates a new transactor for sending transactions with enhanced configuration
func (c *Client) GetAuth(ctx context.Context) (*bind.TransactOpts, error) {
	nonce, err := c.ethClient.PendingNonceAt(ctx, c.publicAddress)
	if err != nil {
		return nil, fmt.Errorf("failed to get nonce: %w", err)
	}

	chainID, err := c.ethClient.NetworkID(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get chain ID: %w", err)
	}

	// Create transactor with proper EIP-1559 support
	auth, err := bind.NewKeyedTransactorWithChainID(c.privateKey, chainID)
	if err != nil {
		return nil, fmt.Errorf("failed to create transactor: %w", err)
	}

	// Set context for proper cancellation handling
	auth.Context = ctx
	auth.Nonce = big.NewInt(int64(nonce))
	auth.Value = big.NewInt(0)

	// Check if network supports EIP-1559 (London fork)
	block, err := c.ethClient.HeaderByNumber(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get latest block: %w", err)
	}

	// Use EIP-1559 pricing if supported, otherwise fall back to legacy
	if block.BaseFee != nil {
		// EIP-1559 transaction with dynamic fees
		tipCap, err := c.ethClient.SuggestGasTipCap(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to suggest gas tip cap: %w", err)
		}

		// Calculate max fee per gas (base fee + tip)
		// Use 2x base fee + tip as max fee to handle base fee fluctuations
		maxFeePerGas := new(big.Int).Add(
			new(big.Int).Mul(block.BaseFee, big.NewInt(2)),
			tipCap,
		)

		auth.GasTipCap = tipCap
		auth.GasFeeCap = maxFeePerGas

		log.Printf("DEBUG GetAuth: Using EIP-1559 pricing - TipCap: %s, FeeCap: %s",
			tipCap.String(), maxFeePerGas.String())
	} else {
		// Legacy transaction pricing
		gasPrice, err := c.ethClient.SuggestGasPrice(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to suggest gas price: %w", err)
		}

		// Add 10% buffer to suggested gas price to improve transaction success rate
		gasPriceWithBuffer := new(big.Int).Mul(gasPrice, big.NewInt(110))
		gasPriceWithBuffer.Div(gasPriceWithBuffer, big.NewInt(100))

		auth.GasPrice = gasPriceWithBuffer

		log.Printf("DEBUG GetAuth: Using legacy pricing - GasPrice: %s (with 10%% buffer)",
			gasPriceWithBuffer.String())
	}

	// Set gas limit with reasonable default
	if c.config.GasLimit > 0 {
		auth.GasLimit = c.config.GasLimit
	} else {
		auth.GasLimit = 300000 // Reasonable default for contract interactions
	}

	log.Printf("DEBUG GetAuth: GasLimit: %d, Nonce: %d", auth.GasLimit, nonce)

	return auth, nil
}

// calculateTotalGasCost estimates the total gas cost for a transaction
func (c *Client) calculateTotalGasCost(ctx context.Context, gasLimit uint64) (*big.Int, error) {
	// Check if network supports EIP-1559
	block, err := c.ethClient.HeaderByNumber(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get latest block: %w", err)
	}

	var gasPrice *big.Int

	if block.BaseFee != nil {
		// EIP-1559: estimate with base fee + tip
		tipCap, err := c.ethClient.SuggestGasTipCap(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to suggest gas tip cap: %w", err)
		}

		// Use base fee + tip as estimated gas price
		gasPrice = new(big.Int).Add(block.BaseFee, tipCap)
	} else {
		// Legacy: use suggested gas price with buffer
		suggestedGasPrice, err := c.ethClient.SuggestGasPrice(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to suggest gas price: %w", err)
		}

		// Add 10% buffer
		gasPrice = new(big.Int).Mul(suggestedGasPrice, big.NewInt(110))
		gasPrice.Div(gasPrice, big.NewInt(100))
	}

	// Total gas cost = gas price * gas limit
	totalGasCost := new(big.Int).Mul(gasPrice, big.NewInt(int64(gasLimit)))

	log.Printf("DEBUG calculateTotalGasCost: GasPrice: %s, GasLimit: %d, TotalCost: %s wei",
		gasPrice.String(), gasLimit, totalGasCost.String())

	return totalGasCost, nil
}

// toUsdE8 converts USD float to 8-decimal "micro-dollars" format expected by the contract
func toUsdE8(usdFloat float64) (*big.Int, error) {
	val := new(big.Float).Mul(big.NewFloat(usdFloat), big.NewFloat(1e8))
	intVal, _ := val.Int(nil) // Uses banker's rounding (round to even)
	return intVal, nil
}

// PostJob creates a new job on the blockchain with enhanced error handling and slippage protection
func (c *Client) PostJob(ctx context.Context, jobID uint64, freelancer common.Address, usdAmountFloat float64, client common.Address) (*TransactionResult, error) {
	log.Printf("DEBUG PostJob: Starting PostJob for JobID=%d, USDAmount=%.2f", jobID, usdAmountFloat)

	// Convert USD to 8-decimal format expected by contract
	usdE8, err := toUsdE8(usdAmountFloat)
	if err != nil {
		log.Printf("ERROR PostJob: Failed to convert USD to E8 format: %v", err)
		return nil, fmt.Errorf("failed to convert USD to E8 format: %w", err)
	}

	// Get current ETH price and calculate required ETH
	ethAmount, err := c.contract.ConvertUsdToEth(&bind.CallOpts{Context: ctx}, usdE8)
	if err != nil {
		log.Printf("ERROR PostJob: Failed to convert USD to ETH: %v", err)
		return nil, fmt.Errorf("failed to convert USD to ETH: %w", err)
	}

	// Add slippage buffer to protect against price changes between calculation and execution
	slippageBuffer := new(big.Int).Div(ethAmount, big.NewInt(50)) // 2% slippage buffer
	ethAmountWithSlippage := new(big.Int).Add(ethAmount, slippageBuffer)

	log.Printf("DEBUG PostJob: USD E8: %s, ETH amount: %s, Slippage buffer: %s, Final amount: %s",
		usdE8.String(), ethAmount.String(), slippageBuffer.String(), ethAmountWithSlippage.String())

	// Get transaction options first to calculate gas costs
	auth, err := c.GetAuth(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get auth: %w", err)
	}

	// Calculate total gas cost for balance checking
	totalGasCost, err := c.calculateTotalGasCost(ctx, auth.GasLimit)
	if err != nil {
		log.Printf("WARNING PostJob: Could not calculate gas cost: %v", err)
		// Continue without gas cost calculation
		totalGasCost = big.NewInt(0)
	}

	// Check wallet balance including gas costs
	balance, err := c.GetBalance(ctx, c.publicAddress)
	if err != nil {
		log.Printf("WARNING PostJob: Could not check wallet balance: %v", err)
	} else {
		log.Printf("DEBUG PostJob: Wallet balance: %s wei", balance.String())
		log.Printf("DEBUG PostJob: Required: ETH=%s + Gas=%s = Total=%s wei",
			ethAmountWithSlippage.String(), totalGasCost.String(),
			new(big.Int).Add(ethAmountWithSlippage, totalGasCost).String())

		// Check if we have sufficient balance for ETH amount + gas costs
		totalRequired := new(big.Int).Add(ethAmountWithSlippage, totalGasCost)
		if balance.Cmp(totalRequired) < 0 {
			return nil, fmt.Errorf("insufficient balance: need %s wei (ETH: %s + Gas: %s) but only have %s wei",
				totalRequired.String(), ethAmountWithSlippage.String(), totalGasCost.String(), balance.String())
		}
	}

	// Set the value to send (ETH amount with slippage buffer)
	auth.Value = ethAmountWithSlippage

	log.Printf("DEBUG PostJob: Executing transaction with value: %s wei", auth.Value.String())

	// Execute transaction with usdE8
	tx, err := c.contract.PostJob(auth, big.NewInt(int64(jobID)), freelancer, usdE8, client)
	if err != nil {
		log.Printf("ERROR PostJob: Transaction failed: %v", err)
		return &TransactionResult{
			Success: false,
			Error:   fmt.Errorf("transaction execution failed: %w", err),
		}, err
	}

	log.Printf("DEBUG PostJob: Transaction submitted, hash: %s", tx.Hash().Hex())

	// Wait for transaction confirmation with enhanced retry logic
	result, err := c.waitForTransactionWithRetry(ctx, tx, 3)
	if err != nil {
		log.Printf("ERROR PostJob: Transaction confirmation failed: %v", err)
	} else {
		log.Printf("DEBUG PostJob: Transaction confirmed successfully")
	}

	return result, err
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

	return c.waitForTransactionWithRetry(ctx, tx, 3)
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

	return c.waitForTransactionWithRetry(ctx, tx, 3)
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
//
//nolint:unused
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
	details, err := c.contract.GetJobDetails(&bind.CallOpts{Context: ctx}, big.NewInt(int64(jobID)))
	if err != nil {
		// If error contains "job does not exist" or similar, return false
		// Otherwise, it's a real error
		errStr := err.Error()
		if strings.Contains(errStr, "job does not exist") || strings.Contains(errStr, "Job not found") || strings.Contains(errStr, "execution reverted") {
			return false, nil
		}
		return false, err
	}

	// NEW: Check if job is corrupted (ghost job with ALL fields being zero/null)
	// This aligns with smart contract requirement: jobs[jobId].client == address(0)
	zeroAddr := "0x0000000000000000000000000000000000000000"
	isCorrupted := (details.Client.Hex() == zeroAddr &&
		details.Freelancer.Hex() == zeroAddr &&
		details.UsdAmount.Cmp(big.NewInt(0)) == 0)

	if isCorrupted {
		log.Printf("DEBUG JobExists: Job %d exists but is CORRUPTED (null addresses or zero amount) - treating as non-existing", jobID)
		return false, nil // Allow overwriting corrupted jobs
	}

	log.Printf("DEBUG JobExists: Job %d exists and is VALID (legitimate job)", jobID)
	return true, nil
}

// waitForTransactionWithRetry waits for transaction confirmation with exponential backoff
func (c *Client) waitForTransactionWithRetry(ctx context.Context, tx *types.Transaction, maxRetries int) (*TransactionResult, error) {
	log.Printf("Transaction sent: %s", tx.Hash().Hex())

	var lastErr error
	baseDelay := time.Second

	for attempt := 1; attempt <= maxRetries; attempt++ {
		log.Printf("Attempt %d/%d: Waiting for transaction confirmation", attempt, maxRetries)

		// Create a context with timeout for this attempt
		attemptCtx, cancel := context.WithTimeout(ctx, 60*time.Second)

		// Wait for transaction to be mined
		receipt, err := bind.WaitMined(attemptCtx, c.ethClient, tx)
		cancel() // Call immediately to prevent context leaks

		if err != nil {
			lastErr = err
			if attempt < maxRetries {
				// Exponential backoff: wait longer between retries
				delay := time.Duration(1<<uint(attempt-1)) * baseDelay
				log.Printf("Attempt %d failed: %v. Retrying in %v...", attempt, err, delay)

				select {
				case <-time.After(delay):
					continue
				case <-ctx.Done():
					return &TransactionResult{
						TxHash:  tx.Hash().Hex(),
						Success: false,
						Error:   ctx.Err(),
					}, ctx.Err()
				}
			}

			log.Printf("All %d attempts failed, last error: %v", maxRetries, err)
			return &TransactionResult{
				TxHash:  tx.Hash().Hex(),
				Success: false,
				Error:   err,
			}, err
		}

		// Check if transaction succeeded
		if receipt.Status == types.ReceiptStatusFailed {
			// Get detailed revert reason
			revertReason := c.getRevertReason(ctx, tx.Hash())
			revertErr := fmt.Errorf("transaction reverted: %s", revertReason)

			log.Printf("Transaction failed with revert reason: %s", revertReason)

			return &TransactionResult{
				TxHash:      tx.Hash().Hex(),
				BlockNumber: receipt.BlockNumber.Uint64(),
				GasUsed:     receipt.GasUsed,
				Success:     false,
				Error:       revertErr,
			}, revertErr
		}

		// Transaction succeeded
		log.Printf("Transaction confirmed successfully in block %d, gas used: %d",
			receipt.BlockNumber.Uint64(), receipt.GasUsed)

		return &TransactionResult{
			TxHash:      tx.Hash().Hex(),
			BlockNumber: receipt.BlockNumber.Uint64(),
			GasUsed:     receipt.GasUsed,
			Success:     true,
			Error:       nil,
		}, nil
	}

	// This should never be reached, but included for completeness
	return &TransactionResult{
		TxHash:  tx.Hash().Hex(),
		Success: false,
		Error:   lastErr,
	}, lastErr
}

// getRevertReason attempts to get the detailed revert reason for a failed transaction
func (c *Client) getRevertReason(ctx context.Context, txHash common.Hash) string {
	// Try to get the transaction receipt first
	receipt, err := c.ethClient.TransactionReceipt(ctx, txHash)
	if err != nil {
		log.Printf("DEBUG getRevertReason: Failed to get receipt: %v", err)
		return "execution reverted (unable to fetch receipt)"
	}

	// If the transaction succeeded, there's no revert reason
	if receipt.Status == types.ReceiptStatusSuccessful {
		return "transaction succeeded (no revert)"
	}

	// Try to get the transaction details
	tx, _, err := c.ethClient.TransactionByHash(ctx, txHash)
	if err != nil {
		log.Printf("DEBUG getRevertReason: Failed to get transaction: %v", err)
		return "execution reverted (unable to fetch transaction)"
	}

	// Get the sender address from the transaction
	// We need to derive it since receipt doesn't contain the From field
	var from common.Address
	if tx.ChainId() != nil {
		// For EIP-155 transactions
		signer := types.NewEIP155Signer(tx.ChainId())
		sender, err := types.Sender(signer, tx)
		if err != nil {
			log.Printf("DEBUG getRevertReason: Failed to get sender: %v", err)
			return "execution reverted (unable to get sender)"
		}
		from = sender
	} else {
		// For pre-EIP155 transactions (fallback)
		signer := types.HomesteadSigner{}
		sender, err := types.Sender(signer, tx)
		if err != nil {
			log.Printf("DEBUG getRevertReason: Failed to get sender with fallback: %v", err)
			return "execution reverted (unable to get sender)"
		}
		from = sender
	}

	// Try to simulate the transaction to get the revert reason
	// This attempts to replay the transaction and extract the revert data
	callMsg := ethereum.CallMsg{
		From:     from,
		To:       tx.To(),
		Gas:      tx.Gas(),
		GasPrice: tx.GasPrice(),
		Value:    tx.Value(),
		Data:     tx.Data(),
	}

	// Handle EIP-1559 transactions properly (type 2)
	if tx.Type() == 2 {
		callMsg.GasTipCap = tx.GasTipCap()
		callMsg.GasFeeCap = tx.GasFeeCap()
	}

	// Use the block number from the receipt to replay at the exact same state
	blockNumber := new(big.Int).SetUint64(receipt.BlockNumber.Uint64())

	result, err := c.ethClient.CallContract(ctx, callMsg, blockNumber)
	if err != nil {
		// Extract revert reason from error message if possible
		errStr := err.Error()

		// Common patterns in revert error messages
		if strings.Contains(errStr, "execution reverted: ") {
			// Extract the custom revert message
			parts := strings.Split(errStr, "execution reverted: ")
			if len(parts) > 1 {
				return strings.Trim(parts[1], `"`)
			}
		}

		if strings.Contains(errStr, "revert ") {
			// Extract revert reason from error
			parts := strings.Split(errStr, "revert ")
			if len(parts) > 1 {
				return strings.Trim(parts[1], `"`)
			}
		}

		// Check for common revert patterns
		if strings.Contains(errStr, "insufficient funds") {
			return "InsufficientEthSent"
		}
		if strings.Contains(errStr, "Job already exists") {
			return "JobAlreadyExists"
		}

		log.Printf("DEBUG getRevertReason: Call contract failed: %v", err)
		return fmt.Sprintf("execution reverted (%v)", err)
	}

	// If we get here, the call succeeded, which shouldn't happen for a reverted transaction
	// Try to decode the result as a revert reason
	if len(result) >= 4 {
		// Check if it's an Error(string) revert (0x08c379a0)
		errorSelector := result[:4]
		if hex.EncodeToString(errorSelector) == "08c379a0" && len(result) >= 68 {
			// Decode the string parameter
			// Skip the selector (4 bytes) and offset (32 bytes)
			if len(result) >= 68 {
				lengthBytes := result[36:68]
				length := new(big.Int).SetBytes(lengthBytes).Uint64()

				if len(result) >= int(68+length) {
					messageBytes := result[68 : 68+length]
					return string(messageBytes)
				}
			}
		}
	}

	return "execution reverted (unknown reason)"
}

// GetContract returns the contract instance
func (c *Client) GetContract() (*contracts.EthJobEscrow, error) {
	if c.contract == nil {
		return nil, fmt.Errorf("contract not initialized")
	}
	return c.contract, nil
}
