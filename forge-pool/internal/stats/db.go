package stats

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"strconv"
	"time"

	_ "github.com/lib/pq"
)

var db *sql.DB

// GetDBConnStr returns the database connection string from environment variables
func GetDBConnStr() string {
	host := os.Getenv("DB_HOST")
	if host == "" {
		host = "localhost"
	}
	port := os.Getenv("DB_PORT")
	if port == "" {
		port = "5432"
	}
	user := os.Getenv("DB_USER")
	if user == "" {
		user = "forge"
	}
	password := os.Getenv("DB_PASSWORD")
	if password == "" {
		password = os.Getenv("FORGE_DB_PASSWORD")
	}
	dbname := os.Getenv("DB_NAME")
	if dbname == "" {
		dbname = "forgepool"
	}
	sslmode := os.Getenv("DB_SSLMODE")
	if sslmode == "" {
		sslmode = "disable"
	}

	return fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=%s",
		host, port, user, password, dbname, sslmode)
}

func InitDB(connStr string) error {
	var err error
	db, err = sql.Open("postgres", connStr)
	if err != nil {
		return err
	}

	// Set connection pool settings from environment or use sensible defaults
	maxOpen := 100 // Match config.yaml default
	maxIdle := 25
	if envMaxOpen := os.Getenv("DB_MAX_OPEN_CONNS"); envMaxOpen != "" {
		if v, err := strconv.Atoi(envMaxOpen); err == nil && v > 0 {
			maxOpen = v
		}
	}
	if envMaxIdle := os.Getenv("DB_MAX_IDLE_CONNS"); envMaxIdle != "" {
		if v, err := strconv.Atoi(envMaxIdle); err == nil && v > 0 {
			maxIdle = v
		}
	}
	db.SetMaxOpenConns(maxOpen)
	db.SetMaxIdleConns(maxIdle)
	db.SetConnMaxLifetime(5 * time.Minute)

	if err = db.Ping(); err != nil {
		return err
	}

	log.Printf("✅ Connected to PostgreSQL (pool: %d open, %d idle)", maxOpen, maxIdle)

	// Auto-create tables if they don't exist
	if err := createTablesIfNotExist(); err != nil {
		log.Printf("ERROR: failed to create database tables: %v", err)
		return fmt.Errorf("failed to create tables: %w", err)
	}

	return nil
}

func createTablesIfNotExist() error {
	tables := []string{
		`CREATE TABLE IF NOT EXISTS miners (
			id SERIAL PRIMARY KEY,
			address VARCHAR(255) NOT NULL UNIQUE,
			solo_mining BOOLEAN DEFAULT FALSE,
			manual_diff NUMERIC(20,8) DEFAULT 0,
			min_payout NUMERIC(20,8) DEFAULT 5.0,
			created_at TIMESTAMP DEFAULT NOW(),
			updated_at TIMESTAMP DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_miners_address ON miners(address)`,
		`CREATE TABLE IF NOT EXISTS blocks (
			id SERIAL PRIMARY KEY,
			height INTEGER NOT NULL,
			hash VARCHAR(64) NOT NULL UNIQUE,
			miner_address VARCHAR(255),
			worker_name VARCHAR(255),
			reward NUMERIC(20,8) DEFAULT 50,
			is_solo BOOLEAN DEFAULT FALSE,
			created_at TIMESTAMP DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_blocks_height ON blocks(height)`,
		`CREATE INDEX IF NOT EXISTS idx_blocks_miner ON blocks(miner_address)`,
		`CREATE TABLE IF NOT EXISTS shares (
			id BIGSERIAL PRIMARY KEY,
			miner_address VARCHAR(255) NOT NULL,
			worker_name VARCHAR(255),
			difficulty NUMERIC(20,8) NOT NULL,
			share_diff NUMERIC(30,8) DEFAULT 0,
			is_solo BOOLEAN DEFAULT FALSE,
			created_at TIMESTAMP DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_shares_miner ON shares(miner_address)`,
		`CREATE INDEX IF NOT EXISTS idx_shares_time ON shares(created_at DESC)`,
		// Migration: add share_diff column to existing tables
		`ALTER TABLE shares ADD COLUMN IF NOT EXISTS share_diff NUMERIC(30,8) DEFAULT 0`,
		`CREATE TABLE IF NOT EXISTS payouts (
			id SERIAL PRIMARY KEY,
			miner_address VARCHAR(255) NOT NULL,
			block_height INTEGER,
			amount NUMERIC(20,8) NOT NULL,
			txid VARCHAR(64),
			confirmed BOOLEAN DEFAULT FALSE,
			is_solo BOOLEAN DEFAULT FALSE,
			created_at TIMESTAMP DEFAULT NOW(),
			paid_at TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_payouts_miner ON payouts(miner_address)`,
		`CREATE INDEX IF NOT EXISTS idx_payouts_confirmed ON payouts(confirmed)`,
	}

	for i, stmt := range tables {
		if _, err := db.Exec(stmt); err != nil {
			log.Printf("Failed to execute schema statement %d: %v", i, err)
			return err
		}
	}
	log.Printf("✅ Database schema verified")
	return nil
}

func CloseDB() {
	if db != nil {
		db.Close()
	}
}

// IsDBConnected returns true if the database connection is active
func IsDBConnected() bool {
	if db == nil {
		return false
	}
	return db.Ping() == nil
}

// SavePayout saves a payout to the database
func SavePayout(minerID string, blockHeight int64, amount float64) error {
	if db == nil {
		return nil // No DB, use memory only
	}

	_, err := db.Exec(`
		INSERT INTO payouts (miner_address, block_height, amount, confirmed, created_at)
		VALUES ($1, $2, $3, false, $4)
		ON CONFLICT DO NOTHING`,
		minerID, blockHeight, amount, time.Now())
	return err
}

// SaveBlock saves a block to the database
func SaveBlock(height int64, hash, minerID string, reward float64) error {
	return SaveBlockDBWithSolo(minerID, height, hash, reward, false)
}

// SaveBlockDBWithSolo saves a block to the database with solo flag
func SaveBlockDBWithSolo(minerID string, height int64, hash string, reward float64, isSolo bool) error {
	if db == nil {
		return nil
	}

	_, err := db.Exec(`
		INSERT INTO blocks (height, hash, miner_address, reward, is_solo, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (height) DO NOTHING`,
		height, hash, minerID, reward, isSolo, time.Now())
	return err
}

// SavePayoutAtomic saves both block and payout in a single transaction
// This prevents double-payout bugs and ensures consistency
func SavePayoutAtomic(minerID string, blockHeight int64, amount float64, blockHash string) error {
	return SavePayoutAtomicWithSolo(minerID, blockHeight, amount, blockHash, false)
}

// SavePayoutAtomicWithSolo saves both block and payout with solo flag
func SavePayoutAtomicWithSolo(minerID string, blockHeight int64, amount float64, blockHash string, isSolo bool) error {
	if db == nil {
		return nil
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback() // Rollback if not committed

	// Insert block with conflict handling
	_, err = tx.Exec(`
		INSERT INTO blocks (height, hash, miner_address, reward, is_solo, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (height) DO NOTHING`,
		blockHeight, blockHash, minerID, 50.0, isSolo, time.Now())
	if err != nil {
		return fmt.Errorf("failed to insert block: %w", err)
	}

	// Insert payout with conflict handling
	_, err = tx.Exec(`
		INSERT INTO payouts (miner_address, block_height, amount, confirmed, created_at)
		VALUES ($1, $2, $3, false, $4)
		ON CONFLICT DO NOTHING`,
		minerID, blockHeight, amount, time.Now())
	if err != nil {
		return fmt.Errorf("failed to insert payout: %w", err)
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}

	log.Printf("✅ Saved block %d and payout %.2f BCH2 for %s atomically", blockHeight, amount, minerID)
	return nil
}

// ProcessPayoutAtomic handles the entire payout process in a single transaction
// with proper row locking to prevent race conditions
func ProcessPayoutAtomic(minerID string, currentHeight int64, minPayout float64) (string, float64, error) {
	if db == nil {
		return "", 0, fmt.Errorf("database not initialized")
	}

	// Use context with timeout to prevent hung transactions
	ctx, cancel := context.WithTimeout(context.Background(), DBTimeout)
	defer cancel()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", 0, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Lock and sum mature unpaid payouts for this miner
	var matureAmount float64
	matureHeight := currentHeight - COINBASE_MATURITY

	err = tx.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(amount), 0)
		FROM payouts
		WHERE miner_address = $1
		  AND (txid IS NULL OR txid = '')
		  AND block_height <= $2
		FOR UPDATE`,
		minerID, matureHeight).Scan(&matureAmount)
	if err != nil {
		return "", 0, fmt.Errorf("failed to get mature balance: %w", err)
	}

	if matureAmount < minPayout {
		return "", 0, fmt.Errorf("insufficient mature balance: %.2f < %.2f", matureAmount, minPayout)
	}

	// Generate a placeholder txid that will be updated after actual send
	pendingTxid := fmt.Sprintf("pending_%d_%s", time.Now().UnixNano(), minerID[:8])

	// Mark all mature payouts as being processed (with pending txid)
	_, err = tx.ExecContext(ctx, `
		UPDATE payouts
		SET txid = $1, paid_at = $2
		WHERE miner_address = $3
		  AND (txid IS NULL OR txid = '')
		  AND block_height <= $4`,
		pendingTxid, time.Now(), minerID, matureHeight)
	if err != nil {
		return "", 0, fmt.Errorf("failed to mark payouts: %w", err)
	}

	if err = tx.Commit(); err != nil {
		return "", 0, fmt.Errorf("failed to commit: %w", err)
	}

	return pendingTxid, matureAmount, nil
}

// FinalizePayoutAtomic updates the pending txid to the actual txid after successful send
func FinalizePayoutAtomic(pendingTxid, actualTxid string) error {
	if db == nil {
		return nil
	}

	_, err := db.Exec(`
		UPDATE payouts
		SET txid = $1, confirmed = true
		WHERE txid = $2`,
		actualTxid, pendingTxid)
	return err
}

// RevertPendingPayout reverts a failed payout attempt
func RevertPendingPayout(pendingTxid string) error {
	if db == nil {
		return nil
	}

	_, err := db.Exec(`
		UPDATE payouts
		SET txid = NULL, paid_at = NULL
		WHERE txid = $1`,
		pendingTxid)
	return err
}

// LoadMinerPayouts loads payouts from database into memory
func LoadMinerPayouts(minerID string) {
	if db == nil {
		return
	}

	rows, err := db.Query(`
		SELECT block_height, amount, confirmed, txid, created_at, paid_at
		FROM payouts WHERE miner_address = $1 AND confirmed = false`,
		minerID)
	if err != nil {
		log.Printf("Error loading payouts for %s: %v", minerID, err)
		return
	}
	defer rows.Close()

	pendingPayoutsMu.Lock()
	defer pendingPayoutsMu.Unlock()

	for rows.Next() {
		var p PendingPayout
		var txid sql.NullString
		var paidAt sql.NullTime

		if err := rows.Scan(&p.BlockHeight, &p.Amount, &p.Confirmed, &txid, &p.CreatedAt, &paidAt); err != nil {
			log.Printf("Warning: failed to scan payout for %s: %v", minerID, err)
			continue
		}

		p.MinerID = minerID
		if txid.Valid {
			p.TxID = txid.String
		}
		if paidAt.Valid {
			p.PaidAt = paidAt.Time
		}

		pendingPayouts[minerID] = append(pendingPayouts[minerID], p)
	}

	if err := rows.Err(); err != nil {
		log.Printf("Warning: error iterating payouts for %s: %v", minerID, err)
	}
}

// LoadAllPendingPayouts loads all unpaid payouts from database
func LoadAllPendingPayouts() {
	if db == nil {
		return
	}

	rows, err := db.Query(`
		SELECT miner_address, block_height, amount, confirmed, txid, created_at, paid_at
		FROM payouts WHERE confirmed = false OR txid IS NULL`)
	if err != nil {
		log.Printf("Error loading all payouts: %v", err)
		return
	}
	defer rows.Close()

	pendingPayoutsMu.Lock()
	defer pendingPayoutsMu.Unlock()

	// Clear existing
	pendingPayouts = make(map[string][]PendingPayout)

	for rows.Next() {
		var p PendingPayout
		var txid sql.NullString
		var paidAt sql.NullTime

		if err := rows.Scan(&p.MinerID, &p.BlockHeight, &p.Amount, &p.Confirmed, &txid, &p.CreatedAt, &paidAt); err != nil {
			log.Printf("Warning: failed to scan pending payout: %v", err)
			continue
		}

		if txid.Valid {
			p.TxID = txid.String
		}
		if paidAt.Valid {
			p.PaidAt = paidAt.Time
		}

		pendingPayouts[p.MinerID] = append(pendingPayouts[p.MinerID], p)
	}

	if err := rows.Err(); err != nil {
		log.Printf("Warning: error iterating pending payouts: %v", err)
	}

	log.Printf("✅ Loaded %d miners with pending payouts from database", len(pendingPayouts))
}

// MarkPayoutPaidDB marks payout as paid in database
func MarkPayoutPaidDB(minerID string, blockHeight int64, txid string) error {
	if db == nil {
		return nil
	}
	
	_, err := db.Exec(`
		UPDATE payouts SET confirmed = true, txid = $1, paid_at = $2
		WHERE miner_address = $3 AND block_height = $4`,
		txid, time.Now(), minerID, blockHeight)
	return err
}

// GetMinerBalanceDB gets balance from database
func GetMinerBalanceDB(minerID string, currentHeight int64) (mature float64, immature float64) {
	if db == nil {
		return GetMinerBalance(minerID, currentHeight)
	}
	
	// Mature: blocks with 100+ confirmations and not paid
	row := db.QueryRow(`
		SELECT COALESCE(SUM(amount), 0) FROM payouts
		WHERE miner_address = $1 AND (txid IS NULL OR txid = '') AND block_height <= $2`,
		minerID, currentHeight-COINBASE_MATURITY)
	if err := row.Scan(&mature); err != nil {
		log.Printf("Warning: failed to scan mature balance for %s: %v", minerID, err)
		mature = 0
	}

	// Immature: blocks with < 100 confirmations
	row = db.QueryRow(`
		SELECT COALESCE(SUM(amount), 0) FROM payouts
		WHERE miner_address = $1 AND (txid IS NULL OR txid = '') AND block_height > $2`,
		minerID, currentHeight-COINBASE_MATURITY)
	if err := row.Scan(&immature); err != nil {
		log.Printf("Warning: failed to scan immature balance for %s: %v", minerID, err)
		immature = 0
	}

	return
}

// GetMinerBlocksDB gets blocks from database
func GetMinerBlocksDB(minerID string) []MinerBlock {
	if db == nil {
		return GetMinerBlocks(minerID)
	}

	// Join with payouts table to get the payout txid for each block
	rows, err := db.Query(`
		SELECT b.height, b.hash, b.reward, b.created_at, b.status, COALESCE(p.txid, '')
		FROM blocks b
		LEFT JOIN payouts p ON p.block_height = b.height AND p.miner_address = b.miner_address
		WHERE b.miner_address = $1 ORDER BY b.height DESC LIMIT 100`,
		minerID)
	if err != nil {
		log.Printf("Warning: failed to query miner blocks for %s: %v", minerID, err)
		return []MinerBlock{}
	}
	defer rows.Close()

	var blocks []MinerBlock
	for rows.Next() {
		var b MinerBlock
		var status string
		var payoutTxid string
		if err := rows.Scan(&b.Height, &b.Hash, &b.Reward, &b.Time, &status, &payoutTxid); err != nil {
			log.Printf("Warning: failed to scan miner block: %v", err)
			continue
		}
		b.MinerID = minerID
		b.Confirmed = (status == "confirmed")
		b.PayoutTxid = payoutTxid
		blocks = append(blocks, b)
	}

	if err := rows.Err(); err != nil {
		log.Printf("Warning: error iterating miner blocks for %s: %v", minerID, err)
	}

	return blocks
}

// GetTotalBlocksDB returns total blocks in database
func GetTotalBlocksDB() int64 {
	if db == nil {
		return 0
	}

	var count int64
	if err := db.QueryRow("SELECT COUNT(*) FROM blocks").Scan(&count); err != nil {
		log.Printf("Warning: failed to count total blocks: %v", err)
		return 0
	}
	return count
}

// PoolBlock represents a block mined by the pool
type PoolBlock struct {
	Height    int64   `json:"height"`
	Hash      string  `json:"hash"`
	Reward    float64 `json:"reward"`
	MinerAddr string  `json:"miner_address"`
	Status    string  `json:"status"`
	CreatedAt int64   `json:"time"`
	IsSolo    bool    `json:"is_solo"`
}

// GetAllPoolBlocksDB gets all blocks mined by the pool with pagination
func GetAllPoolBlocksDB(page, limit int) ([]PoolBlock, int64) {
	if db == nil {
		return []PoolBlock{}, 0
	}

	// Get total count
	var total int64
	if err := db.QueryRow("SELECT COUNT(*) FROM blocks").Scan(&total); err != nil {
		log.Printf("Warning: failed to count blocks: %v", err)
		total = 0
	}

	// Get paginated blocks
	offset := (page - 1) * limit
	rows, err := db.Query(`
		SELECT height, hash, reward, miner_address, status, EXTRACT(EPOCH FROM created_at)::bigint, COALESCE(is_solo, false)
		FROM blocks ORDER BY height DESC LIMIT $1 OFFSET $2`,
		limit, offset)
	if err != nil {
		log.Printf("Warning: failed to query pool blocks: %v", err)
		return []PoolBlock{}, total
	}
	defer rows.Close()

	var blocks []PoolBlock
	for rows.Next() {
		var b PoolBlock
		if err := rows.Scan(&b.Height, &b.Hash, &b.Reward, &b.MinerAddr, &b.Status, &b.CreatedAt, &b.IsSolo); err != nil {
			log.Printf("Warning: failed to scan pool block: %v", err)
			continue
		}
		blocks = append(blocks, b)
	}

	if err := rows.Err(); err != nil {
		log.Printf("Warning: error iterating pool blocks: %v", err)
	}

	return blocks, total
}

// MinerBlockContribution represents a miner's contribution to a specific block
type MinerBlockContribution struct {
	Height   int64   `json:"height"`
	Amount   float64 `json:"amount"`
	SharePct float64 `json:"share_pct"`
	Time     int64   `json:"time"`
	IsPaid   bool    `json:"is_paid"`
}

// SoloBlock represents a block found by a solo miner
type SoloBlock struct {
	Height      int64   `json:"height"`
	Hash        string  `json:"hash"`
	Reward      float64 `json:"reward"`
	Time        int64   `json:"time"`
	Status      string  `json:"status"`
	Confirmed   bool    `json:"confirmed"`
	PayoutTxid  string  `json:"payoutTxid,omitempty"`
}

// GetMinerSoloBlocksDB gets solo blocks found by a specific miner
func GetMinerSoloBlocksDB(minerID string) []SoloBlock {
	if db == nil {
		return []SoloBlock{}
	}

	rows, err := db.Query(`
		SELECT b.height, b.hash, b.reward, EXTRACT(EPOCH FROM b.created_at)::bigint, b.status,
			COALESCE(p.txid, '') as payout_txid
		FROM blocks b
		LEFT JOIN payouts p ON p.block_height = b.height AND p.miner_address = b.miner_address
		WHERE b.miner_address = $1 AND b.is_solo = true
		ORDER BY b.height DESC LIMIT 100`,
		minerID)
	if err != nil {
		log.Printf("Warning: failed to query solo blocks for %s: %v", minerID, err)
		return []SoloBlock{}
	}
	defer rows.Close()

	var blocks []SoloBlock
	for rows.Next() {
		var b SoloBlock
		var status string
		var payoutTxid string
		if err := rows.Scan(&b.Height, &b.Hash, &b.Reward, &b.Time, &status, &payoutTxid); err != nil {
			log.Printf("Warning: failed to scan solo block: %v", err)
			continue
		}
		b.Status = status
		b.Confirmed = (status == "confirmed")
		b.PayoutTxid = payoutTxid
		blocks = append(blocks, b)
	}

	if err := rows.Err(); err != nil {
		log.Printf("Warning: error iterating solo blocks for %s: %v", minerID, err)
	}

	return blocks
}

// GetMinerBlockContributionsDB gets all block contributions for a miner from payouts table
func GetMinerBlockContributionsDB(minerID string) []MinerBlockContribution {
	if db == nil {
		return []MinerBlockContribution{}
	}

	rows, err := db.Query(`
		SELECT p.block_height, p.amount, EXTRACT(EPOCH FROM p.created_at)::bigint,
			CASE WHEN p.txid IS NOT NULL AND p.txid != '' THEN true ELSE false END as is_paid
		FROM payouts p
		JOIN blocks b ON b.height = p.block_height
		WHERE p.miner_address = $1 AND b.is_solo = false
		ORDER BY p.block_height DESC LIMIT 50`,
		minerID)
	if err != nil {
		log.Printf("Warning: failed to query block contributions for %s: %v", minerID, err)
		return []MinerBlockContribution{}
	}
	defer rows.Close()

	var contributions []MinerBlockContribution
	for rows.Next() {
		var c MinerBlockContribution
		if err := rows.Scan(&c.Height, &c.Amount, &c.Time, &c.IsPaid); err != nil {
			log.Printf("Warning: failed to scan block contribution: %v", err)
			continue
		}
		// Calculate share percentage (50 BCH2 * 0.99 fee = 49.5 max reward)
		c.SharePct = (c.Amount / 49.5) * 100
		contributions = append(contributions, c)
	}

	if err := rows.Err(); err != nil {
		log.Printf("Warning: error iterating block contributions for %s: %v", minerID, err)
	}

	return contributions
}

// MarkMaturePaidInDB marks all mature payouts as paid directly in database
// Uses a transaction with row locking to prevent race conditions
func MarkMaturePaidInDB(minerID string, currentHeight int64, txid string) error {
	return MarkMaturePaidInDBWithAmount(minerID, currentHeight, txid, 0)
}

// MarkMaturePaidInDBWithAmount marks mature payouts as paid with partial payment support
// If paidAmount > 0, only marks payouts up to that amount; otherwise marks all mature
func MarkMaturePaidInDBWithAmount(minerID string, currentHeight int64, txid string, paidAmount float64) error {
	if db == nil {
		return nil
	}

	// Use context with timeout
	ctx, cancel := context.WithTimeout(context.Background(), DBTimeout)
	defer cancel()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	matureHeight := currentHeight - COINBASE_MATURITY
	now := time.Now()

	var result sql.Result

	if paidAmount == 0 {
		// Full payment mode - mark all mature as paid
		result, err = tx.ExecContext(ctx, `
			UPDATE payouts SET confirmed = true, txid = $1, paid_at = $2
			WHERE miner_address = $3 AND block_height <= $4 AND (txid IS NULL OR txid = '')`,
			txid, now, minerID, matureHeight)
	} else {
		// Partial payment mode - mark payouts up to paidAmount
		// Use a CTE to select payouts in order and mark only up to the paid amount
		result, err = tx.ExecContext(ctx, `
			WITH to_pay AS (
				SELECT id, amount,
					SUM(amount) OVER (ORDER BY block_height, id) as running_total
				FROM payouts
				WHERE miner_address = $1 AND block_height <= $2 AND (txid IS NULL OR txid = '')
			)
			UPDATE payouts SET confirmed = true, txid = $3, paid_at = $4
			WHERE id IN (
				SELECT id FROM to_pay WHERE running_total <= $5 + amount
			)`,
			minerID, matureHeight, txid, now, paidAmount)
	}

	if err != nil {
		log.Printf("DB update error: %v", err)
		return err
	}

	// Also update block status to confirmed for paid blocks
	tx.ExecContext(ctx, `
		UPDATE blocks SET status = 'confirmed'
		WHERE height IN (
			SELECT block_height FROM payouts
			WHERE miner_address = $1 AND txid = $2
		) AND status = 'pending'`,
		minerID, txid)

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit: %w", err)
	}

	rows, _ := result.RowsAffected()
	if paidAmount > 0 {
		log.Printf("Marked %d payouts as paid in DB for %s (txid: %s, amount: %.8f)",
			rows, minerID, txid, paidAmount)
	} else {
		log.Printf("Marked %d payouts as paid in DB for %s (txid: %s)", rows, minerID, txid)
	}
	return nil
}

// PayoutRecord for API response
type PayoutRecord struct {
	TxID      string    `json:"txid"`
	Amount    float64   `json:"amount"`
	PaidAt    time.Time `json:"paidAt"`
	Blocks    int       `json:"blocks"`
	Confirmed bool      `json:"confirmed"`
}

// MinerSettings represents a miner's pool settings
type MinerSettings struct {
	Address    string  `json:"address"`
	SoloMining bool    `json:"solo_mining"`
	ManualDiff float64 `json:"manual_diff"`
	MinPayout  float64 `json:"min_payout"`
}

// SaveMinerSettings saves or updates miner settings in the database
func SaveMinerSettings(settings *MinerSettings) error {
	if db == nil {
		return nil
	}

	_, err := db.Exec(`
		INSERT INTO miners (address, solo_mining, manual_diff, min_payout, updated_at)
		VALUES ($1, $2, $3, $4, NOW())
		ON CONFLICT (address) DO UPDATE SET
			solo_mining = EXCLUDED.solo_mining,
			manual_diff = EXCLUDED.manual_diff,
			min_payout = EXCLUDED.min_payout,
			updated_at = NOW()`,
		settings.Address, settings.SoloMining, settings.ManualDiff, settings.MinPayout)
	return err
}

// GetMinerSettingsDB retrieves miner settings from the database
func GetMinerSettingsDB(address string) (*MinerSettings, error) {
	if db == nil {
		return nil, nil
	}

	var settings MinerSettings
	err := db.QueryRow(`
		SELECT address, solo_mining, manual_diff, min_payout
		FROM miners WHERE address = $1`,
		address).Scan(&settings.Address, &settings.SoloMining, &settings.ManualDiff, &settings.MinPayout)

	if err != nil {
		return nil, err
	}
	return &settings, nil
}

// LoadAllMinerSettings loads all miner settings from database
func LoadAllMinerSettings() map[string]*MinerSettings {
	result := make(map[string]*MinerSettings)
	if db == nil {
		return result
	}

	rows, err := db.Query(`SELECT address, solo_mining, manual_diff, min_payout FROM miners`)
	if err != nil {
		log.Printf("Error loading miner settings: %v", err)
		return result
	}
	defer rows.Close()

	for rows.Next() {
		var s MinerSettings
		if err := rows.Scan(&s.Address, &s.SoloMining, &s.ManualDiff, &s.MinPayout); err != nil {
			log.Printf("Warning: failed to scan miner settings: %v", err)
			continue
		}
		result[s.Address] = &s
	}

	if err := rows.Err(); err != nil {
		log.Printf("Warning: error iterating miner settings: %v", err)
	}

	log.Printf("✅ Loaded %d miner settings from database", len(result))
	return result
}

// MinerListEntry represents a miner in the list
type MinerListEntry struct {
	Address    string  `json:"address"`
	SoloMining bool    `json:"solo_mining"`
	Hashrate   float64 `json:"hashrate"`
}

// GetMinersListDB returns a list of miners from database with optional limit
func GetMinersListDB(limit int) []MinerListEntry {
	if db == nil {
		return []MinerListEntry{}
	}

	query := `SELECT address, solo_mining FROM miners ORDER BY updated_at DESC`
	if limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", limit)
	}

	rows, err := db.Query(query)
	if err != nil {
		log.Printf("Error getting miners list: %v", err)
		return []MinerListEntry{}
	}
	defer rows.Close()

	miners := make([]MinerListEntry, 0)
	for rows.Next() {
		var m MinerListEntry
		if err := rows.Scan(&m.Address, &m.SoloMining); err != nil {
			continue
		}
		miners = append(miners, m)
	}
	return miners
}

// GetMinerPayoutsDB returns payout history from database
func GetMinerPayoutsDB(minerID string) ([]PayoutRecord, int, float64) {
	if db == nil {
		return []PayoutRecord{}, 0, 0
	}

	// Get unique payouts grouped by txid (include pending_ prefixed txids for in-progress payouts)
	rows, err := db.Query(`
		SELECT txid, SUM(amount) as amount, MAX(paid_at) as paid_at, COUNT(*) as blocks,
		       CASE WHEN txid LIKE 'pending_%' THEN false ELSE true END as is_confirmed
		FROM payouts
		WHERE miner_address = $1
		  AND txid IS NOT NULL
		  AND txid != ''
		GROUP BY txid
		ORDER BY MAX(paid_at) DESC
		LIMIT 100`,
		minerID)
	if err != nil {
		log.Printf("Error getting payouts for %s: %v", minerID, err)
		return []PayoutRecord{}, 0, 0
	}
	defer rows.Close()

	var payouts []PayoutRecord
	var totalPaid float64

	for rows.Next() {
		var p PayoutRecord
		var paidAt sql.NullTime
		if err := rows.Scan(&p.TxID, &p.Amount, &paidAt, &p.Blocks, &p.Confirmed); err != nil {
			log.Printf("Warning: failed to scan payout record: %v", err)
			continue
		}
		if paidAt.Valid {
			p.PaidAt = paidAt.Time
		}
		// Only count confirmed payouts in totalPaid
		if p.Confirmed {
			totalPaid += p.Amount
		}
		payouts = append(payouts, p)
	}

	if err := rows.Err(); err != nil {
		log.Printf("Warning: error iterating payout records for %s: %v", minerID, err)
	}

	return payouts, len(payouts), totalPaid
}

// GetMinerSoloPayoutsDB returns payout history for solo blocks only
func GetMinerSoloPayoutsDB(minerID string) ([]PayoutRecord, int, float64) {
	if db == nil {
		return []PayoutRecord{}, 0, 0
	}

	// Get payouts only for solo blocks
	rows, err := db.Query(`
		SELECT p.txid, p.amount, p.paid_at, 1 as blocks,
		       CASE WHEN p.txid LIKE 'pending_%' THEN false ELSE true END as is_confirmed
		FROM payouts p
		JOIN blocks b ON b.height = p.block_height AND b.miner_address = p.miner_address
		WHERE p.miner_address = $1
		  AND b.is_solo = true
		  AND p.txid IS NOT NULL
		  AND p.txid != ''
		ORDER BY p.paid_at DESC NULLS LAST
		LIMIT 100`,
		minerID)
	if err != nil {
		log.Printf("Error getting solo payouts for %s: %v", minerID, err)
		return []PayoutRecord{}, 0, 0
	}
	defer rows.Close()

	var payouts []PayoutRecord
	var totalPaid float64

	for rows.Next() {
		var p PayoutRecord
		var paidAt sql.NullTime
		if err := rows.Scan(&p.TxID, &p.Amount, &paidAt, &p.Blocks, &p.Confirmed); err != nil {
			log.Printf("Warning: failed to scan solo payout record: %v", err)
			continue
		}
		if paidAt.Valid {
			p.PaidAt = paidAt.Time
		}
		if p.Confirmed {
			totalPaid += p.Amount
		}
		payouts = append(payouts, p)
	}

	return payouts, len(payouts), totalPaid
}

// SaveShare saves a PPLNS share to the database for reward distribution
func SaveShare(minerAddress string, workerName string, difficulty float64, actualDiff float64, isSolo bool) error {
	if db == nil {
		return nil
	}

	_, err := db.Exec(`
		INSERT INTO shares (miner_address, worker_name, difficulty, share_diff, is_solo)
		VALUES ($1, $2, $3, $4, $5)`,
		minerAddress, workerName, difficulty, actualDiff, isSolo)
	return err
}

// GetMinerBestDiff returns the best (highest) share difficulty for a miner from the database
func GetMinerBestDiff(minerAddress string) float64 {
	if db == nil {
		return 0
	}

	var bestDiff float64
	err := db.QueryRow(`
		SELECT COALESCE(MAX(share_diff), 0) FROM shares WHERE miner_address = $1`,
		minerAddress).Scan(&bestDiff)
	if err != nil {
		return 0
	}
	return bestDiff
}

// GetWorkerBestDiff returns the best (highest) share difficulty for a specific worker
func GetWorkerBestDiff(minerAddress string, workerName string) float64 {
	if db == nil {
		return 0
	}

	var bestDiff float64
	err := db.QueryRow(`
		SELECT COALESCE(MAX(share_diff), 0) FROM shares WHERE miner_address = $1 AND worker_name = $2`,
		minerAddress, workerName).Scan(&bestDiff)
	if err != nil {
		return 0
	}
	return bestDiff
}

// PPLNSShare represents a miner's share contribution in the PPLNS window
type PPLNSShare struct {
	MinerAddress string
	TotalWork    float64
}

// GetPPLNSShares returns the sum of difficulty per miner for the last N shares
// Returns a map of minerAddress -> total difficulty contributed
func GetPPLNSShares(windowSize int) (map[string]float64, float64, error) {
	if db == nil {
		return nil, 0, fmt.Errorf("database not initialized")
	}

	// Use context with timeout to prevent hung queries
	ctx, cancel := context.WithTimeout(context.Background(), DBTimeout)
	defer cancel()

	// Get the last N PPLNS shares (not solo) and sum by miner
	rows, err := db.QueryContext(ctx, `
		WITH recent_shares AS (
			SELECT miner_address, difficulty
			FROM shares
			WHERE is_solo = false
			ORDER BY id DESC
			LIMIT $1
		)
		SELECT miner_address, SUM(difficulty) as total_work
		FROM recent_shares
		GROUP BY miner_address`,
		windowSize)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to query PPLNS shares: %w", err)
	}
	defer rows.Close()

	result := make(map[string]float64)
	var totalWork float64

	for rows.Next() {
		var minerAddr string
		var work float64
		if err := rows.Scan(&minerAddr, &work); err != nil {
			log.Printf("Warning: failed to scan PPLNS share: %v", err)
			continue
		}
		result[minerAddr] = work
		totalWork += work
	}

	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("error iterating PPLNS shares: %w", err)
	}

	return result, totalWork, nil
}

// CleanupOldShares removes shares older than needed for PPLNS calculation
// Keeps 2x the window size as buffer
func CleanupOldShares(windowSize int) (int64, error) {
	if db == nil {
		return 0, nil
	}

	// Use context with timeout to prevent hung queries
	ctx, cancel := context.WithTimeout(context.Background(), DBTimeout)
	defer cancel()

	// Keep 2x window as buffer, delete older shares
	result, err := db.ExecContext(ctx, `
		DELETE FROM shares
		WHERE id < (
			SELECT MIN(id) FROM (
				SELECT id FROM shares
				ORDER BY id DESC
				LIMIT $1
			) AS recent
		)`,
		windowSize*2)
	if err != nil {
		return 0, fmt.Errorf("failed to cleanup old shares: %w", err)
	}

	deleted, _ := result.RowsAffected()
	return deleted, nil
}
