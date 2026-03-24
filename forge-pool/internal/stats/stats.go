package stats

import (
	"crypto/rand"
	"fmt"
	"sync"
	"time"
)

type ShareRecord struct {
	Time       time.Time
	Difficulty float64
}

// CircularShareBuffer is a fixed-size ring buffer for share records
// This prevents unbounded memory growth while maintaining O(1) insertion
// Thread-safe with internal mutex
type CircularShareBuffer struct {
	mu      sync.RWMutex
	records []ShareRecord
	head    int // Next write position
	size    int // Number of valid records
	cap     int // Maximum capacity
}

// NewCircularShareBuffer creates a new circular buffer with the given capacity
func NewCircularShareBuffer(capacity int) *CircularShareBuffer {
	return &CircularShareBuffer{
		records: make([]ShareRecord, capacity),
		cap:     capacity,
	}
}

// Add adds a share record to the buffer, overwriting oldest if full
func (b *CircularShareBuffer) Add(r ShareRecord) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.records[b.head] = r
	b.head = (b.head + 1) % b.cap
	if b.size < b.cap {
		b.size++
	}
}

// GetRecordsAfter returns all records after the given time
func (b *CircularShareBuffer) GetRecordsAfter(cutoff time.Time) []ShareRecord {
	b.mu.RLock()
	defer b.mu.RUnlock()
	result := make([]ShareRecord, 0, b.size)
	for i := 0; i < b.size; i++ {
		// Read from oldest to newest
		idx := (b.head - b.size + i + b.cap) % b.cap
		if b.records[idx].Time.After(cutoff) {
			result = append(result, b.records[idx])
		}
	}
	return result
}

// Size returns the number of records in the buffer
func (b *CircularShareBuffer) Size() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.size
}

type WorkerStats struct {
	MinerID       string               `json:"miner_id"`
	WorkerName    string               `json:"worker_name"`
	Online        bool                 `json:"online"`
	Hashrate5m    float64              `json:"hashrate_5m"`
	Hashrate60m   float64              `json:"hashrate_60m"`
	ValidShares   int64                `json:"valid_shares"`
	InvalidShares int64                `json:"invalid_shares"`
	BestDiff      float64              `json:"best_diff"`       // Best share this round (resets on block found)
	RoundBestDiff float64              `json:"round_best_diff"` // Alias for best_diff for UI compatibility
	ATHDiff       float64              `json:"ath_diff"`        // All-time high share difficulty
	TotalWork     float64              `json:"total_work"`      // Cumulative share difficulty for round effort
	BlocksFound   int64                `json:"blocks_found"`    // Number of blocks found by this worker
	LastShareAt   time.Time            `json:"last_share_at"`
	ConnectedAt   time.Time            `json:"connected_at"`
	ShareBuffer   *CircularShareBuffer `json:"-"` // Use circular buffer instead of slice
}

type PoolStats struct {
	Hashrate      float64   `json:"hashrate"`
	Workers       int       `json:"workers"`
	BlocksFound   int64     `json:"blocks_found"`
	LastBlockAt   time.Time `json:"last_block_at"`
	LastBlockHash string    `json:"last_block_hash"`
	RoundEffort   float64   `json:"round_effort"`   // Cumulative share difficulty this round
	AvgLuck       float64   `json:"avg_luck"`       // Average luck over recent blocks (0-1 scale, <1 is lucky)
}

type StatsManager struct {
	workers     map[string]*WorkerStats
	poolStats   PoolStats
	roundEffort float64       // Cumulative share difficulty for current round
	luckHistory []float64     // Recent block luck values (capped at 100)
	mu          sync.RWMutex
}

var (
	manager *StatsManager
	once    sync.Once
)

func GetManager() *StatsManager {
	once.Do(func() {
		manager = &StatsManager{
			workers: make(map[string]*WorkerStats),
		}
	})
	return manager
}

func (m *StatsManager) UpdateWorker(minerID, workerName string, valid bool, targetDiff, actualDiff float64) {
	m.mu.Lock()
	defer m.mu.Unlock()

	key := minerID + ":" + workerName
	w, exists := m.workers[key]
	if !exists {
		// Load best diff from database for this worker (survives restarts)
		dbBestDiff := GetWorkerBestDiff(minerID, workerName)
		w = &WorkerStats{
			MinerID:       minerID,
			WorkerName:    workerName,
			ConnectedAt:   time.Now(),
			ShareBuffer:   NewCircularShareBuffer(MaxSharesPerWorker), // Fixed-size buffer
			BestDiff:      dbBestDiff,                                  // Load from DB
			RoundBestDiff: dbBestDiff,                                  // Load from DB
			ATHDiff:       dbBestDiff,                                  // ATH is at least the DB best
		}
		m.workers[key] = w
	}

	w.Online = true
	w.LastShareAt = time.Now()

	// Add share to circular buffer (O(1) operation, no memory allocation)
	// Use target difficulty for hashrate calculation (consistent work credit)
	w.ShareBuffer.Add(ShareRecord{
		Time:       time.Now(),
		Difficulty: targetDiff,
	})

	if valid {
		w.ValidShares++
		w.TotalWork += targetDiff // Accumulate work for round effort calculation
		m.roundEffort += targetDiff // Track pool-wide round effort for luck calculation
		// Use ACTUAL share difficulty for best share tracking
		if actualDiff > w.BestDiff {
			w.BestDiff = actualDiff
			w.RoundBestDiff = actualDiff // Keep in sync for UI compatibility
		}
		if actualDiff > w.ATHDiff {
			w.ATHDiff = actualDiff
		}
	} else {
		w.InvalidShares++
	}

	// Calculate hashrates using shares from the last hour
	shares := w.ShareBuffer.GetRecordsAfter(time.Now().Add(-ShareHistoryDuration))
	w.Hashrate5m = m.calculateHashrate(shares, 5*time.Minute)
	w.Hashrate60m = m.calculateHashrate(shares, 60*time.Minute)
}

func (m *StatsManager) calculateHashrate(shares []ShareRecord, window time.Duration) float64 {
	cutoff := time.Now().Add(-window)
	
	var totalWork float64
	for _, s := range shares {
		if s.Time.After(cutoff) {
			totalWork += s.Difficulty
		}
	}

	if totalWork == 0 {
		return 0
	}

	seconds := window.Seconds()
	// Hashrate = total_difficulty * 2^32 / seconds
	// Result in TH/s
	hashrate := totalWork * 4294967296.0 / seconds / 1e12
	return hashrate
}

func (m *StatsManager) SetWorkerOffline(minerID, workerName string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	key := minerID + ":" + workerName
	if w, exists := m.workers[key]; exists {
		w.Online = false
	}
}

// MarkStaleWorkersOffline marks workers as offline if they haven't submitted shares recently
func (m *StatsManager) MarkStaleWorkersOffline() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	cutoff := time.Now().Add(-WorkerOfflineThreshold)
	count := 0

	for _, w := range m.workers {
		if w.Online && w.LastShareAt.Before(cutoff) {
			w.Online = false
			count++
		}
	}

	return count
}

// StartWorkerTimeoutChecker starts a background goroutine to mark stale workers offline
func (m *StatsManager) StartWorkerTimeoutChecker(stopCh <-chan struct{}) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
			if count := m.MarkStaleWorkersOffline(); count > 0 {
				// Log handled by caller if needed
			}
		}
	}
}

func (m *StatsManager) ResetWorkerRoundStats(minerID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for key, w := range m.workers {
		if w.MinerID == minerID {
			m.workers[key].BestDiff = 0
			m.workers[key].RoundBestDiff = 0
			m.workers[key].TotalWork = 0 // Reset round effort tracking
		}
	}
}

// ResetAllWorkerRoundStats resets round stats for all workers (called after block found)
func (m *StatsManager) ResetAllWorkerRoundStats() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for key := range m.workers {
		m.workers[key].BestDiff = 0
		m.workers[key].RoundBestDiff = 0
		m.workers[key].TotalWork = 0
	}
}

func (m *StatsManager) GetWorkerStats(minerID string) []*WorkerStats {
	m.mu.RLock()
	defer m.mu.RUnlock()

	cutoff := time.Now().Add(-5 * time.Minute)

	var result []*WorkerStats
	for _, w := range m.workers {
		if w.MinerID == minerID {
			wCopy := *w
			wCopy.Online = w.LastShareAt.After(cutoff)
			// Get block count for this worker
			wCopy.BlocksFound = GetWorkerBlockCount(w.MinerID, w.WorkerName)
			result = append(result, &wCopy)
		}
	}
	return result
}

func (m *StatsManager) GetAllWorkerStats() []*WorkerStats {
	m.mu.RLock()
	defer m.mu.RUnlock()

	cutoff := time.Now().Add(-5 * time.Minute)

	var result []*WorkerStats
	for _, w := range m.workers {
		wCopy := *w
		wCopy.Online = w.LastShareAt.After(cutoff)
		// Get block count for this worker
		wCopy.BlocksFound = GetWorkerBlockCount(w.MinerID, w.WorkerName)
		result = append(result, &wCopy)
	}
	return result
}

func (m *StatsManager) RecordBlock(hash string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.poolStats.BlocksFound++
	m.poolStats.LastBlockAt = time.Now()
	m.poolStats.LastBlockHash = hash
}

// RecordBlockWithEffort records a block and calculates luck based on effort vs network difficulty
func (m *StatsManager) RecordBlockWithEffort(hash string, networkDiff float64) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.poolStats.BlocksFound++
	m.poolStats.LastBlockAt = time.Now()
	m.poolStats.LastBlockHash = hash

	// Calculate luck for this block
	// Luck = expected effort (network diff) / actual effort (round shares)
	// <1 means lucky (found faster), >1 means unlucky (took longer)
	if m.roundEffort > 0 && networkDiff > 0 {
		blockLuck := networkDiff / m.roundEffort
		m.luckHistory = append(m.luckHistory, blockLuck)
		// Keep only last 100 blocks
		if len(m.luckHistory) > 100 {
			m.luckHistory = m.luckHistory[len(m.luckHistory)-100:]
		}
	}

	// Store current round effort before resetting
	m.poolStats.RoundEffort = m.roundEffort
	// Reset round effort for next block
	m.roundEffort = 0
}

// GetAverageLuck returns the average luck over recent blocks
func (m *StatsManager) GetAverageLuck() float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if len(m.luckHistory) == 0 {
		return 1.0 // Default to 100% (neutral luck)
	}

	var sum float64
	for _, luck := range m.luckHistory {
		sum += luck
	}
	return sum / float64(len(m.luckHistory))
}

// GetRoundEffort returns current round effort (share difficulty sum)
func (m *StatsManager) GetRoundEffort() float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.roundEffort
}

func (m *StatsManager) GetPoolStats() PoolStats {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var totalHashrate float64
	var onlineWorkers int
	cutoff := time.Now().Add(-5 * time.Minute)

	for _, w := range m.workers {
		if w.LastShareAt.After(cutoff) {
			totalHashrate += w.Hashrate60m
			onlineWorkers++
		}
	}

	// Calculate average luck
	var avgLuck float64 = 1.0 // Default to 100%
	if len(m.luckHistory) > 0 {
		var sum float64
		for _, luck := range m.luckHistory {
			sum += luck
		}
		avgLuck = sum / float64(len(m.luckHistory))
	}

	return PoolStats{
		Hashrate:      totalHashrate,
		Workers:       onlineWorkers,
		BlocksFound:   m.poolStats.BlocksFound,
		LastBlockAt:   m.poolStats.LastBlockAt,
		LastBlockHash: m.poolStats.LastBlockHash,
		RoundEffort:   m.roundEffort,
		AvgLuck:       avgLuck,
	}
}

// Block tracking per miner
type MinerBlock struct {
	Height      int64     `json:"height"`
	Hash        string    `json:"hash"`
	MinerID     string    `json:"miner_id"`
	WorkerName  string    `json:"worker_name"`
	Reward      float64   `json:"reward"`
	Time        time.Time `json:"time"`
	Confirmed   bool      `json:"confirmed"`
	PayoutTxid  string    `json:"payoutTxid,omitempty"`
}

var (
	minerBlocks      = make(map[string][]MinerBlock) // minerID -> blocks
	workerBlockCount = make(map[string]int64)        // minerID:workerName -> block count
	minerBlocksMu    sync.RWMutex
)

func RecordMinerBlock(minerID string, height int64, hash string, reward float64) {
	RecordMinerBlockWithWorker(minerID, "", height, hash, reward)
}

func RecordMinerBlockWithWorker(minerID, workerName string, height int64, hash string, reward float64) {
	RecordMinerBlockWithWorkerSolo(minerID, workerName, height, hash, reward, false)
}

func RecordMinerBlockWithWorkerSolo(minerID, workerName string, height int64, hash string, reward float64, isSolo bool) {
	minerBlocksMu.Lock()
	defer minerBlocksMu.Unlock()

	block := MinerBlock{
		Height:     height,
		Hash:       hash,
		MinerID:    minerID,
		WorkerName: workerName,
		Reward:     reward,
		Time:       time.Now(),
		Confirmed:  false,
	}

	minerBlocks[minerID] = append(minerBlocks[minerID], block)

	// Track per-worker block count
	if workerName != "" {
		key := minerID + ":" + workerName
		workerBlockCount[key]++
	}

	// Keep only last 1000 blocks per miner
	if len(minerBlocks[minerID]) > 1000 {
		minerBlocks[minerID] = minerBlocks[minerID][len(minerBlocks[minerID])-1000:]
	}

	// Save to database with is_solo flag
	SaveBlockDBWithSolo(minerID, height, hash, reward, isSolo)
}

// GetWorkerBlockCount returns the number of blocks found by a specific worker
func GetWorkerBlockCount(minerID, workerName string) int64 {
	minerBlocksMu.RLock()
	defer minerBlocksMu.RUnlock()
	key := minerID + ":" + workerName
	return workerBlockCount[key]
}

func GetMinerBlocks(minerID string) []MinerBlock {
	minerBlocksMu.RLock()
	defer minerBlocksMu.RUnlock()

	blocks, exists := minerBlocks[minerID]
	if !exists {
		return []MinerBlock{}
	}
	
	// Return copy in reverse order (newest first)
	result := make([]MinerBlock, len(blocks))
	for i, b := range blocks {
		result[len(blocks)-1-i] = b
	}
	return result
}

// Payout tracking (COINBASE_MATURITY is in constants.go)

type PendingPayout struct {
	ID          string    `json:"id"`         // Unique identifier (UUID)
	MinerID     string    `json:"miner_id"`
	BlockHeight int64     `json:"block_height"`
	Amount      float64   `json:"amount"`
	PaidAmount  float64   `json:"paid_amount"` // Track partial payments for split payouts
	Confirmed   bool      `json:"confirmed"`
	TxIDs       []string  `json:"txids"`       // Multiple txids for split payouts
	TxID        string    `json:"txid"`        // Primary/last txid (backwards compat)
	CreatedAt   time.Time `json:"created_at"`
	PaidAt      time.Time `json:"paid_at,omitempty"`
}

// generateUUID creates a simple UUID for payout tracking
func generateUUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

var (
	pendingPayouts   = make(map[string][]PendingPayout)
	pendingPayoutsMu sync.RWMutex
)

func AddPendingPayout(minerID string, blockHeight int64, amount float64) {
	pendingPayoutsMu.Lock()
	defer pendingPayoutsMu.Unlock()

	payout := PendingPayout{
		ID:          generateUUID(),
		MinerID:     minerID,
		BlockHeight: blockHeight,
		Amount:      amount,
		PaidAmount:  0,
		Confirmed:   false,
		TxIDs:       []string{},
		CreatedAt:   time.Now(),
	}

	pendingPayouts[minerID] = append(pendingPayouts[minerID], payout)
}

func GetMinerBalance(minerID string, currentHeight int64) (mature float64, immature float64) {
	pendingPayoutsMu.RLock()
	defer pendingPayoutsMu.RUnlock()

	payouts, exists := pendingPayouts[minerID]
	if !exists {
		return 0, 0
	}

	for _, p := range payouts {
		if p.TxID != "" {
			continue
		}
		confirmations := currentHeight - p.BlockHeight
		if confirmations >= COINBASE_MATURITY {
			mature += p.Amount
		} else {
			immature += p.Amount
		}
	}
	return
}

func GetPendingPayouts(minerID string) []PendingPayout {
	pendingPayoutsMu.RLock()
	defer pendingPayoutsMu.RUnlock()

	payouts, exists := pendingPayouts[minerID]
	if !exists {
		return []PendingPayout{}
	}

	result := make([]PendingPayout, len(payouts))
	copy(result, payouts)
	return result
}

func MarkPayoutPaid(minerID string, blockHeight int64, txid string) {
	pendingPayoutsMu.Lock()
	defer pendingPayoutsMu.Unlock()

	payouts, exists := pendingPayouts[minerID]
	if !exists {
		return
	}

	for i, p := range payouts {
		if p.BlockHeight == blockHeight {
			payouts[i].TxID = txid
			payouts[i].PaidAt = time.Now()
			payouts[i].Confirmed = true
		}
	}
	pendingPayouts[minerID] = payouts
}

// ProcessPayouts checks for mature balances and sends payouts
// Returns list of addresses that need payouts and their amounts
func GetReadyPayouts(currentHeight int64, minPayout float64) map[string]float64 {
	pendingPayoutsMu.RLock()
	defer pendingPayoutsMu.RUnlock()

	ready := make(map[string]float64)

	for minerID, payouts := range pendingPayouts {
		var matureAmount float64
		for _, p := range payouts {
			// Skip fully paid payouts
			if p.Confirmed {
				continue
			}
			// Calculate unpaid portion (for split payouts)
			unpaid := p.Amount - p.PaidAmount
			if unpaid <= 0 {
				continue
			}
			confirmations := currentHeight - p.BlockHeight
			if confirmations >= COINBASE_MATURITY {
				matureAmount += unpaid
			}
		}
		if matureAmount >= minPayout {
			ready[minerID] = matureAmount
		}
	}
	return ready
}

// MarkAllMaturePaid marks all mature payouts for a miner as paid
func MarkAllMaturePaid(minerID string, currentHeight int64, txid string) {
	MarkMaturePaidWithAmount(minerID, currentHeight, txid, 0)
}

// MarkMaturePaidWithAmount marks mature payouts as paid, supporting partial/split payments
// If paidAmount > 0, it tracks partial payment; otherwise marks all mature as fully paid
func MarkMaturePaidWithAmount(minerID string, currentHeight int64, txid string, paidAmount float64) {
	pendingPayoutsMu.Lock()
	defer pendingPayoutsMu.Unlock()

	payouts, exists := pendingPayouts[minerID]
	if !exists {
		return
	}

	remainingPayment := paidAmount
	now := time.Now()

	for i, p := range payouts {
		// Skip already fully paid
		if p.Confirmed {
			continue
		}
		confirmations := currentHeight - p.BlockHeight
		if confirmations < COINBASE_MATURITY {
			continue
		}

		// Add txid to list
		payouts[i].TxIDs = append(payouts[i].TxIDs, txid)
		payouts[i].TxID = txid // Keep last txid for backwards compat

		if paidAmount == 0 {
			// Full payment mode - mark all mature as paid
			payouts[i].PaidAmount = p.Amount
			payouts[i].PaidAt = now
			payouts[i].Confirmed = true
		} else {
			// Partial payment mode - track amounts
			unpaidAmount := p.Amount - p.PaidAmount
			if remainingPayment >= unpaidAmount {
				// Fully pay this payout
				payouts[i].PaidAmount = p.Amount
				payouts[i].PaidAt = now
				payouts[i].Confirmed = true
				remainingPayment -= unpaidAmount
			} else if remainingPayment > 0 {
				// Partial payment
				payouts[i].PaidAmount += remainingPayment
				remainingPayment = 0
			}
			if remainingPayment <= 0 {
				break
			}
		}
	}
	pendingPayouts[minerID] = payouts
}

// GetDustBalances returns miners with mature balances below min payout threshold
// This helps track dust accumulation for monitoring/reporting
func GetDustBalances(currentHeight int64, minPayout float64) map[string]float64 {
	pendingPayoutsMu.RLock()
	defer pendingPayoutsMu.RUnlock()

	dust := make(map[string]float64)

	for minerID, payouts := range pendingPayouts {
		var matureAmount float64
		for _, p := range payouts {
			if p.Confirmed {
				continue
			}
			// Check for partial payments
			unpaid := p.Amount - p.PaidAmount
			if unpaid <= 0 {
				continue
			}
			confirmations := currentHeight - p.BlockHeight
			if confirmations >= COINBASE_MATURITY {
				matureAmount += unpaid
			}
		}
		// Only include if has balance but below min payout
		if matureAmount > 0 && matureAmount < minPayout {
			dust[minerID] = matureAmount
		}
	}
	return dust
}

// GetTotalDust returns the total dust amount across all miners
func GetTotalDust(currentHeight int64, minPayout float64) float64 {
	dust := GetDustBalances(currentHeight, minPayout)
	var total float64
	for _, amount := range dust {
		total += amount
	}
	return total
}

// CleanupPaidPayouts removes old paid payouts from memory to prevent unbounded growth
// Retains payouts for 24 hours after payment to allow UI to display recent transactions
func CleanupPaidPayouts() {
	pendingPayoutsMu.Lock()
	defer pendingPayoutsMu.Unlock()

	cutoff := time.Now().Add(-24 * time.Hour)
	totalRemoved := 0

	for minerID, payouts := range pendingPayouts {
		// Filter to keep only unpaid or recently paid
		filtered := make([]PendingPayout, 0, len(payouts))
		for _, p := range payouts {
			// Keep if unpaid OR paid within last 24 hours
			if p.TxID == "" || p.PaidAt.After(cutoff) {
				filtered = append(filtered, p)
			} else {
				totalRemoved++
			}
		}

		if len(filtered) == 0 {
			delete(pendingPayouts, minerID)
		} else if len(filtered) < len(payouts) {
			pendingPayouts[minerID] = filtered
		}
	}
}
