package blockchain

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"strconv"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/fahedafzaal/go-integration/contracts"
	"github.com/fahedafzaal/go-integration/internal/config"
)

// PaymentMode defines the mode of operation for the payment gateway
type PaymentMode int

const (
	// DirectMode uses direct blockchain interaction
	DirectMode PaymentMode = iota
	// HTTPMode uses HTTP calls to the payment gateway service
	HTTPMode
	// HybridMode tries direct first, falls back to HTTP
	HybridMode
)

// PaymentGatewayService provides a unified interface for payment operations
// It supports both direct blockchain interaction and HTTP-based calls
type PaymentGatewayService struct {
	mode       PaymentMode
	client     *Client        // For direct blockchain interaction
	baseURL    string         // For HTTP calls
	httpClient *http.Client   // For HTTP calls
	config     *config.Config // Configuration for direct mode
}

// ServiceConfig holds configuration for the payment gateway service
type ServiceConfig struct {
	Mode            PaymentMode
	BaseURL         string // Required for HTTP and Hybrid modes
	EthereumRPCURL  string // Required for Direct and Hybrid modes
	ContractAddress string // Required for Direct and Hybrid modes
	PrivateKey      string // Required for Direct and Hybrid modes
	GasLimit        uint64 // Optional, defaults to 300000
}

// NewPaymentGatewayService creates a new payment gateway service
func NewPaymentGatewayService(cfg ServiceConfig) (*PaymentGatewayService, error) {
	service := &PaymentGatewayService{
		mode: cfg.Mode,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}

	// Initialize based on mode
	switch cfg.Mode {
	case DirectMode, HybridMode:
		// Initialize blockchain client for direct interaction
		if cfg.EthereumRPCURL == "" || cfg.ContractAddress == "" || cfg.PrivateKey == "" {
			return nil, fmt.Errorf("ethereum RPC URL, contract address, and private key are required for direct mode")
		}

		gasLimit := cfg.GasLimit
		if gasLimit == 0 {
			gasLimit = 300000
		}

		config := &config.Config{
			EthereumRPCURL:  cfg.EthereumRPCURL,
			ContractAddress: cfg.ContractAddress,
			PrivateKey:      cfg.PrivateKey,
			GasLimit:        gasLimit,
		}

		client, err := NewClient(config)
		if err != nil {
			if cfg.Mode == DirectMode {
				return nil, fmt.Errorf("failed to initialize blockchain client: %w", err)
			}
			// For hybrid mode, log the error but continue
			log.Printf("Warning: Failed to initialize blockchain client, will use HTTP mode: %v", err)
		} else {
			service.client = client
			service.config = config
		}

		if cfg.Mode == HybridMode {
			service.baseURL = cfg.BaseURL
		}

	case HTTPMode:
		if cfg.BaseURL == "" {
			return nil, fmt.Errorf("base URL is required for HTTP mode")
		}
		service.baseURL = cfg.BaseURL
	}

	return service, nil
}

// NewPaymentGatewayServiceHTTP creates a service in HTTP-only mode (backward compatibility)
func NewPaymentGatewayServiceHTTP(baseURL string) *PaymentGatewayService {
	service, _ := NewPaymentGatewayService(ServiceConfig{
		Mode:    HTTPMode,
		BaseURL: baseURL,
	})
	return service
}

// NewPaymentGatewayServiceDirect creates a service in direct blockchain mode
func NewPaymentGatewayServiceDirect(ethereumRPCURL, contractAddress, privateKey string) (*PaymentGatewayService, error) {
	return NewPaymentGatewayService(ServiceConfig{
		Mode:            DirectMode,
		EthereumRPCURL:  ethereumRPCURL,
		ContractAddress: contractAddress,
		PrivateKey:      privateKey,
	})
}

// NewPaymentGatewayServiceHybrid creates a service in hybrid mode (direct + HTTP fallback)
func NewPaymentGatewayServiceHybrid(ethereumRPCURL, contractAddress, privateKey, baseURL string) (*PaymentGatewayService, error) {
	return NewPaymentGatewayService(ServiceConfig{
		Mode:            HybridMode,
		EthereumRPCURL:  ethereumRPCURL,
		ContractAddress: contractAddress,
		PrivateKey:      privateKey,
		BaseURL:         baseURL,
	})
}

// PostJobRequest represents the request for posting a job to escrow
type PostJobRequest struct {
	JobID             uint64 `json:"job_id"`             // application.id
	FreelancerAddress string `json:"freelancer_address"` // applicant wallet
	USDAmount         string `json:"usd_amount"`         // agreed_usd_amount
	ClientAddress     string `json:"client_address"`     // poster wallet
	ClientTxHash      string `json:"client_tx_hash"`     // transaction hash from client's wallet
}

// TransactionResponse represents a blockchain transaction response
type TransactionResponse struct {
	TxHash      string `json:"tx_hash"`
	BlockNumber uint64 `json:"block_number"`
	GasUsed     uint64 `json:"gas_used"`
	Success     bool   `json:"success"`
	Error       string `json:"error,omitempty"`
}

// JobStatusResponse represents job status from the payment gateway
type JobStatusResponse struct {
	JobID             uint64 `json:"job_id"`
	ApplicationID     int32  `json:"application_id"`
	FreelancerAddress string `json:"freelancer_address"`
	ClientAddress     string `json:"client_address"`
	USDAmount         string `json:"usd_amount"`
	PaymentStatus     string `json:"payment_status"`
	ApplicationStatus string `json:"application_status"`
	TxHashDeposit     string `json:"tx_hash_deposit,omitempty"`
	TxHashRelease     string `json:"tx_hash_release,omitempty"`
	TxHashRefund      string `json:"tx_hash_refund,omitempty"`
}

// PostJob initiates escrow funding when candidate accepts offer
func (s *PaymentGatewayService) PostJob(ctx context.Context, req PostJobRequest) (*TransactionResponse, error) {
	// DEBUG: Log the incoming request
	log.Printf("DEBUG PostJob: Starting PostJob for JobID=%d, USDAmount='%s', Freelancer=%s, Client=%s, ClientTxHash=%s",
		req.JobID, req.USDAmount, req.FreelancerAddress, req.ClientAddress, req.ClientTxHash)

	// Let the smart contract be the single source of truth for job existence validation

	// Verify client's transaction
	if req.ClientTxHash == "" {
		return nil, fmt.Errorf("client transaction hash is required")
	}

	// Calculate required ETH amount
	requiredEth, err := s.CalculateRequiredETH(ctx, req.USDAmount)
	if err != nil {
		return nil, fmt.Errorf("failed to calculate required ETH: %w", err)
	}

	log.Printf("DEBUG PostJob: Required ETH amount for job %d: %s wei", req.JobID, requiredEth.String())

	// Verify the transaction using event-based approach
	if s.canUseDirect() {
		err := s.verifyJobPostedTransaction(ctx, req, requiredEth)
		if err != nil {
			return nil, fmt.Errorf("transaction verification failed: %w", err)
		}

		log.Printf("DEBUG PostJob: Client transaction verified successfully for job %d", req.JobID)
	}

	// Return success with client's transaction hash
	return &TransactionResponse{
		TxHash:  req.ClientTxHash,
		Success: true,
	}, nil
}

// verifyJobPostedTransaction verifies the client's transaction using event-based approach
func (s *PaymentGatewayService) verifyJobPostedTransaction(ctx context.Context, req PostJobRequest, requiredEth *big.Int) error {
	txHash := common.HexToHash(req.ClientTxHash)

	// Get transaction and receipt
	tx, isPending, err := s.client.ethClient.TransactionByHash(ctx, txHash)
	if err != nil {
		return fmt.Errorf("failed to get transaction: %w", err)
	}
	if isPending {
		return fmt.Errorf("transaction is still pending")
	}

	receipt, err := s.client.ethClient.TransactionReceipt(ctx, txHash)
	if err != nil {
		return fmt.Errorf("failed to get transaction receipt: %w", err)
	}

	// Check if transaction was successful
	if receipt.Status != types.ReceiptStatusSuccessful {
		return fmt.Errorf("transaction failed with status: %d", receipt.Status)
	}

	// Verify transaction sender using block hash (not tx hash)
	from, err := s.client.ethClient.TransactionSender(ctx, tx, receipt.BlockHash, uint(receipt.TransactionIndex))
	if err != nil {
		return fmt.Errorf("failed to get transaction sender: %w", err)
	}

	expectedClient := common.HexToAddress(req.ClientAddress)
	if from != expectedClient {
		return fmt.Errorf("transaction sender %s does not match expected client address %s",
			from.Hex(), expectedClient.Hex())
	}

	// Verify transaction is to the correct contract
	if tx.To() == nil || tx.To().Hex() != s.config.ContractAddress {
		return fmt.Errorf("transaction is not to the correct contract address")
	}

	// Verify transaction value with 1% tolerance
	tolerance := new(big.Int).Div(requiredEth, big.NewInt(100)) // 1% tolerance
	delta := new(big.Int).Sub(tx.Value(), requiredEth)
	deltaAbs := new(big.Int).Abs(delta)
	if deltaAbs.Cmp(tolerance) == 1 {
		return fmt.Errorf("transaction value %s differs from required %s by more than 1%% tolerance",
			tx.Value().String(), requiredEth.String())
	}

	log.Printf("DEBUG PostJob: Value check passed - TX: %s, Required: %s, Tolerance: %s",
		tx.Value().String(), requiredEth.String(), tolerance.String())

	// Most importantly: Verify JobPosted event was emitted
	err = s.verifyJobPostedEvent(ctx, receipt, req)
	if err != nil {
		return fmt.Errorf("failed to verify JobPosted event: %w", err)
	}

	return nil
}

// verifyJobPostedEvent verifies that the JobPosted event was emitted with correct parameters
func (s *PaymentGatewayService) verifyJobPostedEvent(ctx context.Context, receipt *types.Receipt, req PostJobRequest) error {
	// Get the contract instance for event parsing
	contract, err := s.client.GetContract()
	if err != nil {
		return fmt.Errorf("failed to get contract instance: %w", err)
	}

	// Parse expected values
	expectedJobID := big.NewInt(int64(req.JobID))
	expectedClient := common.HexToAddress(req.ClientAddress)
	expectedFreelancer := common.HexToAddress(req.FreelancerAddress)

	usdAmountFloat, err := strconv.ParseFloat(req.USDAmount, 64)
	if err != nil {
		return fmt.Errorf("invalid USD amount: %w", err)
	}
	val := new(big.Float).Mul(big.NewFloat(usdAmountFloat), big.NewFloat(1e8))
	expectedUSDAmount, _ := val.Int(nil)

	// Look for JobPosted event in transaction logs
	var foundEvent *contracts.EthJobEscrowJobPosted
	for _, log := range receipt.Logs {
		// Check if this log is from our contract
		if log.Address != common.HexToAddress(s.config.ContractAddress) {
			continue
		}

		// Try to parse as JobPosted event
		event, err := contract.ParseJobPosted(*log)
		if err != nil {
			// This log is not a JobPosted event, continue
			continue
		}

		// Check if this is the event for our job
		if event.JobId.Cmp(expectedJobID) == 0 {
			foundEvent = event
			break
		}
	}

	if foundEvent == nil {
		return fmt.Errorf("JobPosted event not found for job ID %d in transaction logs", req.JobID)
	}

	// Verify event parameters match expectations
	if foundEvent.Client != expectedClient {
		return fmt.Errorf("event client address %s does not match expected %s",
			foundEvent.Client.Hex(), expectedClient.Hex())
	}

	if foundEvent.Freelancer != expectedFreelancer {
		return fmt.Errorf("event freelancer address %s does not match expected %s",
			foundEvent.Freelancer.Hex(), expectedFreelancer.Hex())
	}

	if foundEvent.UsdAmount.Cmp(expectedUSDAmount) != 0 {
		return fmt.Errorf("event USD amount %s does not match expected %s",
			foundEvent.UsdAmount.String(), expectedUSDAmount.String())
	}

	// Verify the ETH amount is reasonable (within our tolerance)
	requiredEth, err := s.CalculateRequiredETH(ctx, req.USDAmount)
	if err != nil {
		return fmt.Errorf("failed to recalculate required ETH: %w", err)
	}

	tolerance := new(big.Int).Div(requiredEth, big.NewInt(100)) // 1% tolerance
	ethDelta := new(big.Int).Sub(foundEvent.EthAmount, requiredEth)
	ethDeltaAbs := new(big.Int).Abs(ethDelta)
	if ethDeltaAbs.Cmp(tolerance) == 1 {
		return fmt.Errorf("event ETH amount %s differs from expected %s by more than 1%% tolerance",
			foundEvent.EthAmount.String(), requiredEth.String())
	}

	log.Printf("DEBUG PostJob: JobPosted event verified successfully")
	log.Printf("  - JobID: %s", foundEvent.JobId.String())
	log.Printf("  - Client: %s", foundEvent.Client.Hex())
	log.Printf("  - Freelancer: %s", foundEvent.Freelancer.Hex())
	log.Printf("  - USD Amount: %s", foundEvent.UsdAmount.String())
	log.Printf("  - ETH Amount: %s", foundEvent.EthAmount.String())

	return nil
}

// CompleteJob releases payment when poster approves work
func (s *PaymentGatewayService) CompleteJob(ctx context.Context, jobID uint64) (*TransactionResponse, error) {
	// Try direct blockchain interaction first (if available)
	if s.canUseDirect() {
		result, err := s.completeJobDirect(ctx, jobID)
		if err == nil {
			return result, nil
		}

		log.Printf("Direct blockchain call failed: %v", err)

		// If we're in direct mode only, return the error
		if s.mode == DirectMode {
			return nil, fmt.Errorf("direct blockchain interaction failed: %w", err)
		}

		// Otherwise, fall back to HTTP
		log.Printf("Falling back to HTTP mode")
	}

	// Use HTTP mode
	if s.canUseHTTP() {
		return s.completeJobHTTP(ctx, jobID)
	}

	return nil, fmt.Errorf("no available payment method")
}

// CancelJob initiates refund for cancelled jobs
func (s *PaymentGatewayService) CancelJob(ctx context.Context, jobID uint64) (*TransactionResponse, error) {
	// Try direct blockchain interaction first (if available)
	if s.canUseDirect() {
		result, err := s.cancelJobDirect(ctx, jobID)
		if err == nil {
			return result, nil
		}

		log.Printf("Direct blockchain call failed: %v", err)

		// If we're in direct mode only, return the error
		if s.mode == DirectMode {
			return nil, fmt.Errorf("direct blockchain interaction failed: %w", err)
		}

		// Otherwise, fall back to HTTP
		log.Printf("Falling back to HTTP mode")
	}

	// Use HTTP mode
	if s.canUseHTTP() {
		return s.cancelJobHTTP(ctx, jobID)
	}

	return nil, fmt.Errorf("no available payment method")
}

// GetJobStatus retrieves current payment status
func (s *PaymentGatewayService) GetJobStatus(ctx context.Context, jobID uint64) (*JobStatusResponse, error) {
	// Try direct blockchain interaction first (if available)
	if s.canUseDirect() {
		result, err := s.getJobStatusDirect(ctx, jobID)
		if err == nil {
			return result, nil
		}

		log.Printf("Direct blockchain call failed: %v", err)

		// If we're in direct mode only, return the error
		if s.mode == DirectMode {
			return nil, fmt.Errorf("direct blockchain interaction failed: %w", err)
		}

		// Otherwise, fall back to HTTP
		log.Printf("Falling back to HTTP mode")
	}

	// Use HTTP mode
	if s.canUseHTTP() {
		return s.getJobStatusHTTP(ctx, jobID)
	}

	return nil, fmt.Errorf("no available payment method")
}

// GetETHUSDPrice gets current ETH/USD price
func (s *PaymentGatewayService) GetETHUSDPrice(ctx context.Context) (*big.Int, error) {
	// Try direct blockchain interaction first (if available)
	if s.canUseDirect() {
		price, err := s.client.GetETHUSDPrice(ctx)
		if err == nil {
			return price, nil
		}

		log.Printf("Direct blockchain call failed: %v", err)

		// If we're in direct mode only, return the error
		if s.mode == DirectMode {
			return nil, fmt.Errorf("direct blockchain interaction failed: %w", err)
		}

		// Otherwise, fall back to HTTP
		log.Printf("Falling back to HTTP mode")
	}

	// Use HTTP mode
	if s.canUseHTTP() {
		return s.getETHUSDPriceHTTP(ctx)
	}

	return nil, fmt.Errorf("no available payment method")
}

// Helper methods to check if modes are available
func (s *PaymentGatewayService) canUseDirect() bool {
	return s.client != nil && (s.mode == DirectMode || s.mode == HybridMode)
}

func (s *PaymentGatewayService) canUseHTTP() bool {
	return s.baseURL != "" && (s.mode == HTTPMode || s.mode == HybridMode)
}

// Direct blockchain interaction methods
// postJobDirect is removed as we now use client transactions

func (s *PaymentGatewayService) completeJobDirect(ctx context.Context, jobID uint64) (*TransactionResponse, error) {
	result, err := s.client.MarkJobCompleted(ctx, jobID)
	if err != nil {
		return nil, err
	}

	return &TransactionResponse{
		TxHash:      result.TxHash,
		BlockNumber: result.BlockNumber,
		GasUsed:     result.GasUsed,
		Success:     result.Success,
		Error:       "",
	}, nil
}

func (s *PaymentGatewayService) cancelJobDirect(ctx context.Context, jobID uint64) (*TransactionResponse, error) {
	result, err := s.client.CancelJob(ctx, jobID)
	if err != nil {
		return nil, err
	}

	return &TransactionResponse{
		TxHash:      result.TxHash,
		BlockNumber: result.BlockNumber,
		GasUsed:     result.GasUsed,
		Success:     result.Success,
		Error:       "",
	}, nil
}

func (s *PaymentGatewayService) getJobStatusDirect(ctx context.Context, jobID uint64) (*JobStatusResponse, error) {
	details, err := s.client.GetJobDetails(ctx, jobID)
	if err != nil {
		return nil, err
	}

	// Convert amounts back to strings using fromUsdE8
	usdAmountStr := fromUsdE8(details.USDAmount)

	// Determine payment status
	paymentStatus := "pending"
	if details.IsPaid {
		paymentStatus = "released"
	} else if details.IsCompleted {
		paymentStatus = "completed"
	} else if details.ETHAmount.Cmp(big.NewInt(0)) > 0 {
		paymentStatus = "deposited"
	}

	return &JobStatusResponse{
		JobID:             jobID,
		ApplicationID:     int32(jobID), // Assuming jobID maps to application ID
		FreelancerAddress: details.Freelancer.Hex(),
		ClientAddress:     details.Client.Hex(),
		USDAmount:         usdAmountStr,
		PaymentStatus:     paymentStatus,
		ApplicationStatus: "active", // This would need to be determined from your DB
	}, nil
}

func (s *PaymentGatewayService) getETHUSDPriceHTTP(ctx context.Context) (*big.Int, error) {
	url := fmt.Sprintf("%s/eth-price", s.baseURL)

	httpReq, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := s.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("failed to execute request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("request failed with status %d", resp.StatusCode)
	}

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	// Extract price from response
	priceFloat, ok := result["price"].(float64)
	if !ok {
		return nil, fmt.Errorf("invalid price format in response")
	}

	// Convert to big.Int (assuming price is in USD with 8 decimal places like Chainlink)
	price := big.NewInt(int64(priceFloat * 100000000))
	return price, nil
}

// HTTP-based methods (existing implementation)
func (s *PaymentGatewayService) postJobHTTP(ctx context.Context, req PostJobRequest) (*TransactionResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", s.baseURL+"/post-job", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("failed to execute request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("request failed with status %d", resp.StatusCode)
	}

	var result TransactionResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &result, nil
}

func (s *PaymentGatewayService) completeJobHTTP(ctx context.Context, jobID uint64) (*TransactionResponse, error) {
	url := fmt.Sprintf("%s/complete-job?job_id=%d", s.baseURL, jobID)

	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := s.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("failed to execute request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("request failed with status %d", resp.StatusCode)
	}

	var result TransactionResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &result, nil
}

func (s *PaymentGatewayService) cancelJobHTTP(ctx context.Context, jobID uint64) (*TransactionResponse, error) {
	url := fmt.Sprintf("%s/cancel-job?job_id=%d", s.baseURL, jobID)

	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := s.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("failed to execute request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("request failed with status %d", resp.StatusCode)
	}

	var result TransactionResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &result, nil
}

func (s *PaymentGatewayService) getJobStatusHTTP(ctx context.Context, jobID uint64) (*JobStatusResponse, error) {
	url := fmt.Sprintf("%s/job-status?job_id=%d", s.baseURL, jobID)

	httpReq, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := s.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("failed to execute request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("request failed with status %d", resp.StatusCode)
	}

	var result JobStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &result, nil
}

// ConfirmDeposit confirms that a deposit transaction has been mined (HTTP only for now)
func (s *PaymentGatewayService) ConfirmDeposit(ctx context.Context, jobID uint64) error {
	if !s.canUseHTTP() {
		return fmt.Errorf("HTTP mode not available for confirmation")
	}

	url := fmt.Sprintf("%s/confirm-deposit?job_id=%d", s.baseURL, jobID)

	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := s.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("failed to execute request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("request failed with status %d", resp.StatusCode)
	}

	return nil
}

// ConfirmRelease confirms that a release transaction has been mined (HTTP only for now)
func (s *PaymentGatewayService) ConfirmRelease(ctx context.Context, jobID uint64) error {
	if !s.canUseHTTP() {
		return fmt.Errorf("HTTP mode not available for confirmation")
	}

	url := fmt.Sprintf("%s/confirm-release?job_id=%d", s.baseURL, jobID)

	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := s.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("failed to execute request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("request failed with status %d", resp.StatusCode)
	}

	return nil
}

// Close cleans up resources
func (s *PaymentGatewayService) Close() {
	if s.client != nil {
		s.client.Close()
	}
}

// CheckAndReconcileJobState checks smart contract state and reconciles with expected state
func (s *PaymentGatewayService) CheckAndReconcileJobState(ctx context.Context, jobID uint64) (*JobStatusResponse, error) {
	if !s.canUseDirect() {
		return nil, fmt.Errorf("direct blockchain access required for state reconciliation")
	}

	// Check if job exists in smart contract
	exists, err := s.client.JobExists(ctx, jobID)
	if err != nil {
		return nil, fmt.Errorf("failed to check job existence: %w", err)
	}

	if !exists {
		log.Printf("DEBUG CheckState: Job %d does not exist in smart contract", jobID)
		return &JobStatusResponse{
			JobID:         jobID,
			ApplicationID: int32(jobID),
			PaymentStatus: "not_found",
		}, nil
	}

	log.Printf("DEBUG CheckState: Job %d exists in smart contract, getting details...", jobID)

	// Get job details from smart contract
	details, err := s.client.GetJobDetails(ctx, jobID)
	if err != nil {
		return nil, fmt.Errorf("failed to get job details: %w", err)
	}

	// DEBUG: Log raw blockchain data
	log.Printf("DEBUG CheckState: Job %d blockchain details:", jobID)
	log.Printf("  - Client: %s", details.Client.Hex())
	log.Printf("  - Freelancer: %s", details.Freelancer.Hex())
	log.Printf("  - USDAmount: %s", details.USDAmount.String())
	log.Printf("  - ETHAmount: %s wei (%.6f ETH)", details.ETHAmount.String(),
		new(big.Float).Quo(new(big.Float).SetInt(details.ETHAmount), new(big.Float).SetInt64(1e18)))
	log.Printf("  - IsCompleted: %v", details.IsCompleted)
	log.Printf("  - IsPaid: %v", details.IsPaid)

	// NEW: Detect corrupted/ghost jobs and treat them as "not found"
	// This aligns with smart contract requirement: jobs[jobId].client == address(0)
	zeroAddr := "0x0000000000000000000000000000000000000000"
	isCorrupted := (details.Client.Hex() == zeroAddr &&
		details.Freelancer.Hex() == zeroAddr &&
		details.USDAmount.Cmp(big.NewInt(0)) == 0)

	if isCorrupted {
		log.Printf("DEBUG CheckState: Job %d is CORRUPTED (null addresses or zero amount) - treating as 'not_found'", jobID)
		log.Printf("INFO CheckState: Corrupted job %d can be safely overwritten with legitimate data", jobID)
		return &JobStatusResponse{
			JobID:         jobID,
			ApplicationID: int32(jobID),
			PaymentStatus: "not_found", // Allow overwriting corrupted jobs
		}, nil
	}

	// Convert amounts back to strings using fromUsdE8
	usdAmountStr := fromUsdE8(details.USDAmount)

	// Determine payment status based on smart contract state
	paymentStatus := "pending_deposit"
	if details.IsPaid {
		paymentStatus = "released"
		log.Printf("DEBUG CheckState: Job %d is PAID (released)", jobID)
	} else if details.IsCompleted {
		paymentStatus = "completed"
		log.Printf("DEBUG CheckState: Job %d is COMPLETED but not paid", jobID)
	} else if details.ETHAmount.Cmp(big.NewInt(0)) > 0 {
		paymentStatus = "deposited"
		log.Printf("DEBUG CheckState: Job %d has ETH deposit (%s wei) - status: deposited", jobID, details.ETHAmount.String())
	} else {
		log.Printf("DEBUG CheckState: Job %d has NO ETH deposit but valid addresses - status: pending_deposit", jobID)
	}

	log.Printf("DEBUG CheckState: Final determined status for job %d: %s", jobID, paymentStatus)

	return &JobStatusResponse{
		JobID:             jobID,
		ApplicationID:     int32(jobID),
		FreelancerAddress: details.Freelancer.Hex(),
		ClientAddress:     details.Client.Hex(),
		USDAmount:         usdAmountStr,
		PaymentStatus:     paymentStatus,
		ApplicationStatus: "active",
	}, nil
}

// fromUsdE8 converts 8-decimal "micro-dollars" back to human-readable USD string
func fromUsdE8(u *big.Int) string {
	f := new(big.Float).Quo(new(big.Float).SetInt(u), big.NewFloat(1e8))
	return f.Text('f', 2) // "1234.56"
}

// CalculateRequiredETH calculates the required ETH amount for a USD amount
func (s *PaymentGatewayService) CalculateRequiredETH(ctx context.Context, usd string) (*big.Int, error) {
	usdFloat, err := strconv.ParseFloat(usd, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid USD amount: %w", err)
	}

	val := new(big.Float).Mul(big.NewFloat(usdFloat), big.NewFloat(1e8))
	usdE8, _ := val.Int(nil)

	price, err := s.GetETHUSDPrice(ctx) // 8-decimals
	if err != nil {
		return nil, fmt.Errorf("failed to get ETH price: %w", err)
	}

	wei := new(big.Int).Mul(usdE8, big.NewInt(1e18))
	wei.Div(wei, price)

	log.Printf("DEBUG CalculateRequiredETH: USD=%.2f, USDe8=%s, ETHPrice=%s, RequiredWei=%s",
		usdFloat, usdE8.String(), price.String(), wei.String())

	return wei, nil
}

// GetRequiredETH returns the required ETH amount for a job
func (s *PaymentGatewayService) GetRequiredETH(ctx context.Context, usdAmount string) (*TransactionResponse, error) {
	// Calculate required ETH amount
	requiredEth, err := s.CalculateRequiredETH(ctx, usdAmount)
	if err != nil {
		return nil, fmt.Errorf("failed to calculate required ETH: %w", err)
	}

	// Get current ETH price for reference
	ethPrice, err := s.GetETHUSDPrice(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get ETH price: %w", err)
	}

	// Convert to float for display
	ethPriceFloat := new(big.Float).Quo(new(big.Float).SetInt(ethPrice), new(big.Float).SetInt64(1e8))
	ethPriceStr := ethPriceFloat.Text('f', 2)

	// Return response with required ETH amount
	return &TransactionResponse{
		TxHash:  requiredEth.String(), // Use TxHash field to return the required ETH amount
		Success: true,
		Error:   fmt.Sprintf("ETH_PRICE:%s", ethPriceStr), // Include current ETH price in error field
	}, nil
}

// GetContractInteractionData returns the data needed for the client to interact with the contract
func (s *PaymentGatewayService) GetContractInteractionData(ctx context.Context, req PostJobRequest) (*TransactionResponse, error) {
	// Calculate required ETH amount
	requiredEth, err := s.CalculateRequiredETH(ctx, req.USDAmount)
	if err != nil {
		return nil, fmt.Errorf("failed to calculate required ETH: %w", err)
	}

	// Get current ETH price for reference
	ethPrice, err := s.GetETHUSDPrice(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get ETH price: %w", err)
	}

	// Convert to float for display
	ethPriceFloat := new(big.Float).Quo(new(big.Float).SetInt(ethPrice), new(big.Float).SetInt64(1e8))
	ethPriceStr := ethPriceFloat.Text('f', 2)

	// Parse addresses
	freelancerAddr := common.HexToAddress(req.FreelancerAddress)
	clientAddr := common.HexToAddress(req.ClientAddress)

	// Parse USD amount
	usdAmountFloat, err := strconv.ParseFloat(req.USDAmount, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid USD amount: %w", err)
	}
	val := new(big.Float).Mul(big.NewFloat(usdAmountFloat), big.NewFloat(1e8))
	usdE8, _ := val.Int(nil)

	// Generate contract interaction data
	contractData := map[string]interface{}{
		"contract_address": s.config.ContractAddress,
		"required_eth":     requiredEth.String(),
		"eth_price_usd":    ethPriceStr,
		"job_id":           req.JobID,
		"freelancer":       freelancerAddr.Hex(),
		"client":           clientAddr.Hex(),
		"usd_amount":       usdE8.String(),
	}

	// Convert to JSON
	jsonData, err := json.Marshal(contractData)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal contract data: %w", err)
	}

	// Return response with contract interaction data
	return &TransactionResponse{
		TxHash:  string(jsonData), // Use TxHash field to return the contract data
		Success: true,
	}, nil
}

// GetTransactionData returns the encoded transaction data for the client
func (s *PaymentGatewayService) GetTransactionData(ctx context.Context, req PostJobRequest) (string, error) {
	// Parse addresses
	freelancerAddr := common.HexToAddress(req.FreelancerAddress)
	clientAddr := common.HexToAddress(req.ClientAddress)

	// Parse USD amount
	usdAmountFloat, err := strconv.ParseFloat(req.USDAmount, 64)
	if err != nil {
		return "", fmt.Errorf("invalid USD amount: %w", err)
	}
	val := new(big.Float).Mul(big.NewFloat(usdAmountFloat), big.NewFloat(1e8))
	usdE8, _ := val.Int(nil)

	// Get the contract ABI
	abi, err := contracts.EthJobEscrowMetaData.GetAbi()
	if err != nil {
		return "", fmt.Errorf("failed to get contract ABI: %w", err)
	}

	// Encode the function call data
	data, err := abi.Pack("postJob", big.NewInt(int64(req.JobID)), freelancerAddr, usdE8, clientAddr)
	if err != nil {
		return "", fmt.Errorf("failed to encode transaction data: %w", err)
	}

	// Return the hex-encoded data
	return common.Bytes2Hex(data), nil
}
