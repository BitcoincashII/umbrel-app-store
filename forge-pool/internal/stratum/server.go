package stratum

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

const (
	// DifficultyGracePeriod is the time window after a difficulty change
	// during which shares at the old difficulty are still accepted
	DifficultyGracePeriod = 30 * time.Second

	// MaxDifficultyMultiplier caps how much difficulty can change in one adjustment
	MaxDifficultyMultiplier = 1.5 // Max 50% change per adjustment (miningcore style)

	// VardiffVariancePercent - only adjust if outside this variance from target
	VardiffVariancePercent = 0.30 // 30% variance allowed

	// VardiffMinShares is the minimum shares needed before vardiff adjusts
	VardiffMinShares = 10

	// RecentSubmissionsWindow is the number of submissions to track for rejection rate
	RecentSubmissionsWindow = 50

	// MaxRejectionRate is the rejection rate above which difficulty is reduced
	// Set higher (30%) to allow natural statistical variance in share difficulty
	MaxRejectionRate = 0.30 // 30%

	// DifficultyReductionCooldown is how long to wait before increasing above the ceiling
	DifficultyReductionCooldown = 2 * time.Minute
)

type Server struct {
	config         *ServerConfig
	logger         *zap.Logger
	listener       net.Listener
	clients        sync.Map
	clientCount    int64
	currentJob     atomic.Value
	jobHistory     sync.Map
	extraNonce     uint32
	extraNonceMu   sync.Mutex
	shareProcessor ShareProcessor
	minerSettings  MinerSettingsStore
	shutdownCh     chan struct{}
	stats          *ServerStats
	// Duplicate share detection
	submittedShares sync.Map // map[shareKey]time.Time
	shareCleanupMu  sync.Mutex
}

// shareKey uniquely identifies a submitted share
type shareKey struct {
	JobID       string
	ExtraNonce1 string
	ExtraNonce2 string
	NTime       string
	Nonce       string
}

type ServerConfig struct {
	Host               string
	Port               int
	MaxConnections     int
	BanDuration        time.Duration
	MaxSharesPerSecond int
	VardiffEnabled     bool
	MinDiff            float64
	RentalMinDiff      float64 // Minimum difficulty for NiceHash/MRR (they require 500k+)
	RentalMaxDiff      float64 // Maximum difficulty for NiceHash/MRR (cap to prevent issues)
	MaxDiff            float64
	TargetShareTime    int
	RetargetTime       int // Seconds between vardiff adjustments
	HighHashThreshold  int
	HighHashDiff       float64
	ExtraNonce2Size    int    // Size of extranonce2 in bytes (default 4, Braiins needs 8)
	ExtraNonce1Size    int    // Size of extranonce1 in bytes (default 6)
	ServerName         string // Name for logging (e.g., "main", "braiins")
}

type ServerStats struct {
	TotalConnections  int64
	ActiveConnections int64
	TotalShares       int64
	ValidShares       int64
	InvalidShares     int64
	BlocksFound       int64
	SoloMiners        int64
	PPLNSMiners       int64
}

type ShareProcessor interface {
	ProcessShare(ctx context.Context, share *Share) error
	ProcessBlock(ctx context.Context, block *Block) error
}

type MinerSettingsStore interface {
	GetMinerSettings(minerID string) (*MinerSettings, error)
}

type MinerSettings struct {
	MinerID    string
	SoloMining bool
	ManualDiff float64
	MinPayout  float64
}

func NewServer(config *ServerConfig, logger *zap.Logger, sp ShareProcessor, ms MinerSettingsStore) *Server {
	if config.HighHashThreshold == 0 {
		config.HighHashThreshold = 10
	}
	if config.HighHashDiff == 0 {
		config.HighHashDiff = 1000000
	}
	if config.MinDiff == 0 {
		config.MinDiff = 32768
	}
	if config.RentalMinDiff == 0 {
		config.RentalMinDiff = 500000 // NiceHash/MRR require 500k+
	}

	s := &Server{
		config:         config,
		logger:         logger,
		shareProcessor: sp,
		minerSettings:  ms,
		shutdownCh:     make(chan struct{}),
		stats:          &ServerStats{},
	}

	// Start periodic share cleanup
	go s.shareCleanupLoop()

	return s
}

// getMinDiffForClient returns the appropriate minimum difficulty based on client type
// Rental services (NiceHash/MRR) require higher minimum difficulty
func (s *Server) getMinDiffForClient(client *Client) float64 {
	client.mu.RLock()
	rental := client.RentalService
	client.mu.RUnlock()

	if rental != RentalNone {
		return s.config.RentalMinDiff
	}
	return s.config.MinDiff
}

// shareCleanupLoop periodically removes old share entries
func (s *Server) shareCleanupLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-s.shutdownCh:
			return
		case <-ticker.C:
			s.cleanupOldShares()
		}
	}
}

// cleanupOldShares removes shares older than 5 minutes
func (s *Server) cleanupOldShares() {
	cutoff := time.Now().Add(-5 * time.Minute)
	s.submittedShares.Range(func(key, value interface{}) bool {
		if t, ok := value.(time.Time); ok && t.Before(cutoff) {
			s.submittedShares.Delete(key)
		}
		return true
	})
}

// clearSharesForJob performs cleanup when a new job is broadcast
// SECURITY: Don't wipe all shares - this would allow replay attacks
// Instead, do time-based cleanup to remove old shares while keeping recent ones
func (s *Server) clearSharesForJob() {
	s.shareCleanupMu.Lock()
	defer s.shareCleanupMu.Unlock()
	// Only remove shares older than 2 minutes to prevent replay while allowing
	// legitimate shares from recent jobs that miners might still be working on
	cutoff := time.Now().Add(-2 * time.Minute)
	s.submittedShares.Range(func(key, value interface{}) bool {
		if t, ok := value.(time.Time); ok && t.Before(cutoff) {
			s.submittedShares.Delete(key)
		}
		return true
	})
}

// isDuplicateShare checks if this share was already submitted
func (s *Server) isDuplicateShare(jobID, en1, en2, ntime, nonce string) bool {
	key := shareKey{
		JobID:       jobID,
		ExtraNonce1: en1,
		ExtraNonce2: en2,
		NTime:       ntime,
		Nonce:       nonce,
	}

	_, exists := s.submittedShares.LoadOrStore(key, time.Now())
	return exists
}

// validateShare verifies that the submitted share meets the difficulty target
// Returns: (isValid bool, actualDifficulty float64, blockHash []byte, error)

// calculateMerkleRoot computes the merkle root from coinbase and branches
func calculateMerkleRoot(coinbase []byte, merkleBranches []string) []byte {
	result := doubleSHA256(coinbase)
	for _, branchHex := range merkleBranches {
		branch, _ := hex.DecodeString(branchHex)
		combined := make([]byte, 64)
		copy(combined[:32], result)
		copy(combined[32:], branch)
		result = doubleSHA256(combined)
	}
	return result
}

func (s *Server) validateShare(job *Job, extranonce1, extranonce2, ntime, nonce, versionBits string, targetDiff float64) (bool, float64, []byte, error) {
	// Validate hex input lengths based on configured sizes
	en1Size := s.config.ExtraNonce1Size
	if en1Size == 0 {
		en1Size = 6
	}
	en2Size := s.config.ExtraNonce2Size
	if en2Size == 0 {
		en2Size = 4
	}
	if len(extranonce1) != en1Size*2 || len(extranonce2) != en2Size*2 {
		return false, 0, nil, fmt.Errorf("invalid extranonce length (got en1=%d, en2=%d, want en1=%d, en2=%d)",
			len(extranonce1)/2, len(extranonce2)/2, en1Size, en2Size)
	}
	if len(ntime) != 8 || len(nonce) != 8 {
		return false, 0, nil, fmt.Errorf("invalid ntime/nonce length")
	}

	// Validate hex format
	if _, err := hex.DecodeString(extranonce1); err != nil {
		return false, 0, nil, fmt.Errorf("invalid extranonce1 hex")
	}
	if _, err := hex.DecodeString(extranonce2); err != nil {
		return false, 0, nil, fmt.Errorf("invalid extranonce2 hex")
	}
	if _, err := hex.DecodeString(ntime); err != nil {
		return false, 0, nil, fmt.Errorf("invalid ntime hex")
	}
	if _, err := hex.DecodeString(nonce); err != nil {
		return false, 0, nil, fmt.Errorf("invalid nonce hex")
	}

	// Build coinbase transaction
	coinbase := buildCoinbaseFromParts(job.CoinBase1, extranonce1, extranonce2, job.CoinBase2)

	// Calculate merkle root (for single tx, it's just double SHA256 of coinbase)
	merkleRoot := calculateMerkleRoot(coinbase, job.MerkleBranches)

	// Build block header (80 bytes)
	header := buildBlockHeader(job, merkleRoot, ntime, nonce, versionBits)

	// Calculate block hash (double SHA256 of header)
	blockHash := doubleSHA256(header)

	// Reverse for display (Bitcoin uses little-endian internally)
	displayHash := make([]byte, 32)
	copy(displayHash, blockHash)
	reverseBytes(displayHash)

	// Calculate difficulty from hash
	actualDiff := hashToDifficulty(blockHash)

	// Check if meets target difficulty
	isValid := actualDiff >= targetDiff

	return isValid, actualDiff, displayHash, nil
}

// buildCoinbaseFromParts constructs the coinbase transaction
func buildCoinbaseFromParts(cb1, en1, en2, cb2 string) []byte {
	cb1Bytes, _ := hex.DecodeString(cb1)
	en1Bytes, _ := hex.DecodeString(en1)
	en2Bytes, _ := hex.DecodeString(en2)
	cb2Bytes, _ := hex.DecodeString(cb2)

	var coinbase bytes.Buffer
	coinbase.Write(cb1Bytes)
	coinbase.Write(en1Bytes)
	coinbase.Write(en2Bytes)
	coinbase.Write(cb2Bytes)

	return coinbase.Bytes()
}

// buildBlockHeader constructs the 80-byte block header
func buildBlockHeader(job *Job, merkleRoot []byte, ntime, nonce, versionBits string) []byte {
	var header bytes.Buffer

	// Version (4 bytes, little-endian)
	versionBytes, _ := hex.DecodeString(job.Version)
	if versionBits != "" {
		vbBytes, _ := hex.DecodeString(versionBits)
		for i := 0; i < len(versionBytes) && i < len(vbBytes); i++ {
			versionBytes[i] ^= vbBytes[i]
		}
	}
	reverseBytes(versionBytes)
	header.Write(versionBytes)

	// Previous block hash (32 bytes)
	// job.PrevBlockHash is in stratum format, need to reverse back
	prevHashBytes, _ := hex.DecodeString(job.PrevBlockHash)
	// Undo the 4-byte swap that stratum does
	for i := 0; i < 32; i += 4 {
		prevHashBytes[i], prevHashBytes[i+1], prevHashBytes[i+2], prevHashBytes[i+3] =
			prevHashBytes[i+3], prevHashBytes[i+2], prevHashBytes[i+1], prevHashBytes[i]
	}
	header.Write(prevHashBytes)

	// Merkle root (32 bytes)
	header.Write(merkleRoot)

	// Time (4 bytes, little-endian)
	ntimeBytes, _ := hex.DecodeString(ntime)
	reverseBytes(ntimeBytes)
	header.Write(ntimeBytes)

	// Bits (4 bytes, little-endian)
	bitsBytes, _ := hex.DecodeString(job.NBits)
	reverseBytes(bitsBytes)
	header.Write(bitsBytes)

	// Nonce (4 bytes, little-endian)
	nonceBytes, _ := hex.DecodeString(nonce)
	reverseBytes(nonceBytes)
	header.Write(nonceBytes)

	return header.Bytes()
}

// doubleSHA256 performs double SHA256 hash
func doubleSHA256(data []byte) []byte {
	first := sha256.Sum256(data)
	second := sha256.Sum256(first[:])
	return second[:]
}

// reverseBytes reverses a byte slice in place
func reverseBytes(b []byte) {
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
}

// hashToDifficulty converts a block hash to its difficulty value
func hashToDifficulty(hash []byte) float64 {
	// Difficulty 1 target (Bitcoin)
	// 0x00000000FFFF0000000000000000000000000000000000000000000000000000
	diff1Target := new(big.Int)
	diff1Target.SetString("00000000FFFF0000000000000000000000000000000000000000000000000000", 16)

	// Convert hash to big.Int (hash is in internal byte order, need to reverse for big.Int)
	hashReversed := make([]byte, 32)
	copy(hashReversed, hash)
	reverseBytes(hashReversed)

	hashInt := new(big.Int).SetBytes(hashReversed)

	if hashInt.Sign() == 0 {
		return 0
	}

	// Difficulty = diff1Target / hashInt
	// Use floating point for precision
	diff1Float := new(big.Float).SetInt(diff1Target)
	hashFloat := new(big.Float).SetInt(hashInt)

	result := new(big.Float).Quo(diff1Float, hashFloat)
	difficulty, _ := result.Float64()

	return difficulty
}

// difficultyToTarget converts a difficulty value to a target for comparison
func difficultyToTarget(diff float64) *big.Int {
	if diff <= 0 {
		return new(big.Int)
	}

	diff1Target := new(big.Int)
	diff1Target.SetString("00000000FFFF0000000000000000000000000000000000000000000000000000", 16)

	// Target = diff1Target / difficulty
	diff1Float := new(big.Float).SetInt(diff1Target)
	diffFloat := new(big.Float).SetFloat64(diff)

	targetFloat := new(big.Float).Quo(diff1Float, diffFloat)
	targetInt, _ := targetFloat.Int(nil)

	return targetInt
}

func (s *Server) Start() error {
	addr := fmt.Sprintf("%s:%d", s.config.Host, s.config.Port)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("failed to listen: %w", err)
	}
	s.listener = listener
	s.logger.Info("Stratum server started", zap.String("addr", addr))
	go s.acceptLoop()
	return nil
}

func (s *Server) Stop() {
	s.logger.Info("Initiating graceful shutdown...")

	// Signal all goroutines to stop
	close(s.shutdownCh)

	// Stop accepting new connections
	if s.listener != nil {
		s.listener.Close()
	}

	// Wait for active clients to disconnect (with timeout)
	shutdownDeadline := time.Now().Add(30 * time.Second)
	for atomic.LoadInt64(&s.clientCount) > 0 {
		if time.Now().After(shutdownDeadline) {
			s.logger.Warn("Shutdown timeout reached, forcing disconnect",
				zap.Int64("remaining_clients", atomic.LoadInt64(&s.clientCount)))
			// Force close all client connections
			s.clients.Range(func(key, value interface{}) bool {
				client := value.(*Client)
				client.Conn.Close()
				return true
			})
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	s.logger.Info("Graceful shutdown complete")
}

func (s *Server) acceptLoop() {
	for {
		select {
		case <-s.shutdownCh:
			return
		default:
		}
		conn, err := s.listener.Accept()
		if err != nil {
			continue
		}
		if atomic.LoadInt64(&s.clientCount) >= int64(s.config.MaxConnections) {
			conn.Close()
			continue
		}
		go s.handleClient(conn)
	}
}

func (s *Server) handleClient(conn net.Conn) {
	// Configure TCP connection for mining
	if tc, ok := conn.(*net.TCPConn); ok {
		// Disable Nagle algorithm for low-latency share responses
		tc.SetNoDelay(true)
		// Enable TCP keepalive to detect dead connections and prevent NAT timeout
		// This is critical for rental services (NiceHash, MRR) that may have
		// intermediate proxies/firewalls that drop idle connections
		tc.SetKeepAlive(true)
		tc.SetKeepAlivePeriod(30 * time.Second) // Check every 30 seconds
	}

	atomic.AddInt64(&s.clientCount, 1)
	atomic.AddInt64(&s.stats.ActiveConnections, 1)
	defer func() {
		atomic.AddInt64(&s.clientCount, -1)
		atomic.AddInt64(&s.stats.ActiveConnections, -1)
		conn.Close()
	}()

	client := &Client{
		ID:          fmt.Sprintf("%d", time.Now().UnixNano()),
		Conn:        conn,
		IP:          conn.RemoteAddr().String(),
		Difficulty:  s.config.MinDiff,
		ConnectedAt: time.Now(),
		ShareTimes:  make([]time.Time, 0, 100),
	}

	s.clients.Store(client.ID, client)
	defer s.clients.Delete(client.ID)

	// Log external connections at Info level for debugging
	if !strings.HasPrefix(client.IP, "127.0.0.1") {
		s.logger.Info("External client connected", zap.String("ip", client.IP))
	} else {
		s.logger.Debug("Client connected", zap.String("ip", client.IP))
	}

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 64*1024), 64*1024)

	for scanner.Scan() {
		select {
		case <-s.shutdownCh:
			return
		default:
		}
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		s.handleMessage(client, line)
	}

	// Log disconnection with details
	client.mu.RLock()
	minerID := client.MinerID
	workerName := client.WorkerName
	rental := client.RentalService
	authorized := client.Authorized
	subscribed := client.Subscribed
	difficulty := client.Difficulty
	client.mu.RUnlock()

	duration := time.Since(client.ConnectedAt)
	scanErr := scanner.Err()

	// Log external connections that never subscribed
	if !strings.HasPrefix(client.IP, "127.0.0.1") && !subscribed {
		s.logger.Warn("External client disconnected without subscribing",
			zap.String("ip", client.IP),
			zap.Duration("connected_duration", duration),
			zap.Error(scanErr))
	}

	if authorized && minerID != "" {
		s.logger.Info("Client disconnected",
			zap.String("miner", minerID),
			zap.String("worker", workerName),
			zap.String("rental_service", rental.String()),
			zap.Float64("difficulty", difficulty),
			zap.Duration("connected_duration", duration),
			zap.Error(scanErr))
	}
}

func (s *Server) handleMessage(client *Client, data []byte) {
	var req Request
	if err := json.Unmarshal(data, &req); err != nil {
		s.logger.Warn("Failed to parse stratum message",
			zap.String("ip", client.IP),
			zap.Error(err),
			zap.ByteString("data", data))
		return
	}

	// Log all messages from NiceHash clients for debugging
	client.mu.RLock()
	rental := client.RentalService
	minerID := client.MinerID
	client.mu.RUnlock()
	if rental == RentalNiceHash && req.Method != "" {
		s.logger.Info("NiceHash message received",
			zap.String("miner", minerID),
			zap.String("method", req.Method))
	}

	switch req.Method {
	case MethodSubscribe:
		resp := s.handleSubscribe(client, &req)
		s.sendResponse(client, resp)
		// Difficulty will be sent after authorize
	case MethodAuthorize:
		resp := s.handleAuthorize(client, &req)
		// Send auth response FIRST
		s.sendResponse(client, resp)
		// Then send difficulty and job
		if resp.Result == true {
			// Send difficulty once, then job
			s.sendDifficulty(client, client.Difficulty)
			if job := s.currentJob.Load(); job != nil {
				// Send initial job with clean=true so miner starts fresh
				initialJob := *job.(*Job)
				initialJob.CleanJobs = true
				s.sendJob(client, &initialJob)
				s.logger.Debug("Sent initial job after auth",
					zap.String("miner", client.MinerID),
					zap.String("job_id", initialJob.ID))
			} else {
				s.logger.Warn("No current job to send after auth",
					zap.String("miner", client.MinerID))
			}
		}
	case MethodConfigure:
		s.logger.Info("mining.configure received",
			zap.String("ip", client.IP),
			zap.String("user_agent", client.UserAgent))
		resp := s.handleConfigure(client, &req)
		s.sendResponse(client, resp)
	case MethodSubmit:
		resp := s.handleSubmit(client, &req)
		s.sendResponse(client, resp)
	case "mining.suggest_difficulty":
		// Handle miner's difficulty suggestion
		var params []float64
		if err := json.Unmarshal(req.Params, &params); err == nil && len(params) > 0 {
			suggestedDiff := params[0]
			// Clamp to our min/max range (use appropriate min for client type)
			minDiff := s.getMinDiffForClient(client)
			if suggestedDiff < minDiff {
				suggestedDiff = minDiff
			}
			if suggestedDiff > s.config.MaxDiff {
				suggestedDiff = s.config.MaxDiff
			}
			client.mu.Lock()
			client.Difficulty = suggestedDiff
			client.mu.Unlock()
			s.logger.Info("Miner suggested difficulty accepted",
				zap.String("ip", client.IP),
				zap.Float64("difficulty", suggestedDiff))
			s.sendDifficulty(client, suggestedDiff)
		}
		s.sendResponse(client, &Response{ID: req.ID, Result: true})
	case "mining.extranonce.subscribe":
		// Support extranonce subscription for rental services (NiceHash, MRR)
		client.mu.Lock()
		client.SupportsExtranonce = true
		rental := client.RentalService
		client.mu.Unlock()

		if rental != RentalNone {
			s.logger.Info("Rental service subscribed to extranonce updates",
				zap.String("ip", client.IP),
				zap.String("rental_service", rental.String()))
		}
		s.sendResponse(client, &Response{ID: req.ID, Result: true})
	default:
		s.logger.Info("Ignoring unsupported stratum method",
			zap.String("method", req.Method),
			zap.String("ip", client.IP))
		s.sendResponse(client, &Response{ID: req.ID, Result: true})
	}
}

func (s *Server) handleSubscribe(client *Client, req *Request) *Response {
	client.mu.Lock()
	defer client.mu.Unlock()

	// Parse subscription params to detect user agent
	// Format: ["user-agent/version", "session-id"] or ["user-agent/version"]
	var params []interface{}
	if err := json.Unmarshal(req.Params, &params); err == nil && len(params) > 0 {
		if ua, ok := params[0].(string); ok {
			client.UserAgent = ua
			client.RentalService = detectRentalService(ua)
			// Bump difficulty to rental minimum if rental service detected
			if client.RentalService != RentalNone && client.Difficulty < s.config.RentalMinDiff {
				client.Difficulty = s.config.RentalMinDiff
			}
		}
	}

	s.extraNonceMu.Lock()
	s.extraNonce++
	// Use configured extranonce1 size (default 6 bytes = 12 hex chars)
	en1Size := s.config.ExtraNonce1Size
	if en1Size == 0 {
		en1Size = 6
	}
	en1Format := fmt.Sprintf("%%0%dx", en1Size*2)
	client.ExtraNonce1 = fmt.Sprintf(en1Format, s.extraNonce)
	s.extraNonceMu.Unlock()

	// Use configured extranonce2 size (default 4, Braiins needs 8)
	client.ExtraNonce2Size = s.config.ExtraNonce2Size
	if client.ExtraNonce2Size == 0 {
		client.ExtraNonce2Size = 4
	}
	client.SubscriptionID = fmt.Sprintf("forge_%s", client.ID)
	client.Subscribed = true

	result := []interface{}{
		[][]string{
			{"mining.set_difficulty", client.SubscriptionID},
			{"mining.notify", client.SubscriptionID},
		},
		client.ExtraNonce1,
		client.ExtraNonce2Size,
	}

	// Log with rental service detection
	if client.RentalService != RentalNone {
		s.logger.Info("Rental service client subscribed",
			zap.String("ip", client.IP),
			zap.String("extranonce", client.ExtraNonce1),
			zap.String("rental_service", client.RentalService.String()),
			zap.String("user_agent", client.UserAgent))
	} else {
		s.logger.Info("Client subscribed",
			zap.String("ip", client.IP),
			zap.String("extranonce", client.ExtraNonce1),
			zap.String("user_agent", client.UserAgent))
	}

	return &Response{ID: req.ID, Result: result}
}

// detectRentalService identifies rental services from user agent string
func detectRentalService(userAgent string) RentalService {
	ua := strings.ToLower(userAgent)

	// NiceHash detection patterns
	// Examples: "NiceHash/1.0.0", "nhmp/1.0.0", "excavator/1.6.3"
	nicehashPatterns := []string{
		"nicehash",
		"nhmp",
		"excavator",
		"nh/",
	}
	for _, pattern := range nicehashPatterns {
		if strings.Contains(ua, pattern) {
			return RentalNiceHash
		}
	}

	// Mining Rig Rentals detection patterns
	// Examples: "MiningRigRentals/1.0", "mrr/", "miningrigrentals"
	mrrPatterns := []string{
		"miningrigrentals",
		"mrr/",
		"mrr-",
		"rigrentals",
	}
	for _, pattern := range mrrPatterns {
		if strings.Contains(ua, pattern) {
			return RentalMRR
		}
	}

	// Generic rental/proxy indicators
	rentalPatterns := []string{
		"rental",
		"proxy",
		"stratum-proxy",
	}
	for _, pattern := range rentalPatterns {
		if strings.Contains(ua, pattern) {
			return RentalOther
		}
	}

	return RentalNone
}

func (s *Server) handleAuthorize(client *Client, req *Request) *Response {
	var params []string
	// Debug log
	if err := json.Unmarshal(req.Params, &params); err != nil || len(params) < 1 {
		return &Response{ID: req.ID, Result: false, Error: ErrUnauthorized}
	}

	username := params[0]
	minerID, workerName := parseUsername(username)

	// Allow Braiins probe connections (used to verify pool connectivity)
	// These use usernames like "braiinstest" which aren't valid addresses
	if strings.HasPrefix(strings.ToLower(username), "braiins") && minerID == "" {
		s.logger.Info("Braiins probe connection accepted",
			zap.String("username", username),
			zap.String("ip", client.IP))
		// Set a dummy address for probe - won't receive payouts
		minerID = "probe"
		workerName = username
	}

	// Reject invalid addresses - they cannot receive payouts
	if minerID == "" {
		s.logger.Warn("Rejected connection with invalid address",
			zap.String("username", username),
			zap.String("ip", client.IP))
		return &Response{ID: req.ID, Result: false, Error: ErrUnauthorized}
	}

	// Detect rental service from worker name patterns
	rentalFromWorker := detectRentalFromWorker(workerName)

	var soloMode bool
	var manualDiff float64

	if s.minerSettings != nil {
		if settings, err := s.minerSettings.GetMinerSettings(minerID); err == nil && settings != nil {
			soloMode = settings.SoloMining
			manualDiff = settings.ManualDiff
		}
	}

	client.mu.Lock()
	client.Authorized = true
	client.MinerID = minerID
	client.WorkerName = workerName
	client.SoloMining = soloMode
	client.ManualDiff = manualDiff
	client.LastSettingsRefresh = time.Now()

	// Update rental detection if found from worker name (user agent takes priority)
	if client.RentalService == RentalNone && rentalFromWorker != RentalNone {
		client.RentalService = rentalFromWorker
	}

	rental := client.RentalService

	if manualDiff > 0 {
		// For rental services, ensure manual diff meets their minimum requirement
		if rental != RentalNone && manualDiff < s.config.RentalMinDiff {
			client.Difficulty = s.config.RentalMinDiff
		} else {
			client.Difficulty = manualDiff
		}
	}
	client.mu.Unlock()

	if soloMode {
		atomic.AddInt64(&s.stats.SoloMiners, 1)
	} else {
		atomic.AddInt64(&s.stats.PPLNSMiners, 1)
	}

	modeStr := "PPLNS"
	if soloMode {
		modeStr = "SOLO"
	}

	if rental != RentalNone {
		s.logger.Info("Rental miner authorized",
			zap.String("miner", minerID),
			zap.String("worker", workerName),
			zap.String("mode", modeStr),
			zap.String("rental_service", rental.String()),
			zap.Float64("difficulty", client.Difficulty))
	} else {
		s.logger.Info("Miner authorized",
			zap.String("miner", minerID),
			zap.String("worker", workerName),
			zap.String("mode", modeStr),
			zap.Float64("difficulty", client.Difficulty))
	}

	return &Response{ID: req.ID, Result: true}
}

// detectRentalFromWorker detects rental services from worker name patterns
func detectRentalFromWorker(worker string) RentalService {
	w := strings.ToLower(worker)

	// NiceHash often uses worker names like "nh_xxxx" or contains "nicehash"
	if strings.HasPrefix(w, "nh_") || strings.HasPrefix(w, "nh-") ||
		strings.Contains(w, "nicehash") {
		return RentalNiceHash
	}

	// MRR often uses worker names like "mrr_xxxx" or "rig_xxxx"
	if strings.HasPrefix(w, "mrr_") || strings.HasPrefix(w, "mrr-") ||
		strings.HasPrefix(w, "mrr.") || strings.Contains(w, "miningrigrentals") {
		return RentalMRR
	}

	// Generic rental patterns
	if strings.Contains(w, "rental") || strings.Contains(w, "rent_") {
		return RentalOther
	}

	return RentalNone
}

func (s *Server) handleSubmit(client *Client, req *Request) *Response {
	client.mu.RLock()
	if !client.Authorized {
		client.mu.RUnlock()
		s.logger.Warn("Submit from unauthorized client", zap.String("ip", client.IP))
		return &Response{ID: req.ID, Result: false, Error: ErrUnauthorized}
	}
	minerID := client.MinerID
	workerName := client.WorkerName
	difficulty := client.Difficulty
	soloMining := client.SoloMining
	manualDiff := client.ManualDiff
	extranonce1 := client.ExtraNonce1
	extranonce2Size := client.ExtraNonce2Size
	lastSettingsRefresh := client.LastSettingsRefresh
	client.mu.RUnlock()

	// Refresh settings every 15 seconds to allow on-the-fly mode changes
	if s.minerSettings != nil && time.Since(lastSettingsRefresh) > 15*time.Second {
		if settings, err := s.minerSettings.GetMinerSettings(minerID); err == nil && settings != nil {
			client.mu.Lock()
			if client.SoloMining != settings.SoloMining {
				s.logger.Info("Miner mode changed on-the-fly",
					zap.String("miner", minerID),
					zap.Bool("old_solo", client.SoloMining),
					zap.Bool("new_solo", settings.SoloMining))
			}
			client.SoloMining = settings.SoloMining
			client.ManualDiff = settings.ManualDiff
			client.LastSettingsRefresh = time.Now()
			soloMining = settings.SoloMining
			manualDiff = settings.ManualDiff
			client.mu.Unlock()
		}
	}

	// Parse params as []interface{} to handle miners that send mixed types
	var rawParams []interface{}
	if err := json.Unmarshal(req.Params, &rawParams); err != nil {
		s.logger.Warn("Failed to parse submit params",
			zap.String("miner", minerID),
			zap.Error(err),
			zap.ByteString("params", req.Params))
		atomic.AddInt64(&s.stats.InvalidShares, 1)
		atomic.AddInt64(&client.InvalidShares, 1)
		return &Response{ID: req.ID, Result: false, Error: ErrLowDifficulty}
	}

	if len(rawParams) < 5 {
		s.logger.Warn("Insufficient submit params",
			zap.String("miner", minerID),
			zap.Int("count", len(rawParams)))
		atomic.AddInt64(&s.stats.InvalidShares, 1)
		atomic.AddInt64(&client.InvalidShares, 1)
		return &Response{ID: req.ID, Result: false, Error: ErrLowDifficulty}
	}

	// Convert all params to strings (handles both string and numeric values)
	// SECURITY: Validate numeric ranges to prevent integer overflow attacks
	params := make([]string, len(rawParams))
	for i, p := range rawParams {
		switch v := p.(type) {
		case string:
			params[i] = v
		case float64:
			// SECURITY: Validate range before casting to prevent overflow
			if v < 0 || v > float64(^uint32(0)) {
				s.logger.Warn("Invalid numeric parameter - out of range",
					zap.Int("param_index", i),
					zap.Float64("value", v))
				atomic.AddInt64(&s.stats.InvalidShares, 1)
				atomic.AddInt64(&client.InvalidShares, 1)
				return &Response{ID: req.ID, Result: false, Error: ErrLowDifficulty}
			}
			params[i] = fmt.Sprintf("%08x", uint32(v))
		case json.Number:
			if n, err := v.Int64(); err == nil {
				// SECURITY: Validate range before casting
				if n < 0 || n > int64(^uint32(0)) {
					s.logger.Warn("Invalid numeric parameter - out of range",
						zap.Int("param_index", i),
						zap.Int64("value", n))
					atomic.AddInt64(&s.stats.InvalidShares, 1)
					atomic.AddInt64(&client.InvalidShares, 1)
					return &Response{ID: req.ID, Result: false, Error: ErrLowDifficulty}
				}
				params[i] = fmt.Sprintf("%08x", uint32(n))
			} else {
				params[i] = string(v)
			}
		default:
			params[i] = fmt.Sprintf("%v", p)
		}
	}

	jobID := params[1]
	extranonce2 := normalizeHex(params[2], extranonce2Size*2) // Use client's configured size
	ntime := normalizeHex(params[3], 8)
	nonce := normalizeHex(params[4], 8)
	versionBits := ""
	if len(params) > 5 {
		versionBits = params[5]
	}

	s.logger.Info("Share submitted",
		zap.String("miner", minerID),
		zap.String("worker", workerName),
		zap.String("job", jobID),
		zap.String("extranonce2", extranonce2),
		zap.String("ntime", ntime),
		zap.String("nonce", nonce))

	// Check for duplicate share FIRST
	if s.isDuplicateShare(jobID, extranonce1, extranonce2, ntime, nonce) {
		s.logger.Warn("Duplicate share rejected",
			zap.String("miner", minerID),
			zap.String("job", jobID))
		atomic.AddInt64(&s.stats.InvalidShares, 1)
		atomic.AddInt64(&client.InvalidShares, 1)
		return &Response{ID: req.ID, Result: false, Error: ErrDuplicateShare}
	}

	// Get the job from history
	jobInterface, exists := s.jobHistory.Load(jobID)
	if !exists {
		s.logger.Warn("Job not found",
			zap.String("miner", minerID),
			zap.String("job", jobID))
		atomic.AddInt64(&s.stats.InvalidShares, 1)
		atomic.AddInt64(&client.InvalidShares, 1)
		return &Response{ID: req.ID, Result: false, Error: ErrJobNotFound}
	}
	job := jobInterface.(*Job)

	// Validate the share - verify proof of work
	// Accept any share that meets min_diff - don't waste miner's work
	// Target difficulty is for rate limiting/vardiff, not rejection
	isValid, actualDiff, blockHash, err := s.validateShare(job, extranonce1, extranonce2, ntime, nonce, versionBits, s.config.MinDiff)
	if err != nil {
		s.logger.Warn("Share validation error",
			zap.String("miner", minerID),
			zap.Error(err))
		atomic.AddInt64(&s.stats.InvalidShares, 1)
		atomic.AddInt64(&client.InvalidShares, 1)
		return &Response{ID: req.ID, Result: false, Error: ErrLowDifficulty}
	}

	if !isValid {
		s.logger.Warn("Share below minimum difficulty",
			zap.String("miner", minerID),
			zap.Float64("required", s.config.MinDiff),
			zap.Float64("actual", actualDiff))
		atomic.AddInt64(&s.stats.InvalidShares, 1)
		atomic.AddInt64(&client.InvalidShares, 1)

		// Track rejection for vardiff adjustment
		client.mu.Lock()
		client.RecentSubmissions = append(client.RecentSubmissions, false)
		if len(client.RecentSubmissions) > RecentSubmissionsWindow {
			client.RecentSubmissions = client.RecentSubmissions[1:]
		}
		// Check if rejection rate is too high and reduce difficulty
		if len(client.RecentSubmissions) >= 20 && manualDiff == 0 && s.config.VardiffEnabled {
			rejections := 0
			for _, accepted := range client.RecentSubmissions {
				if !accepted {
					rejections++
				}
			}
			rejectionRate := float64(rejections) / float64(len(client.RecentSubmissions))
			// Use appropriate min_diff based on client type (rental vs regular)
			minDiff := s.config.MinDiff
			if client.RentalService != RentalNone {
				minDiff = s.config.RentalMinDiff
			}
			if rejectionRate > MaxRejectionRate && client.Difficulty > minDiff {
				// Use more aggressive reduction (2x) to settle faster
				oldDiff := client.Difficulty
				newDiff := client.Difficulty / 2.0
				if newDiff < minDiff {
					newDiff = minDiff
				}
				client.PreviousDifficulty = oldDiff
				client.DifficultyChangedAt = time.Now()
				client.DifficultyReducedAt = time.Now()
				client.DifficultyReducedFrom = oldDiff // Remember the ceiling that caused rejections
				client.Difficulty = newDiff
				client.RecentSubmissions = client.RecentSubmissions[:0] // Reset after adjustment
				client.mu.Unlock()

				s.logger.Info("High rejection rate, reducing difficulty",
					zap.String("miner", minerID),
					zap.Float64("rejection_rate", rejectionRate),
					zap.Float64("old_diff", oldDiff),
					zap.Float64("new_diff", newDiff))

				s.sendDifficulty(client, newDiff)
				return &Response{ID: req.ID, Result: false, Error: ErrLowDifficulty}
			}
		}
		client.mu.Unlock()

		return &Response{ID: req.ID, Result: false, Error: ErrLowDifficulty}
	}

	now := time.Now()

	client.mu.Lock()
	client.ShareTimes = append(client.ShareTimes, now)
	if len(client.ShareTimes) > 100 {
		client.ShareTimes = client.ShareTimes[1:]
	}
	// Track accepted submission for rejection rate calculation
	client.RecentSubmissions = append(client.RecentSubmissions, true)
	if len(client.RecentSubmissions) > RecentSubmissionsWindow {
		client.RecentSubmissions = client.RecentSubmissions[1:]
	}
	shareCount := len(client.ShareTimes)

	// Save the difficulty at which this share was actually submitted
	// (before any adjustments that apply to future shares)
	shareDifficulty := difficulty

	client.mu.Unlock()

	// Vardiff adjustment - respects retarget_time interval
	if manualDiff == 0 && s.config.VardiffEnabled && shareCount >= VardiffMinShares {
		s.adjustVardiff(client)
	}

	share := &Share{
		JobID:       jobID,
		MinerID:     minerID,
		WorkerName:  workerName,
		Difficulty:  shareDifficulty, // Use target difficulty for hashrate calculation
		ActualDiff:  actualDiff,      // Use actual share difficulty for block candidate detection
		IP:          client.IP,
		ExtraNonce2: extranonce2,
		ExtraNonce1: extranonce1,
		NTime:       ntime,
		Nonce:       nonce,
		VersionBits: versionBits,
		IsValid:     true,
		IsSolo:      soloMining,
		SubmittedAt: now,
		BlockHash:   hex.EncodeToString(blockHash),
	}

	atomic.AddInt64(&s.stats.ValidShares, 1)
	atomic.AddInt64(&client.ValidShares, 1)

	if s.shareProcessor != nil {
		go s.shareProcessor.ProcessShare(context.Background(), share)
	}

	s.logger.Info("Share accepted",
		zap.String("miner", minerID),
		zap.String("worker", workerName),
		zap.Float64("diff", actualDiff),
		zap.Float64("target_diff", difficulty),
		zap.Bool("solo", soloMining),
		zap.String("hash", hex.EncodeToString(blockHash)[:16]+"..."))

	return &Response{ID: req.ID, Result: true}
}

func (s *Server) adjustVardiff(client *Client) {
	client.mu.Lock()

	// Only adjust every RetargetTime seconds (default 60)
	retargetTime := s.config.RetargetTime
	if retargetTime == 0 {
		retargetTime = 60
	}
	if time.Since(client.DifficultyChangedAt) < time.Duration(retargetTime)*time.Second {
		client.mu.Unlock()
		return
	}

	// Use larger sample window for more stable measurements
	sampleSize := VardiffMinShares
	if len(client.ShareTimes) < sampleSize {
		client.mu.Unlock()
		return
	}

	recent := client.ShareTimes[len(client.ShareTimes)-sampleSize:]
	totalTime := recent[sampleSize-1].Sub(recent[0]).Seconds()
	if totalTime <= 0 {
		client.mu.Unlock()
		return
	}
	avgTime := totalTime / float64(sampleSize-1)

	targetTime := float64(s.config.TargetShareTime)
	ratio := targetTime / avgTime

	// Check rejection rate before adjusting
	rejections := 0
	for _, accepted := range client.RecentSubmissions {
		if !accepted {
			rejections++
		}
	}
	rejectionRate := 0.0
	if len(client.RecentSubmissions) > 0 {
		rejectionRate = float64(rejections) / float64(len(client.RecentSubmissions))
	}

	// If rejection rate is high, don't increase difficulty even if timing suggests we should
	if rejectionRate > MaxRejectionRate && ratio > 1.0 {
		client.mu.Unlock()
		return
	}

	// Only adjust if outside variance window (miningcore style)
	// This prevents constant small adjustments
	varianceLow := 1.0 - VardiffVariancePercent
	varianceHigh := 1.0 + VardiffVariancePercent
	if ratio >= varianceLow && ratio <= varianceHigh {
		client.mu.Unlock()
		return
	}

	// Clamp ratio to prevent extreme difficulty changes (max 50% per adjustment)
	if ratio > MaxDifficultyMultiplier {
		ratio = MaxDifficultyMultiplier
	} else if ratio < 1.0/MaxDifficultyMultiplier {
		ratio = 1.0 / MaxDifficultyMultiplier
	}

	// Calculate new difficulty
	newDiff := client.Difficulty * ratio

	// For rental services, apply gentler MaxDelta (max 25% change)
	maxDelta := client.Difficulty * 0.5 // 50% max change for regular miners
	if client.RentalService != RentalNone {
		maxDelta = client.Difficulty * 0.25 // 25% max change for NiceHash/MRR
	}

	diffDelta := newDiff - client.Difficulty
	if diffDelta > maxDelta {
		newDiff = client.Difficulty + maxDelta
	} else if diffDelta < -maxDelta {
		newDiff = client.Difficulty - maxDelta
	}

	// Use appropriate min_diff based on client type (rental vs regular)
	minDiff := s.config.MinDiff
	if client.RentalService != RentalNone {
		minDiff = s.config.RentalMinDiff
	}
	if newDiff < minDiff {
		newDiff = minDiff
	}
	// Use rental-specific max diff for NiceHash/MRR to prevent over-ramping
	maxDiff := s.config.MaxDiff
	if client.RentalService != RentalNone && s.config.RentalMaxDiff > 0 {
		maxDiff = s.config.RentalMaxDiff
	}
	if newDiff > maxDiff {
		newDiff = maxDiff
	}

	// If we recently reduced difficulty due to high rejection rate,
	// don't increase above 80% of the ceiling that caused the rejection
	if client.DifficultyReducedFrom > 0 && time.Since(client.DifficultyReducedAt) < DifficultyReductionCooldown {
		ceiling := client.DifficultyReducedFrom * 0.8
		if newDiff > ceiling {
			newDiff = ceiling
			s.logger.Debug("Vardiff capped at ceiling",
				zap.String("miner", client.MinerID),
				zap.Float64("ceiling", ceiling),
				zap.Float64("original", client.Difficulty*ratio))
		}
	}

	if newDiff != client.Difficulty {
		oldDiff := client.Difficulty
		client.PreviousDifficulty = oldDiff
		client.DifficultyChangedAt = time.Now()
		client.Difficulty = newDiff
		minerID := client.MinerID
		client.mu.Unlock()

		// Send difficulty synchronously to ensure miner receives it
		s.sendDifficulty(client, newDiff)

		s.logger.Info("Vardiff adjusted",
			zap.String("miner", minerID),
			zap.Float64("avg_time", avgTime),
			zap.Float64("old_diff", oldDiff),
			zap.Float64("new_diff", newDiff))
		return
	}
	client.mu.Unlock()
}

func (s *Server) sendResponse(client *Client, resp *Response) {
	data, _ := json.Marshal(resp)
	data = append(data, '\n')
	client.Conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := client.Conn.Write(data); err != nil {
		client.Conn.Close() // Force disconnect on write error
	}
}

func (s *Server) sendNotification(client *Client, notif *Notification) {
	data, _ := json.Marshal(notif)
	data = append(data, '\n')
	client.Conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := client.Conn.Write(data); err != nil {
		// Log write error for NiceHash clients
		client.mu.RLock()
		rental := client.RentalService
		minerID := client.MinerID
		client.mu.RUnlock()
		if rental == RentalNiceHash {
			s.logger.Warn("Write error to NiceHash client",
				zap.String("miner", minerID),
				zap.String("method", notif.Method),
				zap.Error(err))
		}
		client.Conn.Close() // Force disconnect on write error
	}
}

func (s *Server) sendDifficulty(client *Client, diff float64) {
	// Prevent sending duplicate difficulty notifications within 500ms
	// This avoids race conditions between authorize and job broadcast
	client.mu.Lock()
	if time.Since(client.LastDifficultySentAt) < 500*time.Millisecond {
		client.mu.Unlock()
		return
	}
	client.LastDifficultySentAt = time.Now()
	client.mu.Unlock()

	notif := &Notification{
		Method: MethodSetDifficulty,
		Params: []interface{}{diff},
	}
	s.sendNotification(client, notif)
}

func (s *Server) sendJob(client *Client, job *Job) {
	notif := &Notification{
		Method: MethodNotify,
		Params: []interface{}{
			job.ID,
			job.PrevBlockHash,
			job.CoinBase1,
			job.CoinBase2,
			job.MerkleBranches,
			job.Version,
			job.NBits,
			job.NTime,
			job.CleanJobs,
		},
	}

	// Log job delivery for NiceHash clients for debugging
	client.mu.RLock()
	rental := client.RentalService
	minerID := client.MinerID
	client.mu.RUnlock()

	if rental == RentalNiceHash {
		s.logger.Info("Sending job to NiceHash client",
			zap.String("miner", minerID),
			zap.String("job_id", job.ID),
			zap.String("prevhash", job.PrevBlockHash[:16]+"..."),
			zap.String("version", job.Version),
			zap.String("nbits", job.NBits),
			zap.String("ntime", job.NTime),
			zap.Bool("clean", job.CleanJobs))
	}

	s.sendNotification(client, notif)
}

func (s *Server) BroadcastJob(job *Job) {
	s.currentJob.Store(job)
	s.jobHistory.Store(job.ID, job)

	// Clear old shares when broadcasting clean jobs (new block height)
	if job.CleanJobs {
		s.clearSharesForJob()
	}

	s.clients.Range(func(key, value interface{}) bool {
		client := value.(*Client)
		if client.Authorized {
			// For clean jobs (new block), resend difficulty to ensure miners have it
			// Some miners (like Whatsminer) may miss difficulty notifications
			if job.CleanJobs {
				client.mu.RLock()
				diff := client.Difficulty
				client.mu.RUnlock()
				s.sendDifficulty(client, diff)
			}
			s.sendJob(client, job)
		}
		return true
	})
}

func (s *Server) GetStats() *ServerStats {
	return &ServerStats{
		ActiveConnections: atomic.LoadInt64(&s.stats.ActiveConnections),
		ValidShares:       atomic.LoadInt64(&s.stats.ValidShares),
		InvalidShares:     atomic.LoadInt64(&s.stats.InvalidShares),
		BlocksFound:       atomic.LoadInt64(&s.stats.BlocksFound),
		SoloMiners:        atomic.LoadInt64(&s.stats.SoloMiners),
		PPLNSMiners:       atomic.LoadInt64(&s.stats.PPLNSMiners),
	}
}

// RentalStats contains statistics about rental service connections
type RentalStats struct {
	NiceHashMiners int64
	MRRMiners      int64
	OtherRentals   int64
	TotalRentals   int64
}

// GetRentalStats returns statistics about connected rental miners
func (s *Server) GetRentalStats() *RentalStats {
	stats := &RentalStats{}

	s.clients.Range(func(key, value interface{}) bool {
		client := value.(*Client)
		client.mu.RLock()
		rental := client.RentalService
		authorized := client.Authorized
		client.mu.RUnlock()

		if !authorized {
			return true
		}

		switch rental {
		case RentalNiceHash:
			stats.NiceHashMiners++
			stats.TotalRentals++
		case RentalMRR:
			stats.MRRMiners++
			stats.TotalRentals++
		case RentalOther:
			stats.OtherRentals++
			stats.TotalRentals++
		}
		return true
	})

	return stats
}

// sendExtranonce sends an extranonce update to a client that supports it
// This is used when a client's extranonce needs to change (rare, but supported)
func (s *Server) sendExtranonce(client *Client, extranonce1 string, extranonce2Size int) {
	client.mu.RLock()
	supportsExtranonce := client.SupportsExtranonce
	client.mu.RUnlock()

	if !supportsExtranonce {
		return
	}

	notif := &Notification{
		Method: "mining.set_extranonce",
		Params: []interface{}{extranonce1, extranonce2Size},
	}
	s.sendNotification(client, notif)

	s.logger.Info("Sent extranonce update",
		zap.String("ip", client.IP),
		zap.String("extranonce1", extranonce1))
}

// IsRentalClient checks if a client is from a rental service
func (c *Client) IsRentalClient() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.RentalService != RentalNone
}

func parseUsername(username string) (minerID, workerName string) {
	parts := strings.SplitN(username, ".", 2)
	minerID = parts[0]
	if len(parts) > 1 {
		workerName = parts[1]
	} else {
		workerName = "default"
	}

	// Normalize address: ensure bitcoincashii: prefix (lowercase)
	minerID = normalizeMinerAddress(minerID)

	// CashAddr is exactly 42 chars after prefix. If extra chars remain
	// (e.g. NiceHash appends worker suffix without dot separator),
	// split them off as worker name.
	if strings.HasPrefix(minerID, "bitcoincashii:") {
		hash := minerID[len("bitcoincashii:"):]
		if len(hash) > 42 {
			extra := hash[42:]
			minerID = "bitcoincashii:" + hash[:42]
			if workerName == "default" {
				workerName = extra
			}
		}
	}

	return
}

// normalizeMinerAddress ensures the address has the correct bitcoincashii: prefix
// Returns empty string for invalid/rejected address formats
func normalizeMinerAddress(addr string) string {
	// Convert to lowercase for comparison
	lowerAddr := strings.ToLower(addr)

	// REJECT bitcoincash2: prefix - invalid format
	if strings.HasPrefix(lowerAddr, "bitcoincash2:") {
		return "" // Signal rejection
	}

	// If already has correct prefix, validate it has an actual hash after the prefix
	if strings.HasPrefix(lowerAddr, "bitcoincashii:") {
		hash := lowerAddr[len("bitcoincashii:"):]
		if len(hash) < 42 || (hash[0] != 'q' && hash[0] != 'p') {
			return "" // Reject: prefix without valid hash
		}
		return lowerAddr
	}

	// Handle truncated prefix: bitcoinii: -> bitcoincashii: (WhatsMiner firmware bug)
	if strings.HasPrefix(lowerAddr, "bitcoinii:") {
		hash := lowerAddr[len("bitcoinii:"):]
		if len(hash) >= 42 && (hash[0] == 'q' || hash[0] == 'p') {
			return "bitcoincashii:" + hash
		}
		return "" // Reject: invalid hash after prefix
	}

	// Reject bare prefix variants with no hash (firmware truncation)
	if lowerAddr == "bitcoincashii" || lowerAddr == "bitcoincash" || lowerAddr == "bitcoinii" {
		return "" // Signal rejection
	}

	// If it's just the hash part (starts with 'q' for mainnet), add prefix
	if len(addr) >= 42 && (strings.HasPrefix(lowerAddr, "q") || strings.HasPrefix(lowerAddr, "p")) {
		return "bitcoincashii:" + lowerAddr
	}

	// Reject anything else that isn't a valid address
	return ""
}

// normalizeHex pads a hex string to the required length with leading zeros
func normalizeHex(s string, length int) string {
	// Remove any "0x" prefix
	s = strings.TrimPrefix(s, "0x")
	s = strings.TrimPrefix(s, "0X")

	// If already correct length, return as-is
	if len(s) == length {
		return s
	}

	// If shorter, pad with leading zeros
	if len(s) < length {
		return strings.Repeat("0", length-len(s)) + s
	}

	// If longer, return as-is (validation will catch it)
	return s
}

func (s *Server) handleConfigure(client *Client, req *Request) *Response {
	// mining.configure for version rolling (AsicBoost) and other extensions
	// Request format: [["extension1", "extension2", ...], {"param1": value1, ...}]

	var params []json.RawMessage
	var supportsMultiVersion bool
	if err := json.Unmarshal(req.Params, &params); err == nil && len(params) >= 1 {
		var extensions []string
		if err := json.Unmarshal(params[0], &extensions); err == nil {
			// Check requested extensions
			for _, ext := range extensions {
				if ext == "version-rolling" {
					client.mu.Lock()
					client.SupportsVersionRolling = true
					client.VersionRollingMask = "1fffe000"
					client.mu.Unlock()
				}
				if ext == "multi_version" {
					supportsMultiVersion = true
				}
			}
		}

		// Parse extension parameters if provided
		if len(params) >= 2 {
			var extParams map[string]interface{}
			if err := json.Unmarshal(params[1], &extParams); err == nil {
				// Check for version-rolling.mask request
				if mask, ok := extParams["version-rolling.mask"].(string); ok {
					// Intersect with our supported mask
					client.mu.Lock()
					client.VersionRollingMask = intersectMasks("1fffe000", mask)
					client.mu.Unlock()
				}
			}
		}
	}

	client.mu.RLock()
	rental := client.RentalService
	mask := client.VersionRollingMask
	if mask == "" {
		mask = "1fffe000"
	}
	client.mu.RUnlock()

	if rental != RentalNone {
		s.logger.Info("Rental service configured",
			zap.String("ip", client.IP),
			zap.String("rental_service", rental.String()),
			zap.String("version_rolling_mask", mask))
	}

	// Return supported extensions
	// Use min-bit-count of 0 - we don't require any minimum bits
	result := map[string]interface{}{
		"version-rolling":               true,
		"version-rolling.mask":          mask,
		"version-rolling.min-bit-count": 0,
	}
	// Add multi_version support if requested (NiceHash compatibility)
	if supportsMultiVersion {
		result["multi_version"] = true
	}
	return &Response{ID: req.ID, Result: result}
}

// intersectMasks returns the intersection of two hex masks
func intersectMasks(mask1, mask2 string) string {
	var m1, m2 uint32
	fmt.Sscanf(mask1, "%x", &m1)
	fmt.Sscanf(mask2, "%x", &m2)
	return fmt.Sprintf("%08x", m1&m2)
}
