package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/fahedafzaal/go-integration/internal/config"
	"github.com/fahedafzaal/go-integration/pkg/blockchain"
	"github.com/fahedafzaal/go-integration/pkg/database"
)

type PaymentGateway struct {
	client *blockchain.Client
	config *config.Config
	db     *database.DB
}

// Request/Response types for your application flow
type PostJobRequest struct {
	JobID             uint64 `json:"job_id"`             // application.id (your escrow_job_id)
	FreelancerAddress string `json:"freelancer_address"` // applicant wallet
	USDAmount         string `json:"usd_amount"`         // agreed_usd_amount
	ClientAddress     string `json:"client_address"`     // poster wallet
}

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

type TransactionResponse struct {
	TxHash      string `json:"tx_hash"`
	BlockNumber uint64 `json:"block_number"`
	GasUsed     uint64 `json:"gas_used"`
	Success     bool   `json:"success"`
	Error       string `json:"error,omitempty"`
}

func NewPaymentGateway(cfg *config.Config) (*PaymentGateway, error) {
	// Initialize blockchain client
	client, err := blockchain.NewClient(cfg)
	if err != nil {
		return nil, err
	}

	// Initialize database connection
	db, err := database.NewDB(cfg.DatabaseURL)
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("failed to connect to database: %v", err)
	}

	return &PaymentGateway{
		client: client,
		config: cfg,
		db:     db,
	}, nil
}

// POST /post-job - Called when candidate accepts offer
func (pg *PaymentGateway) postJobHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req PostJobRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Validate the application is ready for blockchain operations
	applicationID := int32(req.JobID) // Using application.id as escrow job_id
	if err := pg.db.ValidateApplicationForBlockchain(ctx, applicationID); err != nil {
		http.Error(w, fmt.Sprintf("Application validation failed: %v", err), http.StatusBadRequest)
		return
	}

	// Check if escrow deposit has already been initiated (idempotency check)
	alreadyInitiated, existingTxHash, err := pg.db.CheckEscrowIdempotency(ctx, applicationID)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to check escrow idempotency: %v", err), http.StatusInternalServerError)
		return
	}

	if alreadyInitiated {
		log.Printf("Escrow deposit already initiated for application %d (tx: %s)", applicationID, existingTxHash)

		// Return success response indicating deposit was already initiated
		response := TransactionResponse{
			TxHash:      existingTxHash,
			BlockNumber: 0, // Not available for existing transactions
			GasUsed:     0, // Not available for existing transactions
			Success:     true,
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
		return
	}

	// Get application details from database
	details, err := pg.db.GetApplicationPaymentDetails(ctx, applicationID)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to get application details: %v", err), http.StatusInternalServerError)
		return
	}

	// Verify the request matches database data
	if details.ApplicantWalletAddress == nil || *details.ApplicantWalletAddress != req.FreelancerAddress {
		http.Error(w, "Freelancer address mismatch", http.StatusBadRequest)
		return
	}
	if details.PosterWalletAddress == nil || *details.PosterWalletAddress != req.ClientAddress {
		http.Error(w, "Client address mismatch", http.StatusBadRequest)
		return
	}

	// Parse addresses and amount
	freelancerAddr := common.HexToAddress(req.FreelancerAddress)
	clientAddr := common.HexToAddress(req.ClientAddress)
	usdAmountFloat, err := strconv.ParseFloat(req.USDAmount, 64)
	if err != nil {
		http.Error(w, "Invalid USD amount", http.StatusBadRequest)
		return
	}

	// Post job to blockchain - let smart contract handle all validation
	result, err := pg.client.PostJob(ctx, req.JobID, freelancerAddr, usdAmountFloat, clientAddr)
	if err != nil {
		// Smart contract rejected the transaction with a clear reason
		log.Printf("Smart contract rejected job %d: %v", req.JobID, err)
		http.Error(w, fmt.Sprintf("Smart contract rejected transaction: %v", err), http.StatusBadRequest)
		return
	}

	// Validate transaction result - don't proceed if transaction hash is empty
	if result.TxHash == "" {
		log.Printf("Error: PostJob returned success but with empty transaction hash for application %d", applicationID)
		http.Error(w, "Blockchain operation failed: empty transaction hash", http.StatusInternalServerError)
		return
	}

	// Use atomic database update to prevent race conditions
	if err := pg.db.AtomicStartEscrowDeposit(ctx, applicationID, result.TxHash); err != nil {
		log.Printf("Warning: Blockchain transaction succeeded but database update failed: %v", err)
		// Don't fail the request since blockchain transaction succeeded
	}

	response := TransactionResponse{
		TxHash:      result.TxHash,
		BlockNumber: result.BlockNumber,
		GasUsed:     result.GasUsed,
		Success:     result.Success,
	}

	if result.Error != nil {
		response.Error = result.Error.Error()
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// GET /get-transaction-data?job_id=X&freelancer_address=Y&usd_amount=Z&client_address=W
// Returns encoded transaction data for smart contract interaction
func (pg *PaymentGateway) getTransactionDataHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse query parameters
	jobIDStr := r.URL.Query().Get("job_id")
	freelancerAddress := r.URL.Query().Get("freelancer_address")
	usdAmount := r.URL.Query().Get("usd_amount")
	clientAddress := r.URL.Query().Get("client_address")

	if jobIDStr == "" || freelancerAddress == "" || usdAmount == "" || clientAddress == "" {
		http.Error(w, "Missing required parameters: job_id, freelancer_address, usd_amount, client_address", http.StatusBadRequest)
		return
	}

	jobID, err := strconv.ParseUint(jobIDStr, 10, 64)
	if err != nil {
		http.Error(w, "Invalid job_id", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Create a payment service with direct mode
	service, err := blockchain.NewPaymentGatewayServiceDirect(
		pg.config.EthereumRPCURL,
		pg.config.ContractAddress,
		pg.config.PrivateKey,
	)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to create payment service: %v", err), http.StatusInternalServerError)
		return
	}
	defer service.Close()

	// Calculate required ETH amount
	requiredEth, err := service.CalculateRequiredETH(ctx, usdAmount)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to calculate required ETH: %v", err), http.StatusInternalServerError)
		return
	}

	// Get encoded transaction data
	req := blockchain.PostJobRequest{
		JobID:             jobID,
		FreelancerAddress: freelancerAddress,
		USDAmount:         usdAmount,
		ClientAddress:     clientAddress,
	}

	transactionData, err := service.GetTransactionData(ctx, req)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to get transaction data: %v", err), http.StatusInternalServerError)
		return
	}

	// Return transaction data for client to use
	response := map[string]interface{}{
		"contract_address": pg.config.ContractAddress,
		"required_eth":     requiredEth.String(),
		"transaction_data": "0x" + transactionData,
		"job_id":           jobID,
		"freelancer":       freelancerAddress,
		"client":           clientAddress,
		"usd_amount":       usdAmount,
		"instructions":     "Send a transaction to contract_address with value=required_eth and data=transaction_data",
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// POST /complete-job?job_id=X - Called when poster approves work
func (pg *PaymentGateway) completeJobHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	jobIDStr := r.URL.Query().Get("job_id")
	jobID, err := strconv.ParseUint(jobIDStr, 10, 64)
	if err != nil {
		http.Error(w, "Invalid job ID", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	applicationID := int32(jobID) // application.id is used as escrow job_id

	// Get application details to verify payment status
	details, err := pg.db.GetApplicationPaymentDetails(ctx, applicationID)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to get application details: %v", err), http.StatusInternalServerError)
		return
	}

	if details.PaymentStatus != "deposited" {
		http.Error(w, fmt.Sprintf("Cannot complete job: payment status is '%s', expected 'deposited'", details.PaymentStatus), http.StatusBadRequest)
		return
	}

	// Complete job on blockchain
	result, err := pg.client.MarkJobCompleted(ctx, jobID)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to complete job on blockchain: %v", err), http.StatusInternalServerError)
		return
	}

	// Update database with release transaction hash
	if err := pg.db.UpdatePaymentStatus(ctx, applicationID, "release_initiated", &result.TxHash, "release"); err != nil {
		log.Printf("Warning: Failed to update payment status in database: %v", err)
	}

	response := TransactionResponse{
		TxHash:      result.TxHash,
		BlockNumber: result.BlockNumber,
		GasUsed:     result.GasUsed,
		Success:     result.Success,
	}

	if result.Error != nil {
		response.Error = result.Error.Error()
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// POST /cancel-job?job_id=X - Called for refunds
func (pg *PaymentGateway) cancelJobHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	jobIDStr := r.URL.Query().Get("job_id")
	jobID, err := strconv.ParseUint(jobIDStr, 10, 64)
	if err != nil {
		http.Error(w, "Invalid job ID", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	applicationID := int32(jobID) // application.id is used as escrow job_id

	// Get application details to verify payment status
	details, err := pg.db.GetApplicationPaymentDetails(ctx, applicationID)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to get application details: %v", err), http.StatusInternalServerError)
		return
	}

	if details.PaymentStatus != "deposited" {
		http.Error(w, fmt.Sprintf("Cannot cancel job: payment status is '%s', expected 'deposited'", details.PaymentStatus), http.StatusBadRequest)
		return
	}

	// Cancel job on blockchain
	result, err := pg.client.CancelJob(ctx, jobID)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to cancel job on blockchain: %v", err), http.StatusInternalServerError)
		return
	}

	// Update database with refund transaction hash
	if err := pg.db.UpdatePaymentStatus(ctx, applicationID, "refund_initiated", &result.TxHash, "refund"); err != nil {
		log.Printf("Warning: Failed to update payment status in database: %v", err)
	}

	response := TransactionResponse{
		TxHash:      result.TxHash,
		BlockNumber: result.BlockNumber,
		GasUsed:     result.GasUsed,
		Success:     result.Success,
	}

	if result.Error != nil {
		response.Error = result.Error.Error()
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// GET /job-status?job_id=X - Get application payment status
func (pg *PaymentGateway) getJobStatusHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	jobIDStr := r.URL.Query().Get("job_id")
	jobID, err := strconv.ParseUint(jobIDStr, 10, 64)
	if err != nil {
		http.Error(w, "Invalid job ID", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	applicationID := int32(jobID)

	// Get application details from database
	details, err := pg.db.GetApplicationPaymentDetails(ctx, applicationID)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to get application details: %v", err), http.StatusInternalServerError)
		return
	}

	response := JobStatusResponse{
		JobID:             jobID,
		ApplicationID:     details.ApplicationID,
		FreelancerAddress: *details.ApplicantWalletAddress,
		ClientAddress:     *details.PosterWalletAddress,
		USDAmount:         fmt.Sprintf("%d", *details.AgreedUSDAmount),
		PaymentStatus:     details.PaymentStatus,
		ApplicationStatus: details.ApplicationStatus,
	}

	if details.EscrowTxHashDeposit != nil {
		response.TxHashDeposit = *details.EscrowTxHashDeposit
	}
	if details.EscrowTxHashRelease != nil {
		response.TxHashRelease = *details.EscrowTxHashRelease
	}
	if details.EscrowTxHashRefund != nil {
		response.TxHashRefund = *details.EscrowTxHashRefund
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// POST /confirm-deposit?job_id=X - Called to confirm deposit (for polling/webhook)
func (pg *PaymentGateway) confirmDepositHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	jobIDStr := r.URL.Query().Get("job_id")
	jobID, err := strconv.ParseUint(jobIDStr, 10, 64)
	if err != nil {
		http.Error(w, "Invalid job ID", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	applicationID := int32(jobID)

	// Update payment status to deposited
	if err := pg.db.UpdatePaymentStatus(ctx, applicationID, "deposited", nil, ""); err != nil {
		http.Error(w, fmt.Sprintf("Failed to update payment status: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// POST /confirm-release?job_id=X - Called to confirm release (for polling/webhook)
func (pg *PaymentGateway) confirmReleaseHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	jobIDStr := r.URL.Query().Get("job_id")
	jobID, err := strconv.ParseUint(jobIDStr, 10, 64)
	if err != nil {
		http.Error(w, "Invalid job ID", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	applicationID := int32(jobID)

	// Update payment status to released
	if err := pg.db.UpdatePaymentStatus(ctx, applicationID, "released", nil, ""); err != nil {
		http.Error(w, fmt.Sprintf("Failed to update payment status: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// GET /eth-price - Get current ETH price
func (pg *PaymentGateway) getEthPriceHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	price, err := pg.client.GetETHUSDPrice(ctx)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to get ETH price: %v", err), http.StatusInternalServerError)
		return
	}

	response := map[string]string{
		"eth_usd_price": price.String(),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func main() {
	// Load configuration
	cfg := config.Load()

	// Validate required configuration
	if cfg.ContractAddress == "" {
		log.Fatal("CONTRACT_ADDRESS environment variable is required")
	}
	if cfg.PrivateKey == "" {
		log.Fatal("PRIVATE_KEY environment variable is required")
	}
	if cfg.EthereumRPCURL == "https://sepolia.infura.io/v3/YOUR_INFURA_KEY" {
		log.Fatal("Please set a valid ETHEREUM_RPC_URL")
	}
	if cfg.DatabaseURL == "" {
		log.Fatal("DATABASE_URL environment variable is required")
	}

	// Initialize payment gateway
	gateway, err := NewPaymentGateway(cfg)
	if err != nil {
		log.Fatalf("Failed to initialize payment gateway: %v", err)
	}
	defer gateway.client.Close()
	defer gateway.db.Close()

	// Setup HTTP routes for your application flow
	http.HandleFunc("/post-job", gateway.postJobHandler)                        // Offer accepted → fund escrow
	http.HandleFunc("/complete-job", gateway.completeJobHandler)                // Work approved → release payment
	http.HandleFunc("/cancel-job", gateway.cancelJobHandler)                    // Cancel/refund
	http.HandleFunc("/job-status", gateway.getJobStatusHandler)                 // Get payment status
	http.HandleFunc("/get-transaction-data", gateway.getTransactionDataHandler) // Get encoded transaction data
	http.HandleFunc("/confirm-deposit", gateway.confirmDepositHandler)          // Confirm deposit completion
	http.HandleFunc("/confirm-release", gateway.confirmReleaseHandler)          // Confirm release completion
	http.HandleFunc("/eth-price", gateway.getEthPriceHandler)                   // Current ETH price

	// Health check endpoint
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	log.Printf("Starting payment gateway server on port %s", cfg.ServerPort)
	log.Printf("Contract address: %s", cfg.ContractAddress)
	log.Printf("Network ID: %d", cfg.NetworkID)
	log.Printf("Database connected successfully")

	if err := http.ListenAndServe(":"+cfg.ServerPort, nil); err != nil {
		log.Fatalf("Server failed to start: %v", err)
	}
}
