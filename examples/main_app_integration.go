package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/fahedafzaal/go-integration/pkg/blockchain"
)

// Example integration code for your main application
// This shows how to use the payment gateway service in your existing handlers

func main() {
	fmt.Println("=== Payment Gateway Integration Examples ===")
	fmt.Println()

	// Example 1: HTTP Mode (backward compatibility)
	fmt.Println("1. HTTP Mode Example:")
	exampleHTTPMode()
	fmt.Println()

	// Example 2: Direct Blockchain Mode
	fmt.Println("2. Direct Blockchain Mode Example:")
	exampleDirectMode()
	fmt.Println()

	// Example 3: Hybrid Mode (direct + fallback)
	fmt.Println("3. Hybrid Mode Example:")
	exampleHybridMode()
	fmt.Println()

	// Example 4: Integration with your app service
	fmt.Println("4. Application Service Integration:")
	exampleAppServiceIntegration()
}

// Example 1: HTTP Mode (existing functionality)
func exampleHTTPMode() {
	// Create HTTP-only service (backward compatible)
	paymentGateway := blockchain.NewPaymentGatewayServiceHTTP("http://localhost:8081")
	defer paymentGateway.Close()

	exampleRespondToOfferIntegration(paymentGateway)
}

// Example 2: Direct Blockchain Mode
func exampleDirectMode() {
	// Create direct blockchain service
	paymentGateway, err := blockchain.NewPaymentGatewayServiceDirect(
		"https://sepolia.infura.io/v3/YOUR_PROJECT_ID", // Ethereum RPC URL
		"0xYourContractAddress",                        // Contract address
		"your_private_key_hex",                         // Private key
	)
	if err != nil {
		log.Printf("Failed to create direct payment gateway: %v", err)
		return
	}
	defer paymentGateway.Close()

	exampleRespondToOfferIntegration(paymentGateway)
}

// Example 3: Hybrid Mode
func exampleHybridMode() {
	// Create hybrid service (direct + HTTP fallback)
	paymentGateway, err := blockchain.NewPaymentGatewayServiceHybrid(
		"https://sepolia.infura.io/v3/YOUR_PROJECT_ID", // Ethereum RPC URL
		"0xYourContractAddress",                        // Contract address
		"your_private_key_hex",                         // Private key
		"http://localhost:8081",                        // HTTP fallback URL
	)
	if err != nil {
		log.Printf("Failed to create hybrid payment gateway: %v", err)
		return
	}
	defer paymentGateway.Close()

	exampleRespondToOfferIntegration(paymentGateway)
}

// Example 4: Using advanced configuration
func exampleAdvancedConfiguration() {
	// Create service with advanced configuration
	paymentGateway, err := blockchain.NewPaymentGatewayService(blockchain.ServiceConfig{
		Mode:            blockchain.HybridMode,
		BaseURL:         "http://localhost:8081",
		EthereumRPCURL:  os.Getenv("ETHEREUM_RPC_URL"),
		ContractAddress: os.Getenv("CONTRACT_ADDRESS"),
		PrivateKey:      os.Getenv("PRIVATE_KEY"),
		GasLimit:        350000, // Custom gas limit
	})
	if err != nil {
		log.Printf("Failed to create advanced payment gateway: %v", err)
		return
	}
	defer paymentGateway.Close()

	exampleRespondToOfferIntegration(paymentGateway)
}

// Example integration with RespondToOffer method
func exampleRespondToOfferIntegration(paymentGateway *blockchain.PaymentGatewayService) {
	ctx := context.Background()

	// Mock data - would come from your database
	applicationID := uint64(123)
	freelancerWallet := "0x742e4C7aBd4C77d7084b7Bc2E8E73B0b54e8a9e1"
	clientWallet := "0x8ba1f109551bD432803012645Hac136c82F57eBF"
	agreedAmount := "150.00" // USD

	// This would be called when candidate accepts offer
	fmt.Printf("Processing job acceptance for application %d...\n", applicationID)

	// Fund escrow
	req := blockchain.PostJobRequest{
		JobID:             applicationID,
		FreelancerAddress: freelancerWallet,
		USDAmount:         agreedAmount,
		ClientAddress:     clientWallet,
	}

	result, err := paymentGateway.PostJob(ctx, req)
	if err != nil {
		log.Printf("Failed to fund escrow: %v", err)
		return
	}

	fmt.Printf("✅ Escrow funded successfully!")
	fmt.Printf("   Transaction Hash: %s\n", result.TxHash)
	fmt.Printf("   Block Number: %d\n", result.BlockNumber)
	fmt.Printf("   Gas Used: %d\n", result.GasUsed)

	// Check payment status
	status, err := paymentGateway.GetJobStatus(ctx, applicationID)
	if err != nil {
		log.Printf("Failed to get job status: %v", err)
		return
	}

	fmt.Printf("📊 Current Status:")
	fmt.Printf("   Payment Status: %s\n", status.PaymentStatus)
	fmt.Printf("   USD Amount: $%s\n", status.USDAmount)
}

// Example integration with PosterReviewWork method
func examplePosterReviewWorkIntegration(paymentGateway *blockchain.PaymentGatewayService) {
	ctx := context.Background()
	applicationID := uint64(123)

	// This would be called when poster approves work
	fmt.Printf("Processing work approval for application %d...\n", applicationID)

	result, err := paymentGateway.CompleteJob(ctx, applicationID)
	if err != nil {
		log.Printf("Failed to release payment: %v", err)
		return
	}

	fmt.Printf("✅ Payment released successfully!")
	fmt.Printf("   Transaction Hash: %s\n", result.TxHash)
	fmt.Printf("   Block Number: %d\n", result.BlockNumber)
	fmt.Printf("   Gas Used: %d\n", result.GasUsed)
}

// Example Application Service Integration
func exampleAppServiceIntegration() {
	// This shows how you'd integrate with your existing application service

	fmt.Println("// In your ApplicationService initialization:")
	fmt.Println(`
type ApplicationService struct {
    Queries        *db.Queries
    PaymentGateway *blockchain.PaymentGatewayService
}

func NewApplicationService(queries *db.Queries) (*ApplicationService, error) {
    // Option 1: Direct mode (no server needed)
    paymentGateway, err := blockchain.NewPaymentGatewayServiceDirect(
        os.Getenv("ETHEREUM_RPC_URL"),
        os.Getenv("CONTRACT_ADDRESS"),
        os.Getenv("PRIVATE_KEY"),
    )
    if err != nil {
        return nil, err
    }

    // Option 2: Hybrid mode (direct + HTTP fallback)
    // paymentGateway, err := blockchain.NewPaymentGatewayServiceHybrid(
    //     os.Getenv("ETHEREUM_RPC_URL"),
    //     os.Getenv("CONTRACT_ADDRESS"),
    //     os.Getenv("PRIVATE_KEY"),
    //     "http://localhost:8081",
    // )

    // Option 3: HTTP-only mode (backward compatible)
    // paymentGateway := blockchain.NewPaymentGatewayServiceHTTP("http://localhost:8081")

    return &ApplicationService{
        Queries:        queries,
        PaymentGateway: paymentGateway,
    }, nil
}`)

	fmt.Println("\n// The rest of your integration code remains the same!")
	fmt.Println("// All existing method calls (PostJob, CompleteJob, etc.) work unchanged")
}

// Example with environment configuration
func exampleEnvironmentConfig() {
	// Read from environment variables
	mode := os.Getenv("PAYMENT_MODE") // "direct", "http", or "hybrid"

	var paymentGateway *blockchain.PaymentGatewayService
	var err error

	switch mode {
	case "direct":
		paymentGateway, err = blockchain.NewPaymentGatewayServiceDirect(
			os.Getenv("ETHEREUM_RPC_URL"),
			os.Getenv("CONTRACT_ADDRESS"),
			os.Getenv("PRIVATE_KEY"),
		)
	case "hybrid":
		paymentGateway, err = blockchain.NewPaymentGatewayServiceHybrid(
			os.Getenv("ETHEREUM_RPC_URL"),
			os.Getenv("CONTRACT_ADDRESS"),
			os.Getenv("PRIVATE_KEY"),
			os.Getenv("PAYMENT_GATEWAY_URL"),
		)
	default: // "http"
		paymentGateway = blockchain.NewPaymentGatewayServiceHTTP(
			os.Getenv("PAYMENT_GATEWAY_URL"),
		)
	}

	if err != nil {
		log.Fatalf("Failed to initialize payment gateway: %v", err)
	}
	defer paymentGateway.Close()

	// Use the service...
	fmt.Printf("Payment gateway initialized in %s mode\n", mode)
}

/*
Here's how you would modify your existing handlers:

=== In your ApplicationService.RespondToOffer method ===

func (as *ApplicationService) RespondToOffer(ctx context.Context, params RespondToOfferParams) error {
	// ... existing code for database updates ...

	if params.Accept && newAppStatus == StatusHired {
		// NEW: Call payment gateway to fund escrow
		paymentGateway := blockchain.NewPaymentGatewayService("http://localhost:8081")

		// Get wallet addresses from database
		app, _ := as.Queries.GetApplicationByID(ctx, params.ApplicationID)
		job, _ := as.Queries.GetJobByID(ctx, app.JobID)
		applicant, _ := as.Queries.GetUserByID(ctx, app.UserID)
		poster, _ := as.Queries.GetUserByID(ctx, job.UserID)

		req := blockchain.PostJobRequest{
			JobID:             uint64(params.ApplicationID),
			FreelancerAddress: applicant.WalletAddress.String,
			USDAmount:         fmt.Sprintf("%d", app.AgreedUsdAmount.Int32),
			ClientAddress:     poster.WalletAddress.String,
		}

		result, err := paymentGateway.PostJob(ctx, req)
		if err != nil {
			return fmt.Errorf("failed to fund escrow: %w", err)
		}

		// Update payment status
		err = as.InitiateEscrowDeposit(ctx, params.ApplicationID, result.TxHash)
		if err != nil {
			log.Printf("Warning: blockchain transaction successful but failed to update DB: %v", err)
		}
	}

	return tx.Commit(ctx)
}

=== In your ApplicationService.PosterReviewWork method ===

func (as *ApplicationService) PosterReviewWork(ctx context.Context, params PosterReviewWorkParams) error {
	// ... existing code for database updates ...

	if params.NewStatus == StatusWorkApproved && currentAppPaymentDetails.PaymentStatus.String == PaymentStatusDeposited {
		// NEW: Call payment gateway to release payment
		paymentGateway := blockchain.NewPaymentGatewayService("http://localhost:8081")

		result, err := paymentGateway.CompleteJob(ctx, uint64(params.ApplicationID))
		if err != nil {
			return fmt.Errorf("failed to release payment: %w", err)
		}

		// Update payment status
		err = as.InitiatePaymentRelease(ctx, params.ApplicationID, result.TxHash)
		if err != nil {
			log.Printf("Warning: blockchain transaction successful but failed to update DB: %v", err)
		}
	}

	return tx.Commit(ctx)
}

=== You'll also want to add these helper methods ===

// CheckTransactionStatus can be called periodically to confirm transactions
func (as *ApplicationService) CheckTransactionStatus(ctx context.Context, applicationID int32) error {
	paymentGateway := blockchain.NewPaymentGatewayService("http://localhost:8081")

	status, err := paymentGateway.GetJobStatus(ctx, uint64(applicationID))
	if err != nil {
		return err
	}

	// Update your database based on the blockchain status
	// This could be called from a background job or webhook

	return nil
}
*/
