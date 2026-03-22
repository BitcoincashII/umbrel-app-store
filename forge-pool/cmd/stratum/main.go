package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/bch2/forge-pool/internal/mining"
	"github.com/bch2/forge-pool/internal/stats"
	"github.com/bch2/forge-pool/internal/stratum"
	"github.com/bch2/forge-pool/internal/stratumv2"
	"github.com/spf13/viper"
	"go.uber.org/zap"
)

var (
	logger            *zap.Logger
	jobManager        *mining.JobManager
	currentJob        *mining.Job
	currentJobMu      sync.RWMutex // Protects currentJob access
	jobHistory        = make(map[string]*mining.Job) // Store jobs by ID for block submission
	jobHistoryOrder   []string                       // Track insertion order for FIFO cleanup
	jobHistoryMu      sync.RWMutex
	rpcURL            string
	rpcUser           string
	rpcPass           string
	networkDifficulty float64 = 1.0
	networkDiffMu     sync.RWMutex // Protects networkDifficulty access
	poolAddress       string
	poolFee           float64 = 1.0  // PPLNS fee percentage
	soloFee           float64 = 0.5  // Solo fee percentage
	blockReward       float64 = 50.0
	minPayout         float64 = 5.0
	pplnsWindow       int     = 100000 // PPLNS window size (shares)
	stratumServer        *stratum.Server // Global reference for API handlers
	stratumBraiinsServer *stratum.Server // Second stratum for Braiins (8-byte extranonce2)
	stratumV2Server      *stratumv2.Server // Stratum V2 server (optional)
	v2JobIDCounter       uint32 // V2 job ID counter

	// Shutdown channel for graceful termination
	shutdownCh        = make(chan struct{})

	// Security: Payout mutex to prevent concurrent payout processing per miner
	payoutMu          sync.Mutex
	payoutInProgress  = make(map[string]time.Time) // Track active payout requests per miner
	payoutMuMap       sync.RWMutex

	// Security: Required internal API token (must be set in environment)
	internalAPIToken  string

	// Global HTTP client for RPC calls (reuses connections)
	httpClient = &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     90 * time.Second,
		},
	}
)

// Thread-safe access to currentJob
func getCurrentJob() *mining.Job {
	currentJobMu.RLock()
	defer currentJobMu.RUnlock()
	return currentJob
}

func setCurrentJob(job *mining.Job) {
	currentJobMu.Lock()
	defer currentJobMu.Unlock()
	currentJob = job
}

// Thread-safe access to networkDifficulty
func getNetworkDifficulty() float64 {
	networkDiffMu.RLock()
	defer networkDiffMu.RUnlock()
	return networkDifficulty
}

func setNetworkDifficulty(diff float64) {
	networkDiffMu.Lock()
	defer networkDiffMu.Unlock()
	networkDifficulty = diff
}

func startPayoutProcessor() {
	ticker := time.NewTicker(60 * time.Second) // Check every minute
	defer ticker.Stop()

	// Use global rpcURL configured from config file
	nodeURL := rpcURL

	// Track failed payouts for retry (address -> retry count)
	failedPayouts := make(map[string]int)
	const maxRetries = 3

	// Dust logging interval (every 10 cycles = ~10 minutes)
	dustLogCounter := 0

	for {
		select {
		case <-shutdownCh:
			log.Println("Payout processor shutting down")
			return
		case <-ticker.C:
		}
		// Continue with payout processing
		// Get current height
		heightResp, err := rpcCall(nodeURL, "getblockcount", []interface{}{})
		if err != nil {
			log.Printf("Failed to get block height: %v", err)
			continue
		}
		heightFloat, ok := heightResp.(float64)
		if !ok {
			log.Printf("Unexpected response type for getblockcount: %T", heightResp)
			continue
		}
		currentHeight := int64(heightFloat)

		// Periodic dust balance logging
		dustLogCounter++
		if dustLogCounter >= 10 {
			dustLogCounter = 0
			totalDust := stats.GetTotalDust(currentHeight, minPayout)
			if totalDust > 0 {
				dustCount := len(stats.GetDustBalances(currentHeight, minPayout))
				log.Printf("Dust balances: %.8f BCH2 across %d miners (below %.2f min payout)",
					totalDust, dustCount, minPayout)
			}
		}

		// Get ready payouts using global minPayout config
		ready := stats.GetReadyPayouts(currentHeight, minPayout)
		if len(ready) == 0 {
			continue
		}

		for address, amount := range ready {
			// Skip if exceeded max retries (will be retried after pool restart)
			if failedPayouts[address] >= maxRetries {
				continue
			}

			// Handle split payouts for large amounts
			remainingAmount := amount
			var txids []string

			for remainingAmount > 0 {
				// Limit per transaction to avoid "transaction too large"
				payAmount := remainingAmount
				if payAmount > 1000.0 {
					payAmount = 1000.0
				}

				// Skip invalid amounts (NaN, Inf, zero, negative)
				if payAmount <= 0 || math.IsNaN(payAmount) || math.IsInf(payAmount, 0) {
					log.Printf("Skipping invalid payout amount for %s: %v", address, payAmount)
					break
				}

				// Round to 8 decimal places to avoid RPC issues
				payAmount = math.Round(payAmount*1e8) / 1e8

				// Create and send transaction with retry
				txid, err := sendPayoutWithRetry(nodeURL, address, payAmount, 3)
				if err != nil {
					failedPayouts[address]++
					log.Printf("Payout failed for %s (amount: %.8f, attempt %d/%d): %v",
						address, payAmount, failedPayouts[address], maxRetries, err)
					break
				}

				txids = append(txids, txid)
				remainingAmount -= payAmount

				// CRITICAL FIX: Update DB first, then memory
				// This prevents double-payouts if DB update fails
				err = stats.MarkMaturePaidInDBWithAmount(address, currentHeight, txid, payAmount)
				if err != nil {
					// DB update failed - log critical error but don't mark memory as paid
					log.Printf("CRITICAL: Payout sent but DB update failed for %s (txid: %s): %v",
						address, txid, err)
					log.Printf("MANUAL ACTION REQUIRED: Verify txid %s and update database", txid)
					break
				}

				// Update memory with partial payment tracking
				stats.MarkMaturePaidWithAmount(address, currentHeight, txid, payAmount)

				log.Printf("Payout sent: %s -> %.8f BCH2 (txid: %s)%s",
					address, payAmount, txid,
					func() string {
						if remainingAmount > 0 {
							return fmt.Sprintf(" [split: %.8f remaining]", remainingAmount)
						}
						return ""
					}())
			}

			// Clear retry counter on any successful payment
			if len(txids) > 0 {
				delete(failedPayouts, address)
				if len(txids) > 1 {
					log.Printf("Split payout complete for %s: %d transactions, total %.8f BCH2",
						address, len(txids), amount)
				}
			}
		}

		// Periodic cleanup of old paid payouts from memory (every cycle)
		stats.CleanupPaidPayouts()
	}
}

// sendPayoutWithRetry attempts to send a payout with exponential backoff retry
func sendPayoutWithRetry(rpcURL, address string, amount float64, maxAttempts int) (string, error) {
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		txid, err := sendPayout(rpcURL, address, amount)
		if err == nil {
			return txid, nil
		}
		lastErr = err

		// Don't retry on invalid address/amount errors (won't succeed)
		errStr := err.Error()
		if strings.Contains(errStr, "Invalid address") || strings.Contains(errStr, "Invalid amount") {
			return "", err
		}

		// Exponential backoff: 1s, 2s, 4s
		if attempt < maxAttempts {
			backoff := time.Duration(1<<uint(attempt-1)) * time.Second
			log.Printf("Payout attempt %d failed for %s, retrying in %v: %v", attempt, address, backoff, err)
			time.Sleep(backoff)
		}
	}
	return "", fmt.Errorf("failed after %d attempts: %w", maxAttempts, lastErr)
}

// getRPCCredentials returns RPC credentials from environment variables
func getRPCCredentials() (string, string) {
	user := os.Getenv("RPC_USER")
	if user == "" {
		user = os.Getenv("FORGE_RPC_USER")
	}
	pass := os.Getenv("RPC_PASSWORD")
	if pass == "" {
		pass = os.Getenv("FORGE_RPC_PASSWORD")
	}
	return user, pass
}

func rpcCall(url, method string, params []interface{}) (interface{}, error) {
	reqBody, err := json.Marshal(map[string]interface{}{
		"jsonrpc": "1.0",
		"method":  method,
		"params":  params,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal RPC request: %w", err)
	}

	req, err := http.NewRequest("POST", url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create RPC request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	rpcUser, rpcPass := getRPCCredentials()
	if rpcUser == "" || rpcPass == "" {
		return nil, fmt.Errorf("RPC credentials not configured - set RPC_USER and RPC_PASSWORD environment variables")
	}
	req.SetBasicAuth(rpcUser, rpcPass)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result struct {
		Result interface{} `json:"result"`
		Error  interface{} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode RPC response: %w", err)
	}

	if result.Error != nil {
		return nil, fmt.Errorf("RPC error: %v", result.Error)
	}
	return result.Result, nil
}


// bitsToDifficulty converts compact "bits" from block template to difficulty
func bitsToDifficulty(bitsHex string) float64 {
	bits, err := strconv.ParseUint(bitsHex, 16, 32)
	if err != nil || bits == 0 {
		return 0
	}
	exp := bits >> 24
	mantissa := bits & 0xFFFFFF
	if mantissa == 0 {
		return 0
	}
	// diff1 target exponent = 0x1d (29)
	// difficulty = (0xFFFF / mantissa) * 256^(29 - exp)
	return (float64(0xFFFF) / float64(mantissa)) * math.Pow(256, float64(29-exp))
}
func sendPayout(rpcURL, address string, amount float64) (string, error) {
	result, err := rpcCall(rpcURL, "sendtoaddress", []interface{}{address, amount})
	if err != nil {
		return "", err
	}
	txid, ok := result.(string)
	if !ok {
		return "", fmt.Errorf("unexpected response type for sendtoaddress: %T", result)
	}
	return txid, nil
}

// sendWebhookAlert sends a webhook notification for important events
func sendWebhookAlert(event string, data map[string]interface{}) {
	webhookURL := os.Getenv("WEBHOOK_URL")
	if webhookURL == "" {
		return // No webhook configured
	}

	payload := map[string]interface{}{
		"event":     event,
		"timestamp": time.Now().UTC().Format(time.RFC3339),
		"pool":      "Forge Pool",
		"data":      data,
	}

	jsonData, err := json.Marshal(payload)
	if err != nil {
		log.Printf("Failed to marshal webhook payload: %v", err)
		return
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(webhookURL, "application/json", bytes.NewReader(jsonData))
	if err != nil {
		log.Printf("Failed to send webhook: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		log.Printf("Webhook returned status %d", resp.StatusCode)
	}
}

func main() {
	configPath := flag.String("config", "configs/config.yaml", "Path to config file")
	flag.Parse()

	var logErr error
	logger, logErr = zap.NewProduction()
	if logErr != nil {
		log.Fatalf("Failed to initialize logger: %v", logErr)
	}
	defer logger.Sync()

	logger.Info("🔥 Forge Pool - BCH2 Mining Pool")

	// Initialize database with credentials from environment
	dbConnStr := stats.GetDBConnStr()
	if dbErr := stats.InitDB(dbConnStr); dbErr != nil {
		logger.Warn("Database not available, using memory only", zap.Error(dbErr))
	} else {
		logger.Info("✅ Connected to PostgreSQL database")
		stats.LoadAllPendingPayouts()
		// Note: startPayoutProcessor is started later after config is loaded
	}
	defer stats.CloseDB()

	config, err := loadConfig(*configPath)
	if err != nil {
		logger.Fatal("Failed to load config", zap.Error(err))
	}

	serverConfig := &stratum.ServerConfig{
		Host:               config.GetString("stratum.host"),
		Port:               config.GetInt("stratum.port"),
		MaxConnections:     config.GetInt("stratum.max_connections"),
		BanDuration:        config.GetDuration("stratum.ban_duration"),
		MaxSharesPerSecond: config.GetInt("stratum.max_shares_per_second"),
		VardiffEnabled:     config.GetBool("stratum.vardiff.enabled"),
		MinDiff:            config.GetFloat64("stratum.vardiff.min_diff"),
		RentalMinDiff:      config.GetFloat64("stratum.vardiff.rental_min_diff"),
		RentalMaxDiff:      config.GetFloat64("stratum.vardiff.rental_max_diff"),
		MaxDiff:            config.GetFloat64("stratum.vardiff.max_diff"),
		TargetShareTime:    config.GetInt("stratum.vardiff.target_time"),
		RetargetTime:       config.GetInt("stratum.vardiff.retarget_time"),
		HighHashThreshold:  config.GetInt("stratum.high_hash_threshold"),
		HighHashDiff:       config.GetFloat64("stratum.high_hash_diff"),
		ExtraNonce1Size:    config.GetInt("stratum.extranonce1_size"),
		ExtraNonce2Size:    config.GetInt("stratum.extranonce2_size"),
		ServerName:         "main",
	}

	// Build RPC URL from config
	nodeHost := config.GetString("node.host")
	nodePort := config.GetInt("node.port")
	nodeSSL := config.GetBool("node.use_ssl")
	if nodeHost == "" {
		nodeHost = "127.0.0.1"
	}
	if nodePort == 0 {
		nodePort = 8342
	}
	protocol := "http"
	if nodeSSL {
		protocol = "https"
	}
	rpcURL = fmt.Sprintf("%s://%s:%d", protocol, nodeHost, nodePort)
	logger.Info("RPC URL configured", zap.String("url", rpcURL))

	rpcUser, rpcPass = getRPCCredentials()

	// Load pool configuration
	poolAddress = config.GetString("pool.address")
	poolFee = config.GetFloat64("pool.fee")
	soloFee = config.GetFloat64("pool.solo_fee")
	blockReward = config.GetFloat64("pool.block_reward")
	minPayout = config.GetFloat64("pool.min_payout")
	pplnsWindow = config.GetInt("pool.pplns_window")
	if pplnsWindow <= 0 {
		pplnsWindow = 100000 // Default PPLNS window
	}

	logger.Info("Pool configuration loaded",
		zap.String("address", poolAddress),
		zap.Float64("fee", poolFee),
		zap.Float64("solo_fee", soloFee),
		zap.Float64("block_reward", blockReward),
		zap.Float64("min_payout", minPayout),
		zap.Int("pplns_window", pplnsWindow))

	// Start payout processor now that config is loaded
	if stats.IsDBConnected() {
		go startPayoutProcessor()
		logger.Info("💰 Payout processor started")
	}

	logger.Info("Vardiff configuration",
		zap.Bool("enabled", serverConfig.VardiffEnabled),
		zap.Float64("min_diff", serverConfig.MinDiff),
		zap.Float64("rental_min_diff", serverConfig.RentalMinDiff),
		zap.Float64("max_diff", serverConfig.MaxDiff),
		zap.Int("target_time", serverConfig.TargetShareTime),
		zap.Int("retarget_time", serverConfig.RetargetTime))

	jobManager = mining.NewJobManager(rpcURL, rpcUser, rpcPass, poolAddress)

	shareProcessor := &BlockFindingShareProcessor{logger: logger}
	// Create API-backed miner settings store
	apiHost := os.Getenv("API_HOST")
	if apiHost == "" {
		apiHost = "127.0.0.1"
	}
	apiPort := os.Getenv("API_PORT")
	if apiPort == "" {
		apiPort = "8080"
	}
	minerSettings := stratum.NewAPIMinerSettings(fmt.Sprintf("http://%s:%s", apiHost, apiPort))
	stratumServer = stratum.NewServer(serverConfig, logger, shareProcessor, minerSettings)

	if err := stratumServer.Start(); err != nil {
		logger.Fatal("Failed to start", zap.Error(err))
	}

	// Start Braiins-compatible stratum server (8-byte extranonce2)
	if config.GetBool("stratum_braiins.enabled") {
		braiinsConfig := &stratum.ServerConfig{
			Host:            config.GetString("stratum_braiins.host"),
			Port:            config.GetInt("stratum_braiins.port"),
			MaxConnections:  config.GetInt("stratum_braiins.max_connections"),
			VardiffEnabled:  config.GetBool("stratum_braiins.vardiff.enabled"),
			MinDiff:         config.GetFloat64("stratum_braiins.vardiff.min_diff"),
			MaxDiff:         config.GetFloat64("stratum_braiins.vardiff.max_diff"),
			TargetShareTime: config.GetInt("stratum_braiins.vardiff.target_time"),
			RetargetTime:    config.GetInt("stratum_braiins.vardiff.retarget_time"),
			ExtraNonce1Size: config.GetInt("stratum_braiins.extranonce1_size"),
			ExtraNonce2Size: config.GetInt("stratum_braiins.extranonce2_size"),
			ServerName:      "braiins",
		}
		stratumBraiinsServer = stratum.NewServer(braiinsConfig, logger, shareProcessor, minerSettings)
		if err := stratumBraiinsServer.Start(); err != nil {
			logger.Error("Failed to start Braiins stratum", zap.Error(err))
		} else {
			logger.Info("✅ Braiins stratum running",
				zap.Int("port", braiinsConfig.Port),
				zap.Int("extranonce2_size", braiinsConfig.ExtraNonce2Size))
		}
	}

	// Start Stratum V2 server if enabled
	if config.GetBool("stratumv2.enabled") {
		v2Config := &stratumv2.ServerConfig{
			Host:              config.GetString("stratumv2.host"),
			Port:              config.GetInt("stratumv2.port"),
			MaxConnections:    config.GetInt("stratumv2.max_connections"),
			MinDiff:           config.GetFloat64("stratumv2.vardiff.min_diff"),
			MaxDiff:           config.GetFloat64("stratumv2.vardiff.max_diff"),
			TargetShareTime:   config.GetInt("stratumv2.vardiff.target_time"),
			RetargetTime:      config.GetInt("stratumv2.vardiff.retarget_time"),
			RequireEncryption: config.GetBool("stratumv2.require_encryption"),
			ExtranonceSize:    config.GetInt("stratumv2.extranonce_size"),
		}

		// Create V2 share processor that bridges to V1 processing
		v2ShareProcessor := &V2ShareProcessor{logger: logger}
		v2MinerSettings := &V2MinerSettingsAdapter{v1Settings: minerSettings}

		var err error
		stratumV2Server, err = stratumv2.NewServer(v2Config, logger, v2ShareProcessor, v2MinerSettings)
		if err != nil {
			logger.Error("Failed to create V2 server", zap.Error(err))
		} else {
			if err := stratumV2Server.Start(); err != nil {
				logger.Error("Failed to start V2 server", zap.Error(err))
			} else {
				logger.Info("✅ Stratum V2 server running",
					zap.Int("port", v2Config.Port),
					zap.Bool("encryption", v2Config.RequireEncryption))
			}
		}
	}

	// Start worker timeout detection (marks workers offline after 5 min of no shares)
	workerTimeoutStop := make(chan struct{})
	go stats.GetManager().StartWorkerTimeoutChecker(workerTimeoutStop)

	go startStatsServer()
	logger.Info("✅ Stratum server running", zap.Int("port", serverConfig.Port))

	// Job broadcast loop
	// Miners expect periodic job updates to confirm pool is alive
	// Send new jobs on:
	//   1. New block height (CleanJobs=true) - immediately
	//   2. Periodic ntime update (CleanJobs=false) - every 30 seconds
	go func() {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()

		var lastHeight int64
		var lastJobTime time.Time

		for {
			select {
			case <-shutdownCh:
				logger.Info("Job broadcast loop shutting down")
				return
			case <-ticker.C:
				template, err := jobManager.GetBlockTemplate()
				if err != nil {
					logger.Error("Failed to get block template", zap.Error(err))
					continue
				}

				// Update network difficulty from block template bits (actual next-block target)
				if templateDiff := bitsToDifficulty(template.Bits); templateDiff > 0 {
					oldDiff := getNetworkDifficulty()
					if templateDiff != oldDiff {
						logger.Info("Network difficulty updated from template",
							zap.Float64("old_diff", oldDiff),
							zap.Float64("new_diff", templateDiff),
							zap.String("bits", template.Bits))
					}
					setNetworkDifficulty(templateDiff)
				}

				curJob := getCurrentJob()
				isNewBlock := template.Height != lastHeight || curJob == nil
				needPeriodicUpdate := time.Since(lastJobTime) >= 15*time.Second // Faster updates for NiceHash

				if isNewBlock || needPeriodicUpdate {
					job := jobManager.CreateJob(template)
					setCurrentJob(job)

					// Store job in history for block submission lookup
					jobHistoryMu.Lock()
					jobHistory[job.ID] = job
					jobHistoryOrder = append(jobHistoryOrder, job.ID)
					// Clean old jobs using FIFO (keep last 100)
					for len(jobHistoryOrder) > 100 {
						oldestID := jobHistoryOrder[0]
						jobHistoryOrder = jobHistoryOrder[1:]
						delete(jobHistory, oldestID)
					}
					jobHistoryMu.Unlock()

					// CleanJobs=true only for new blocks, false for periodic updates
					cleanJobs := isNewBlock

					stratumJob := &stratum.Job{
						ID:               job.ID,
						Height:           job.Height,
						PrevBlockHash:    job.PrevBlockHash,
						OriginalPrevHash: job.OriginalPrevHash,
						CoinBase1:        job.CoinBase1,
						CoinBase2:        job.CoinBase2,
						MerkleBranches:   job.MerkleBranches,
						Version:          job.Version,
						NBits:            job.NBits,
						NTime:            job.NTime,
						CleanJobs:        cleanJobs,
						Target:           job.Target,
						CreatedAt:        time.Now(),
						Transactions:     job.Transactions,
					}
					stratumServer.BroadcastJob(stratumJob)

					// Broadcast to Braiins server if enabled
					if stratumBraiinsServer != nil {
						stratumBraiinsServer.BroadcastJob(stratumJob)
					}

					// Broadcast to V2 server if enabled
					if stratumV2Server != nil {
						v2JobID := atomic.AddUint32(&v2JobIDCounter, 1)
						v2Job, err := stratumv2.ConvertV1ToV2Job(&stratumv2.V1JobData{
							ID:               job.ID,
							Height:           job.Height,
							PrevBlockHash:    job.PrevBlockHash,
							OriginalPrevHash: job.OriginalPrevHash,
							CoinBase1:        job.CoinBase1,
							CoinBase2:        job.CoinBase2,
							MerkleBranches:   job.MerkleBranches,
							Version:          job.Version,
							NBits:            job.NBits,
							NTime:            job.NTime,
							CleanJobs:        cleanJobs,
							CreatedAt:        time.Now(),
							Transactions:     job.Transactions,
						}, v2JobID)
						if err == nil {
							stratumV2Server.BroadcastJob(v2Job)
						}
					}

					if isNewBlock {
						logger.Info("📢 New block job broadcast",
							zap.Int64("height", template.Height),
							zap.String("job_id", job.ID))
					} else {
						logger.Debug("📢 Periodic job update",
							zap.Int64("height", template.Height),
							zap.String("job_id", job.ID))
					}

					lastHeight = template.Height
					lastJobTime = time.Now()
				}
			}
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	logger.Info("Shutting down...")
	close(shutdownCh)        // Signal all goroutines to stop
	close(workerTimeoutStop) // Stop worker timeout checker
	if stratumV2Server != nil {
		stratumV2Server.Stop()
	}
	if stratumBraiinsServer != nil {
		stratumBraiinsServer.Stop()
	}
	stratumServer.Stop()
}

func loadConfig(path string) (*viper.Viper, error) {
	v := viper.New()
	v.SetConfigFile(path)
	v.SetConfigType("yaml")

	// Stratum defaults
	v.SetDefault("stratum.host", "0.0.0.0")
	v.SetDefault("stratum.port", 3333)
	v.SetDefault("stratum.max_connections", 10000)
	v.SetDefault("stratum.ban_duration", "10m")
	v.SetDefault("stratum.max_shares_per_second", 100)
	v.SetDefault("stratum.vardiff.enabled", true)
	v.SetDefault("stratum.vardiff.min_diff", 32768)
	v.SetDefault("stratum.vardiff.rental_min_diff", 500000)  // NiceHash/MRR require 500k+
	v.SetDefault("stratum.vardiff.rental_max_diff", 50000000) // Cap NiceHash/MRR at 50M for high-hashrate orders
	v.SetDefault("stratum.vardiff.max_diff", 1000000000)
	v.SetDefault("stratum.vardiff.target_time", 10)
	v.SetDefault("stratum.high_hash_threshold", 10)
	v.SetDefault("stratum.high_hash_diff", 1000000)

	// Node defaults - IMPORTANT: Set RPC_USER and RPC_PASSWORD env vars
	// DO NOT use default credentials in production
	v.SetDefault("node.user", "")
	v.SetDefault("node.password", "")

	// Pool defaults
	v.SetDefault("pool.fee", 1.0)
	v.SetDefault("pool.solo_fee", 0.5)
	v.SetDefault("pool.block_reward", 50.0)
	v.SetDefault("pool.min_payout", 5.0)
	v.SetDefault("pool.address", "") // Must be set in config or env

	if err := v.ReadInConfig(); err != nil {
		return nil, err
	}
	return v, nil
}

type BlockFindingShareProcessor struct {
	logger *zap.Logger
}

func (p *BlockFindingShareProcessor) ProcessShare(ctx context.Context, share *stratum.Share) error {
	mode := "PPLNS"
	if share.IsSolo {
		mode = "SOLO"
	}

	networkDiff := getNetworkDifficulty()

	// Track worker stats - log the difficulty being recorded for verification
	p.logger.Debug("Recording share for hashrate",
		zap.String("miner", share.MinerID),
		zap.Float64("target_diff", share.Difficulty),
		zap.Float64("actual_diff", share.ActualDiff))
	stats.GetManager().UpdateWorker(share.MinerID, share.WorkerName, true, share.Difficulty, share.ActualDiff)

	// Save share to database for PPLNS distribution
	// Use target difficulty as the credited work amount
	if err := stats.SaveShare(share.MinerID, share.WorkerName, share.Difficulty, share.IsSolo); err != nil {
		p.logger.Warn("Failed to save share to DB", zap.Error(err))
	}

	// Calculate how close this share is to network difficulty (use actual share diff)
	diffRatio := share.ActualDiff / networkDiff

	// Log exceptionally good shares (>1% of network diff)
	if diffRatio >= 0.01 {
		p.logger.Info("⚡ High difficulty share",
			zap.String("miner", share.MinerID),
			zap.Float64("actual_diff", share.ActualDiff),
			zap.Float64("network_diff", networkDiff),
			zap.Float64("ratio_percent", diffRatio*100),
			zap.String("job_id", share.JobID))
	}

	if share.ActualDiff >= networkDiff {
		p.logger.Info("🎉 BLOCK CANDIDATE!",
			zap.String("miner", share.MinerID),
			zap.Float64("actual_diff", share.ActualDiff),
			zap.Float64("network_diff", networkDiff),
			zap.String("job_id", share.JobID),
			zap.String("extranonce1", share.ExtraNonce1),
			zap.String("extranonce2", share.ExtraNonce2),
			zap.String("ntime", share.NTime),
			zap.String("nonce", share.Nonce))

		go p.submitBlock(share)
	}

	p.logger.Debug("Share processed",
		zap.String("miner", share.MinerID),
		zap.Float64("diff", share.Difficulty),
		zap.String("mode", mode))
	return nil
}

func (p *BlockFindingShareProcessor) submitBlock(share *stratum.Share) {
	// Look up the EXACT job that the share was submitted for
	// This is critical - using wrong job data would create an invalid block
	jobHistoryMu.RLock()
	job, exists := jobHistory[share.JobID]
	jobHistoryMu.RUnlock()

	if !exists {
		// Fall back to currentJob if job not in history (shouldn't happen)
		job = getCurrentJob()
		if job == nil {
			p.logger.Error("No job found to build block",
				zap.String("job_id", share.JobID))
			return
		}
		p.logger.Warn("Job not in history, using currentJob",
			zap.String("share_job_id", share.JobID),
			zap.String("current_job_id", job.ID))
	}

	// Build coinbase using the correct job's coinbase parts
	coinbase := buildCoinbase(job.CoinBase1, share.ExtraNonce1, share.ExtraNonce2, job.CoinBase2)
	coinbaseHex := hex.EncodeToString(coinbase)

	// Build block using the correct job
	blockHex := buildBlock(job, coinbase, share.NTime, share.Nonce, share.VersionBits)

	// Calculate block hash for debug
	headerBytes, _ := hex.DecodeString(blockHex[:160]) // First 80 bytes = header
	blockHash := doubleSHA256(headerBytes)
	reverseBytes(blockHash)

	p.logger.Info("Submitting block to node",
		zap.String("job_id", share.JobID),
		zap.Int64("height", job.Height),
		zap.Int("block_size", len(blockHex)/2),
		zap.String("nonce", share.Nonce),
		zap.String("ntime", share.NTime),
		zap.String("coinbase_full", coinbaseHex),
		zap.String("block_hash", hex.EncodeToString(blockHash)),
		zap.String("header_hex", blockHex[:160]))

	result, err := submitBlockToNode(blockHex)
	if err != nil {
		p.logger.Error("Failed to submit block", zap.Error(err))
		return
	}

	if result == "" {
		// Calculate payout after fee deduction
		var feePercent float64
		var mode string
		if share.IsSolo {
			feePercent = soloFee
			mode = "SOLO"
		} else {
			feePercent = poolFee
			mode = "PPLNS"
		}
		payoutAmount := blockReward * (1 - feePercent/100)
		hashStr := hex.EncodeToString(blockHash)

		p.logger.Info("🎉🎉🎉 BLOCK ACCEPTED BY NODE! 🎉🎉🎉",
			zap.Int64("height", job.Height),
			zap.String("miner", share.MinerID),
			zap.String("mode", mode),
			zap.Float64("reward", blockReward),
			zap.Float64("fee_percent", feePercent),
			zap.Float64("payout", payoutAmount))

		// Record block for miner stats with effort tracking for luck calculation
		stats.RecordMinerBlockWithWorkerSolo(share.MinerID, share.WorkerName, job.Height, hashStr, blockReward, share.IsSolo)
		stats.GetManager().RecordBlockWithEffort(hashStr, getNetworkDifficulty())

		// Send webhook alert for block found
		go sendWebhookAlert("block_found", map[string]interface{}{
			"height":      job.Height,
			"hash":        hashStr,
			"miner":       share.MinerID,
			"worker":      share.WorkerName,
			"mode":        mode,
			"reward":      blockReward,
			"payout":      payoutAmount,
			"fee_percent": feePercent,
		})

		if share.IsSolo {
			// SOLO MODE: Pay only the block finder
			stats.AddPendingPayout(share.MinerID, job.Height, payoutAmount)
			if err := stats.SavePayoutAtomicWithSolo(share.MinerID, job.Height, payoutAmount, hashStr, true); err != nil {
				p.logger.Error("Failed to save solo payout", zap.Error(err))
			}
			p.logger.Info("💰 Solo block payout credited",
				zap.String("miner", share.MinerID),
				zap.Float64("amount", payoutAmount))
		} else {
			// PPLNS MODE: Distribute reward among all PPLNS contributors
			pplnsShares, totalWork, err := stats.GetPPLNSShares(pplnsWindow)
			if err != nil || totalWork == 0 {
				// Fallback to block finder if PPLNS data unavailable
				p.logger.Warn("PPLNS shares unavailable, paying block finder only",
					zap.Error(err))
				stats.AddPendingPayout(share.MinerID, job.Height, payoutAmount)
				if err := stats.SavePayoutAtomic(share.MinerID, job.Height, payoutAmount, hashStr); err != nil {
					p.logger.Error("Failed to save payout", zap.Error(err))
				}
			} else {
				// Distribute proportionally
				p.logger.Info("📊 Distributing PPLNS rewards",
					zap.Int("contributors", len(pplnsShares)),
					zap.Float64("total_work", totalWork),
					zap.Float64("reward_pool", payoutAmount))

				for minerAddr, work := range pplnsShares {
					// Calculate proportional share with safety bounds
					proportion := work / totalWork
					if proportion > 1.0 {
						proportion = 1.0 // Cap at 100% due to floating point errors
					}
					if proportion <= 0 {
						continue // Skip invalid proportions
					}
					minerPayout := payoutAmount * proportion

					// Skip dust amounts (< 0.00001 BCH2)
					if minerPayout < 0.00001 {
						continue
					}

					stats.AddPendingPayout(minerAddr, job.Height, minerPayout)
					if err := stats.SavePayout(minerAddr, job.Height, minerPayout); err != nil {
						p.logger.Error("Failed to save PPLNS payout",
							zap.String("miner", minerAddr),
							zap.Error(err))
					}

					p.logger.Info("💰 PPLNS payout credited",
						zap.String("miner", minerAddr),
						zap.Float64("work", work),
						zap.Float64("proportion", proportion*100),
						zap.Float64("amount", minerPayout))
				}

				// Save block record
				if err := stats.SaveBlock(job.Height, hashStr, share.MinerID, blockReward); err != nil {
					p.logger.Error("Failed to save block record", zap.Error(err))
				}
			}
		}

		// Reset round stats after block found
		if share.IsSolo {
			// Solo mode: only reset the block finder's stats
			stats.GetManager().ResetWorkerRoundStats(share.MinerID)
		} else {
			// PPLNS mode: reset all workers (shared round)
			stats.GetManager().ResetAllWorkerRoundStats()
		}

		// Cleanup old shares periodically (keep 2x window)
		go func() {
			if deleted, err := stats.CleanupOldShares(pplnsWindow); err == nil && deleted > 0 {
				p.logger.Info("Cleaned up old shares", zap.Int64("deleted", deleted))
			}
		}()
	} else {
		p.logger.Warn("Block rejected by node", zap.String("reason", result))
	}
}

func (p *BlockFindingShareProcessor) ProcessBlock(ctx context.Context, block *stratum.Block) error {
	p.logger.Info("🎉 BLOCK FOUND!", zap.String("hash", block.Hash), zap.Int64("height", block.Height))
	return nil
}

func buildCoinbase(cb1, extranonce1, extranonce2, cb2 string) []byte {
	cb1Bytes, _ := hex.DecodeString(cb1)
	en1Bytes, _ := hex.DecodeString(extranonce1)
	en2Bytes, _ := hex.DecodeString(extranonce2)
	cb2Bytes, _ := hex.DecodeString(cb2)

	var coinbase bytes.Buffer
	coinbase.Write(cb1Bytes)
	coinbase.Write(en1Bytes)
	coinbase.Write(en2Bytes)
	coinbase.Write(cb2Bytes)

	return coinbase.Bytes()
}

func buildBlock(job *mining.Job, coinbase []byte, ntime, nonce, versionBits string) string {
	var block bytes.Buffer

	// Version (4 bytes) - stratum sends as hex string like "20000000"
	// For block, we need little-endian, so reverse the bytes
	versionBytes, _ := hex.DecodeString(job.Version)
	if versionBits != "" {
		vbBytes, _ := hex.DecodeString(versionBits)
		for i := 0; i < len(versionBytes) && i < len(vbBytes); i++ {
			versionBytes[i] ^= vbBytes[i]
		}
	}
	reverseBytes(versionBytes)
	block.Write(versionBytes)

	// Previous block hash (32 bytes)
	// Stratum prevhash was reversed, reverse it back for block
	prevHashBytes, _ := hex.DecodeString(job.OriginalPrevHash)
	reverseBytes(prevHashBytes)
	block.Write(prevHashBytes)

	// Merkle root calculation
	// Start with coinbase hash, then combine with merkle branches
	merkleRoot := doubleSHA256(coinbase)
	for _, branchHex := range job.MerkleBranches {
		branch, _ := hex.DecodeString(branchHex)
		combined := make([]byte, 64)
		copy(combined[:32], merkleRoot)
		copy(combined[32:], branch)
		merkleRoot = doubleSHA256(combined)
	}
	block.Write(merkleRoot)

	// Time (4 bytes) - ntime from miner is big-endian hex, need little-endian
	ntimeBytes, _ := hex.DecodeString(ntime)
	reverseBytes(ntimeBytes)
	block.Write(ntimeBytes)

	// Bits (4 bytes) - big-endian hex, need little-endian
	bitsBytes, _ := hex.DecodeString(job.NBits)
	reverseBytes(bitsBytes)
	block.Write(bitsBytes)

	// Nonce (4 bytes) - from miner, big-endian hex, need little-endian
	nonceBytes, _ := hex.DecodeString(nonce)
	reverseBytes(nonceBytes)
	block.Write(nonceBytes)

	// TX count (varint) - 1 coinbase + N transactions
	txCount := 1 + len(job.Transactions)
	writeVarInt(&block, uint64(txCount))

	// Coinbase transaction
	block.Write(coinbase)

	// Additional transactions from block template
	for _, txHex := range job.Transactions {
		txBytes, _ := hex.DecodeString(txHex)
		block.Write(txBytes)
	}

	return hex.EncodeToString(block.Bytes())
}

// writeVarInt writes a variable-length integer to the buffer
func writeVarInt(buf *bytes.Buffer, n uint64) {
	if n < 0xfd {
		buf.WriteByte(byte(n))
	} else if n <= 0xffff {
		buf.WriteByte(0xfd)
		buf.WriteByte(byte(n))
		buf.WriteByte(byte(n >> 8))
	} else if n <= 0xffffffff {
		buf.WriteByte(0xfe)
		buf.WriteByte(byte(n))
		buf.WriteByte(byte(n >> 8))
		buf.WriteByte(byte(n >> 16))
		buf.WriteByte(byte(n >> 24))
	} else {
		buf.WriteByte(0xff)
		buf.WriteByte(byte(n))
		buf.WriteByte(byte(n >> 8))
		buf.WriteByte(byte(n >> 16))
		buf.WriteByte(byte(n >> 24))
		buf.WriteByte(byte(n >> 32))
		buf.WriteByte(byte(n >> 40))
		buf.WriteByte(byte(n >> 48))
		buf.WriteByte(byte(n >> 56))
	}
}

func reverseBytes(b []byte) {
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
}

func doubleSHA256(data []byte) []byte {
	first := sha256.Sum256(data)
	second := sha256.Sum256(first[:])
	return second[:]
}

func submitBlockToNode(blockHex string) (string, error) {
	reqBody, err := json.Marshal(map[string]interface{}{
		"jsonrpc": "1.0",
		"id":      "submit",
		"method":  "submitblock",
		"params":  []interface{}{blockHex},
	})
	if err != nil {
		return "", fmt.Errorf("failed to marshal submitblock request: %w", err)
	}

	req, err := http.NewRequest("POST", rpcURL, bytes.NewReader(reqBody))
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(rpcUser, rpcPass)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	var rpcResp struct {
		Result interface{} `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}

	if err := json.Unmarshal(body, &rpcResp); err != nil {
		return "", err
	}

	if rpcResp.Error != nil {
		return rpcResp.Error.Message, nil
	}

	if rpcResp.Result == nil {
		return "", nil
	}

	return fmt.Sprintf("%v", rpcResp.Result), nil
}

// internalAuthMiddleware checks that requests come from localhost and have valid auth
// SECURITY: Token is REQUIRED for sensitive endpoints (trigger-payout, etc.)
func internalAuthMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Get real client IP, handling reverse proxies
		remoteIP := r.RemoteAddr
		// Don't trust X-Forwarded-For - could be spoofed
		// Extract just the IP without port
		if colonIdx := strings.LastIndex(remoteIP, ":"); colonIdx != -1 {
			remoteIP = remoteIP[:colonIdx]
		}
		remoteIP = strings.Trim(remoteIP, "[]") // Remove IPv6 brackets

		// Strict localhost check
		isLocalhost := remoteIP == "127.0.0.1" || remoteIP == "::1" || remoteIP == "localhost"
		if !isLocalhost {
			log.Printf("⚠️ SECURITY: Blocked internal API access from external IP: %s (path: %s)", remoteIP, r.URL.Path)
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}

		// ALWAYS require token for sensitive operations
		token := os.Getenv("INTERNAL_API_TOKEN")
		if token == "" {
			// Generate warning but allow in development (log loudly)
			log.Printf("⚠️ WARNING: INTERNAL_API_TOKEN not set - internal APIs are unprotected!")
		} else {
			authHeader := r.Header.Get("X-Internal-Token")
			if authHeader != token {
				log.Printf("⚠️ SECURITY: Invalid internal API token from: %s (path: %s)", remoteIP, r.URL.Path)
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
		}

		next(w, r)
	}
}

// HTTP server for stats
func startStatsServer() {
	http.HandleFunc("/internal/workers", internalAuthMiddleware(func(w http.ResponseWriter, r *http.Request) {
		workers := stats.GetManager().GetAllWorkerStats()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"workers": workers,
		})
	}))
	http.HandleFunc("/internal/stats", internalAuthMiddleware(func(w http.ResponseWriter, r *http.Request) {
		poolStats := stats.GetManager().GetPoolStats()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(poolStats)
	}))
	http.HandleFunc("/internal/rental-stats", internalAuthMiddleware(func(w http.ResponseWriter, r *http.Request) {
		// Get rental service statistics from stratum server
		rentalStats := stratumServer.GetRentalStats()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"nicehash_miners": rentalStats.NiceHashMiners,
			"mrr_miners":      rentalStats.MRRMiners,
			"other_rentals":   rentalStats.OtherRentals,
			"total_rentals":   rentalStats.TotalRentals,
		})
	}))
	http.HandleFunc("/internal/miner-blocks", internalAuthMiddleware(func(w http.ResponseWriter, r *http.Request) {
		minerID := r.URL.Query().Get("miner")
		blocks := stats.GetMinerBlocksDB(minerID)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"blocks": blocks,
			"total":  len(blocks),
		})
	}))
	http.HandleFunc("/internal/pool-blocks", internalAuthMiddleware(func(w http.ResponseWriter, r *http.Request) {
		page := 1
		limit := 25
		if p := r.URL.Query().Get("page"); p != "" {
			if v, err := strconv.Atoi(p); err == nil && v > 0 {
				page = v
			}
		}
		if l := r.URL.Query().Get("limit"); l != "" {
			if v, err := strconv.Atoi(l); err == nil && v > 0 && v <= 100 {
				limit = v
			}
		}
		blocks, total := stats.GetAllPoolBlocksDB(page, limit)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"blocks": blocks,
			"total":  total,
			"page":   page,
			"limit":  limit,
		})
	}))
	http.HandleFunc("/internal/miner-payouts", internalAuthMiddleware(func(w http.ResponseWriter, r *http.Request) {
		minerID := r.URL.Query().Get("miner")
		payouts, total, totalPaid := stats.GetMinerPayoutsDB(minerID)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"payouts":   payouts,
			"total":     total,
			"totalPaid": totalPaid,
		})
	}))
	http.HandleFunc("/internal/miner-solo-payouts", internalAuthMiddleware(func(w http.ResponseWriter, r *http.Request) {
		minerID := r.URL.Query().Get("miner")
		payouts, total, totalPaid := stats.GetMinerSoloPayoutsDB(minerID)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"payouts":   payouts,
			"total":     total,
			"totalPaid": totalPaid,
		})
	}))
	http.HandleFunc("/internal/miner-contributions", internalAuthMiddleware(func(w http.ResponseWriter, r *http.Request) {
		minerID := r.URL.Query().Get("miner")
		contributions := stats.GetMinerBlockContributionsDB(minerID)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"contributions": contributions,
			"total":         len(contributions),
		})
	}))
	http.HandleFunc("/internal/miner-solo-blocks", internalAuthMiddleware(func(w http.ResponseWriter, r *http.Request) {
		minerID := r.URL.Query().Get("miner")
		blocks := stats.GetMinerSoloBlocksDB(minerID)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"blocks": blocks,
			"total":  len(blocks),
		})
	}))
	http.HandleFunc("/internal/trigger-payout", internalAuthMiddleware(func(w http.ResponseWriter, r *http.Request) {
		minerID := r.URL.Query().Get("miner")

		// Validate miner address format first
		if minerID == "" || !strings.HasPrefix(minerID, "bitcoincash") {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": "Invalid miner address"})
			return
		}

		// SECURITY: Check if payout already in progress for this miner (prevent double-payout)
		payoutMuMap.Lock()
		if lastPayout, exists := payoutInProgress[minerID]; exists {
			// Check if previous payout is still within cooldown (5 minutes)
			if time.Since(lastPayout) < 5*time.Minute {
				payoutMuMap.Unlock()
				log.Printf("⚠️ SECURITY: Blocked concurrent payout request for %s (last: %v ago)", minerID, time.Since(lastPayout))
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": "Payout already in progress, please wait"})
				return
			}
		}
		// Mark payout as in progress
		payoutInProgress[minerID] = time.Now()
		payoutMuMap.Unlock()

		// SECURITY: Ensure we clear the in-progress flag on exit
		defer func() {
			payoutMuMap.Lock()
			delete(payoutInProgress, minerID)
			payoutMuMap.Unlock()
		}()

		heightResp, err := rpcCall(rpcURL, "getblockcount", []interface{}{})
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": "Failed to get height"})
			return
		}
		heightFloat, ok := heightResp.(float64)
		if !ok {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": "Invalid height response"})
			return
		}
		currentHeight := int64(heightFloat)

		// Use atomic payout processing to prevent race conditions
		pendingTxid, matureAmount, err := stats.ProcessPayoutAtomic(minerID, currentHeight, 5.0)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": err.Error()})
			return
		}

		// SECURITY: Verify balance again before sending (prevent race condition)
		verifyMature, _ := stats.GetMinerBalanceDB(minerID, currentHeight)
		if verifyMature < matureAmount {
			stats.RevertPendingPayout(pendingTxid)
			log.Printf("⚠️ SECURITY: Balance changed during payout processing for %s (expected: %.2f, got: %.2f)", minerID, matureAmount, verifyMature)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": "Balance changed during processing"})
			return
		}

		// Send the actual payout
		var totalSent float64
		var lastTxid string
		remaining := matureAmount

		for remaining >= 5 {
			payAmount := remaining
			if payAmount > 1000 {
				payAmount = 1000
			}
			txid, err := sendPayout(rpcURL, minerID, payAmount)
			if err != nil {
				// Revert the pending payout on failure
				stats.RevertPendingPayout(pendingTxid)
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": "Payout send failed: " + err.Error()})
				return
			}
			lastTxid = txid
			totalSent += payAmount
			remaining -= payAmount
			log.Printf("💰 Manual payout: %s -> %.2f BCH2 (txid: %s)", minerID, payAmount, txid)
		}

		// Finalize the payout with actual txid
		stats.FinalizePayoutAtomic(pendingTxid, lastTxid)
		stats.MarkAllMaturePaid(minerID, currentHeight, lastTxid)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "txid": lastTxid, "amount": totalSent})
	}))
	http.HandleFunc("/internal/miner-balance", internalAuthMiddleware(func(w http.ResponseWriter, r *http.Request) {
		minerID := r.URL.Query().Get("miner")
		heightStr := r.URL.Query().Get("height")
		height := int64(0)
		if h, err := strconv.ParseInt(heightStr, 10, 64); err == nil {
			height = h
		}
		// Try database first, fall back to memory
		mature, immature := stats.GetMinerBalanceDB(minerID, height)
		if mature == 0 && immature == 0 {
			mature, immature = stats.GetMinerBalance(minerID, height)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"matureBalance":   mature,
			"immatureBalance": immature,
		})
	}))

	http.HandleFunc("/internal/validate-address", internalAuthMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		address := r.URL.Query().Get("address")
		if address == "" {
			json.NewEncoder(w).Encode(map[string]interface{}{"valid": false, "error": "No address provided"})
			return
		}
		result, err := rpcCall(rpcURL, "validateaddress", []interface{}{address})
		if err != nil {
			json.NewEncoder(w).Encode(map[string]interface{}{"valid": false, "error": err.Error()})
			return
		}
		if validResult, ok := result.(map[string]interface{}); ok {
			isValid, _ := validResult["isvalid"].(bool)
			json.NewEncoder(w).Encode(map[string]interface{}{"valid": isValid})
		} else {
			json.NewEncoder(w).Encode(map[string]interface{}{"valid": false, "error": "Invalid response"})
		}
	}))

	// Debug endpoint to verify block submission readiness
	http.HandleFunc("/internal/block-readiness", internalAuthMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		jobHistoryMu.RLock()
		jobCount := len(jobHistory)
		var jobIDs []string
		for id := range jobHistory {
			jobIDs = append(jobIDs, id)
		}
		jobHistoryMu.RUnlock()

		var currentJobInfo map[string]interface{}
		curJob := getCurrentJob()
		if curJob != nil {
			currentJobInfo = map[string]interface{}{
				"id":     curJob.ID,
				"height": curJob.Height,
				"nbits":  curJob.NBits,
			}
		}

		json.NewEncoder(w).Encode(map[string]interface{}{
			"ready":            curJob != nil && jobCount > 0,
			"network_diff":     getNetworkDifficulty(),
			"job_history_size": jobCount,
			"job_ids":          jobIDs,
			"current_job":      currentJobInfo,
			"message":          "Block submission will work when share.Difficulty >= network_diff",
		})
	}))

	// Listen only on localhost for internal endpoints
	log.Printf("Internal stats server starting on 127.0.0.1:3337")
	if err := http.ListenAndServe("127.0.0.1:3337", nil); err != nil {
		log.Printf("ERROR: Internal stats server failed: %v", err)
	}
}

// V2ShareProcessor processes shares from the V2 server
type V2ShareProcessor struct {
	logger *zap.Logger
}

func (p *V2ShareProcessor) ProcessShare(ctx context.Context, share *stratumv2.Share) error {
	mode := "PPLNS"
	if share.IsSolo {
		mode = "SOLO"
	}

	networkDiff := getNetworkDifficulty()

	// Track worker stats
	stats.GetManager().UpdateWorker(share.MinerID, share.WorkerName, true, share.Difficulty, share.ActualDiff)

	// Save share to database
	if err := stats.SaveShare(share.MinerID, share.WorkerName, share.Difficulty, share.IsSolo); err != nil {
		p.logger.Warn("Failed to save V2 share to DB", zap.Error(err))
	}

	// Check for block
	if share.ActualDiff >= networkDiff {
		p.logger.Info("🎉 V2 BLOCK CANDIDATE!",
			zap.String("miner", share.MinerID),
			zap.Float64("actual_diff", share.ActualDiff),
			zap.Float64("network_diff", networkDiff),
			zap.Uint32("job_id", share.JobID))
		// V2 block submission would go here
		// For now, we log it - full block submission requires additional work
	}

	p.logger.Debug("V2 share processed",
		zap.String("miner", share.MinerID),
		zap.Float64("diff", share.Difficulty),
		zap.String("mode", mode))
	return nil
}

func (p *V2ShareProcessor) ProcessBlock(ctx context.Context, block *stratumv2.Block) error {
	p.logger.Info("🎉 V2 BLOCK FOUND!",
		zap.String("hash", block.Hash),
		zap.Int64("height", block.Height))
	return nil
}

// V2MinerSettingsAdapter adapts V1 miner settings to V2 interface
type V2MinerSettingsAdapter struct {
	v1Settings stratum.MinerSettingsStore
}

func (a *V2MinerSettingsAdapter) GetMinerSettings(minerID string) (*stratumv2.MinerSettings, error) {
	if a.v1Settings == nil {
		return nil, nil
	}
	v1Settings, err := a.v1Settings.GetMinerSettings(minerID)
	if err != nil || v1Settings == nil {
		return nil, err
	}
	return &stratumv2.MinerSettings{
		MinerID:    v1Settings.MinerID,
		SoloMining: v1Settings.SoloMining,
		ManualDiff: v1Settings.ManualDiff,
		MinPayout:  v1Settings.MinPayout,
	}, nil
}
