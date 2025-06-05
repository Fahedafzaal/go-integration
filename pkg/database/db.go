package database

import (
	"context"
	"fmt"
	"log"

	"github.com/jackc/pgx/v5/pgxpool"
)

type DB struct {
	Pool *pgxpool.Pool
}

// ApplicationPaymentDetails represents payment-related data from your existing schema
type ApplicationPaymentDetails struct {
	ApplicationID          int32
	JobID                  int32
	ApplicantUserID        int32
	PosterUserID           int32
	AgreedUSDAmount        *int32
	PaymentStatus          string
	EscrowJobID            *int32
	EscrowTxHashDeposit    *string
	EscrowTxHashRelease    *string
	EscrowTxHashRefund     *string
	ApplicantWalletAddress *string
	PosterWalletAddress    *string
	ApplicationStatus      string
}

// NewDB creates a new database connection using pgx
func NewDB(connStr string) (*DB, error) {
	pool, err := pgxpool.New(context.Background(), connStr)
	if err != nil {
		return nil, fmt.Errorf("error opening database: %v", err)
	}

	// Test the connection
	if err := pool.Ping(context.Background()); err != nil {
		return nil, fmt.Errorf("error connecting to database: %v", err)
	}

	return &DB{Pool: pool}, nil
}

// GetApplicationPaymentDetails retrieves application and payment details for blockchain operations
func (db *DB) GetApplicationPaymentDetails(ctx context.Context, applicationID int32) (*ApplicationPaymentDetails, error) {
	query := `
		SELECT 
			a.id as application_id,
			a.job_id,
			a.user_id as applicant_user_id,
			j.user_id as poster_user_id,
			a.agreed_usd_amount,
			COALESCE(a.payment_status, 'pending_deposit') as payment_status,
			a.escrow_job_id,
			a.escrow_tx_hash_deposit,
			a.escrow_tx_hash_release,
			a.escrow_tx_hash_refund,
			applicant.wallet_address as applicant_wallet_address,
			poster.wallet_address as poster_wallet_address,
			a.status as application_status
		FROM applications a
		JOIN jobs j ON a.job_id = j.id
		JOIN users applicant ON a.user_id = applicant.id
		JOIN users poster ON j.user_id = poster.id
		WHERE a.id = $1
	`

	details := &ApplicationPaymentDetails{}
	err := db.Pool.QueryRow(ctx, query, applicationID).Scan(
		&details.ApplicationID,
		&details.JobID,
		&details.ApplicantUserID,
		&details.PosterUserID,
		&details.AgreedUSDAmount,
		&details.PaymentStatus,
		&details.EscrowJobID,
		&details.EscrowTxHashDeposit,
		&details.EscrowTxHashRelease,
		&details.EscrowTxHashRefund,
		&details.ApplicantWalletAddress,
		&details.PosterWalletAddress,
		&details.ApplicationStatus,
	)
	if err != nil {
		return nil, fmt.Errorf("error querying application payment details: %v", err)
	}

	return details, nil
}

// UpdatePaymentStatus updates the payment status and transaction hash
func (db *DB) UpdatePaymentStatus(ctx context.Context, applicationID int32, status string, txHash *string, txType string) error {
	var query string
	var args []interface{}

	switch txType {
	case "deposit":
		query = `
			UPDATE applications 
			SET payment_status = $1, escrow_tx_hash_deposit = $2
			WHERE id = $3
		`
		args = []interface{}{status, txHash, applicationID}
	case "release":
		query = `
			UPDATE applications 
			SET payment_status = $1, escrow_tx_hash_release = $2
			WHERE id = $3
		`
		args = []interface{}{status, txHash, applicationID}
	case "refund":
		query = `
			UPDATE applications 
			SET payment_status = $1, escrow_tx_hash_refund = $2
			WHERE id = $3
		`
		args = []interface{}{status, txHash, applicationID}
	default:
		query = `
			UPDATE applications 
			SET payment_status = $1
			WHERE id = $2
		`
		args = []interface{}{status, applicationID}
	}

	_, err := db.Pool.Exec(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("error updating payment status: %v", err)
	}

	return nil
}

// ValidateApplicationForBlockchain checks if application is ready for blockchain operations
func (db *DB) ValidateApplicationForBlockchain(ctx context.Context, applicationID int32) error {
	var status string
	var applicantWallet, posterWallet *string
	var agreedAmount *int32

	query := `
		SELECT 
			a.status,
			a.agreed_usd_amount,
			applicant.wallet_address as applicant_wallet,
			poster.wallet_address as poster_wallet
		FROM applications a
		JOIN jobs j ON a.job_id = j.id
		JOIN users applicant ON a.user_id = applicant.id
		JOIN users poster ON j.user_id = poster.id
		WHERE a.id = $1
	`

	err := db.Pool.QueryRow(ctx, query, applicationID).Scan(
		&status,
		&agreedAmount,
		&applicantWallet,
		&posterWallet,
	)
	if err != nil {
		return fmt.Errorf("application not found: %v", err)
	}

	if applicantWallet == nil || *applicantWallet == "" {
		return fmt.Errorf("applicant wallet address not set")
	}

	if posterWallet == nil || *posterWallet == "" {
		return fmt.Errorf("poster wallet address not set")
	}

	if agreedAmount == nil || *agreedAmount <= 0 {
		return fmt.Errorf("agreed USD amount not set or invalid")
	}

	return nil
}

// CheckEscrowIdempotency checks if escrow funding has already been initiated for an application
func (db *DB) CheckEscrowIdempotency(ctx context.Context, applicationID int32) (bool, string, error) {
	var txHash *string
	var paymentStatus string

	query := `
		SELECT payment_status, escrow_tx_hash_deposit
		FROM applications 
		WHERE id = $1
	`

	err := db.Pool.QueryRow(ctx, query, applicationID).Scan(&paymentStatus, &txHash)
	if err != nil {
		return false, "", fmt.Errorf("error checking escrow idempotency: %v", err)
	}

	// If there's already a deposit transaction hash, return true (already initiated)
	if txHash != nil && *txHash != "" {
		return true, *txHash, nil
	}

	// If payment status is not pending_deposit, it means deposit was already processed
	if paymentStatus != "pending_deposit" && paymentStatus != "" {
		return true, "", nil
	}

	return false, "", nil
}

// AtomicStartEscrowDeposit atomically marks an application as having escrow deposit initiated
// This prevents race conditions from duplicate calls
func (db *DB) AtomicStartEscrowDeposit(ctx context.Context, applicationID int32, txHash string) error {
	// Use a transaction to ensure atomicity
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %v", err)
	}
	defer tx.Rollback(ctx)

	// Check current state and update only if still pending
	query := `
		UPDATE applications 
		SET payment_status = 'deposit_initiated', 
		    escrow_tx_hash_deposit = $2
		WHERE id = $1 
		AND (payment_status = 'pending_deposit' OR payment_status IS NULL OR payment_status = '')
		AND (escrow_tx_hash_deposit IS NULL OR escrow_tx_hash_deposit = '')
	`

	result, err := tx.Exec(ctx, query, applicationID, txHash)
	if err != nil {
		return fmt.Errorf("error updating application for escrow deposit: %v", err)
	}

	rowsAffected := result.RowsAffected()
	if rowsAffected == 0 {
		// No rows updated means either:
		// 1. Application doesn't exist, or
		// 2. Deposit was already initiated
		// Check which case it is
		var existingTxHash *string
		checkQuery := `SELECT escrow_tx_hash_deposit FROM applications WHERE id = $1`
		err := tx.QueryRow(ctx, checkQuery, applicationID).Scan(&existingTxHash)
		if err != nil {
			return fmt.Errorf("application not found or error checking existing state: %v", err)
		}

		if existingTxHash != nil && *existingTxHash != "" {
			// Deposit already initiated - this is expected in idempotent scenarios
			log.Printf("Escrow deposit already initiated for application %d (existing tx: %s)", applicationID, *existingTxHash)
			return nil
		}

		return fmt.Errorf("failed to initiate escrow deposit - application may be in wrong state")
	}

	return tx.Commit(ctx)
}

// Close closes the database connection pool
func (db *DB) Close() {
	db.Pool.Close()
}
