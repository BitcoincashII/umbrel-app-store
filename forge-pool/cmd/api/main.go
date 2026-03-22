package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bch2/forge-pool/internal/config"
	"github.com/bch2/forge-pool/internal/stats"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/cors"
	"github.com/gofiber/fiber/v2/middleware/limiter"
	"github.com/gofiber/fiber/v2/middleware/logger"
	"go.uber.org/zap"
)

var (
	startTime          = time.Now()
	minerSettings      = make(map[string]MinerSetting)
	settingsLastChange = make(map[string]time.Time)
	settingsMu         sync.RWMutex

	rpcURL           string
	rpcUser          string
	rpcPass          string
	stratumURL       string
	internalAPIToken string
	halvingInterval  int64 = 210000 // BCH2 halving interval, configurable via HALVING_INTERVAL env
	dataDir          string         // Data directory for persistent config

	// Block cache to avoid N+1 RPC queries
	blockCache   = make(map[int64]*CachedBlock)
	blockCacheMu sync.RWMutex

	// Internal HTTP client with timeout to prevent cascading failures
	internalHTTPClient = &http.Client{Timeout: 10 * time.Second}
)

// CachedBlock stores block data with cache timestamp
type CachedBlock struct {
	Height    int64     `json:"height"`
	Hash      string    `json:"hash"`
	Time      int64     `json:"time"`
	Size      int       `json:"size"`
	TxCount   int       `json:"txCount"`
	CachedAt  time.Time `json:"-"`
}

// CashAddr charset for BCH addresses
const cashAddrCharset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"

// cashAddrPolymod computes the BCH checksum polymod
func cashAddrPolymod(values []int) uint64 {
	c := uint64(1)
	for _, d := range values {
		c0 := c >> 35
		c = ((c & 0x07ffffffff) << 5) ^ uint64(d)
		if c0&0x01 != 0 {
			c ^= 0x98f2bc8e61
		}
		if c0&0x02 != 0 {
			c ^= 0x79b76d99e2
		}
		if c0&0x04 != 0 {
			c ^= 0xf33e5fb3c4
		}
		if c0&0x08 != 0 {
			c ^= 0xae2eabe2a8
		}
		if c0&0x10 != 0 {
			c ^= 0x1e4f43e470
		}
	}
	return c ^ 1
}

// prefixToValues converts a CashAddr prefix to 5-bit values for checksum
func prefixToValues(prefix string) []int {
	values := make([]int, len(prefix)+1)
	for i, c := range prefix {
		values[i] = int(c) & 0x1f
	}
	values[len(prefix)] = 0 // separator
	return values
}

// verifyCashAddrChecksum verifies the CashAddr checksum
func verifyCashAddrChecksum(prefix string, payload []int) bool {
	prefixVals := prefixToValues(prefix)
	combined := append(prefixVals, payload...)
	return cashAddrPolymod(combined) == 0
}

// isValidBCH2Address validates a BCH2/Bitcoin Cash address format with full checksum
func isValidBCH2Address(address string) bool {
	if address == "" {
		return false
	}

	// Check for valid prefixes and extract prefix + payload
	validPrefixes := []string{
		"bitcoincashii:",
		"bitcoincash:",
		"bchtest:",
	}

	var prefix string
	var payload string
	hasValidPrefix := false

	for _, p := range validPrefixes {
		if len(address) > len(p) && address[:len(p)] == p {
			hasValidPrefix = true
			prefix = p[:len(p)-1] // Remove colon for checksum calculation
			payload = address[len(p):]
			break
		}
	}

	// Also accept addresses without prefix (legacy format starts with q, p, 1, or 3)
	if !hasValidPrefix {
		if len(address) > 0 {
			first := address[0]
			if first == 'q' || first == 'p' {
				// CashAddr without prefix - assume bitcoincash
				prefix = "bitcoincash"
				payload = address
				hasValidPrefix = true
			} else if first == '1' || first == '3' {
				// Legacy Bitcoin address - basic validation only
				if len(address) >= 26 && len(address) <= 35 {
					return true // Legacy addresses use Base58Check, not CashAddr
				}
				return false
			} else {
				return false
			}
		}
	}

	// Basic length check (CashAddr payload is typically 42 chars)
	if len(payload) < 30 || len(payload) > 60 {
		return false
	}

	// Decode payload to 5-bit values
	payloadValues := make([]int, len(payload))
	for i, c := range payload {
		idx := -1
		for j, ch := range cashAddrCharset {
			if ch == c {
				idx = j
				break
			}
		}
		if idx == -1 {
			return false // Invalid character
		}
		payloadValues[i] = idx
	}

	// Verify checksum
	return verifyCashAddrChecksum(prefix, payloadValues)
}

// normalizeAddress strips the prefix from a BCH2 address for comparison
func normalizeAddress(address string) string {
	// Lowercase for consistent comparison (stratum stores addresses lowercase)
	address = strings.ToLower(address)
	// Strip any known prefix to get the bare hash
	prefixes := []string{"bitcoincashii:", "bitcoincash:", "bchtest:"}
	for _, prefix := range prefixes {
		if len(address) > len(prefix) && address[:len(prefix)] == prefix {
			// Always return with canonical bitcoincashii: prefix
			return "bitcoincashii:" + address[len(prefix):]
		}
	}
	// Bare hash (q... or p...) - add canonical prefix
	if len(address) >= 42 && (address[0] == 'q' || address[0] == 'p') {
		return "bitcoincashii:" + address
	}
	return address
}

// addressMatches compares two addresses, ignoring prefix differences
func addressMatches(a, b string) bool {
	return normalizeAddress(a) == normalizeAddress(b)
}

func init() {
	// Load configuration from environment variables
	rpcURL = os.Getenv("RPC_URL")
	if rpcURL == "" {
		rpcURL = "http://127.0.0.1:8342"
	}
	rpcUser = os.Getenv("RPC_USER")
	if rpcUser == "" {
		rpcUser = os.Getenv("FORGE_RPC_USER")
	}
	rpcPass = os.Getenv("RPC_PASSWORD")
	if rpcPass == "" {
		rpcPass = os.Getenv("FORGE_RPC_PASSWORD")
	}
	stratumURL = os.Getenv("STRATUM_INTERNAL_URL")
	if stratumURL == "" {
		stratumURL = "http://127.0.0.1:3337"
	}
	internalAPIToken = os.Getenv("INTERNAL_API_TOKEN")
	// BCH2 halving interval (default 210000, same as Bitcoin/BCH)
	if envHalving := os.Getenv("HALVING_INTERVAL"); envHalving != "" {
		if h, err := strconv.ParseInt(envHalving, 10, 64); err == nil && h > 0 {
			halvingInterval = h
		}
	}
	// Data directory for persistent config
	dataDir = os.Getenv("DATA_DIR")
	if dataDir == "" {
		dataDir = "/data"
	}
}

type MinerSetting struct {
	Address    string  `json:"address"`
	SoloMining bool    `json:"solo_mining"`
	ManualDiff float64 `json:"manual_diff"`
	MinPayout  float64 `json:"min_payout"`
}

type WorkerStats struct {
	MinerID       string    `json:"miner_id"`
	WorkerName    string    `json:"worker_name"`
	Online        bool      `json:"online"`
	Hashrate5m    float64   `json:"hashrate_5m"`
	Hashrate60m   float64   `json:"hashrate_60m"`
	ValidShares   int64     `json:"valid_shares"`
	InvalidShares int64     `json:"invalid_shares"`
	BestDiff      float64   `json:"best_diff"`
	RoundBestDiff float64   `json:"round_best_diff"`
	ATHDiff       float64   `json:"ath_diff"`
	TotalWork     float64   `json:"total_work"`
	BlocksFound   int64     `json:"blocks_found"`
	LastShareAt   time.Time `json:"last_share_at"`
	ConnectedAt   time.Time `json:"connected_at"`
}

func main() {
	zapLogger, err := zap.NewProduction()
	if err != nil {
		panic(fmt.Sprintf("Failed to initialize logger: %v", err))
	}
	defer zapLogger.Sync()

	zapLogger.Info("🔥 Forge Pool API Server")

	// Initialize database connection for settings persistence
	dbConnStr := stats.GetDBConnStr()
	if err := stats.InitDB(dbConnStr); err != nil {
		zapLogger.Warn("Database not available, settings will not persist", zap.Error(err))
	} else {
		zapLogger.Info("✅ Connected to PostgreSQL database")
		// Load miner settings from database
		loadMinerSettingsFromDB()
		// Periodically reload miner settings from database (every 10 seconds)
		// This ensures rental service's solo_mining updates are reflected
		go func() {
			ticker := time.NewTicker(10 * time.Second)
			defer ticker.Stop()
			for range ticker.C {
				loadMinerSettingsFromDB()
			}
		}()
	}
	defer stats.CloseDB()

	app := fiber.New(fiber.Config{
		AppName: "Forge Pool API",
	})

	app.Use(logger.New())

	// Configure CORS - MUST set CORS_ORIGINS env var in production
	// Example: CORS_ORIGINS="https://pool.example.com,https://www.pool.example.com"
	corsOrigins := os.Getenv("CORS_ORIGINS")
	if corsOrigins == "" {
		// Default to localhost only - MUST be configured for production
		corsOrigins = "http://localhost:3000,http://127.0.0.1:3000"
		log.Println("WARNING: CORS_ORIGINS not set, defaulting to localhost only. Set CORS_ORIGINS env var for production.")
	}
	app.Use(cors.New(cors.Config{
		AllowOrigins:     corsOrigins,
		AllowMethods:     "GET,POST,OPTIONS",
		AllowHeaders:     "Origin,Content-Type,Accept,Authorization",
		AllowCredentials: false,
		MaxAge:           3600,
	}))

	// Rate limiting: 1000 requests per minute per IP
	app.Use(limiter.New(limiter.Config{
		Max:        1000,
		Expiration: 1 * time.Minute,
		KeyGenerator: func(c *fiber.Ctx) string {
			return c.IP()
		},
		LimitReached: func(c *fiber.Ctx) error {
			return c.Status(fiber.StatusTooManyRequests).JSON(fiber.Map{
				"error": "Rate limit exceeded. Please try again later.",
			})
		},
	}))

	// API routes FIRST
	api := app.Group("/api/v1")
	api.Get("/stats", getPoolStats)
	api.Get("/blocks", getBlocksAPI)
	api.Get("/miners/:address", getMiner)
	api.Get("/miners/:address/workers", getMinerWorkers)
	api.Get("/miners/:address/payouts", getMinerPayouts)
	api.Get("/miners/:address/solo-payouts", getMinerSoloPayouts)
	api.Get("/miners/:address/blocks", getMinerBlocks)
	api.Get("/miners/:address/solo-blocks", getMinerSoloBlocks)
	api.Get("/miners/:address/settings", getMinerSettingsAPI)
	api.Post("/miners/settings", saveMinerSettings)
        api.Post("/miners/:address/request-payout", requestPayout)
	api.Get("/network", getNetworkInfo)
	api.Get("/workers", getAllWorkers)
	api.Get("/validate-address", validateAddress)
	api.Get("/health", healthCheck)
	api.Get("/pool/config", getPoolConfig)
	api.Post("/pool/config", savePoolConfig)

	// Prometheus-style metrics endpoint
	app.Get("/metrics", func(c *fiber.Ctx) error {
		workers := getStratumWorkers()

		var totalHashrate float64
		var onlineWorkers int
		var totalShares int64

		for _, w := range workers {
			if w.Online {
				onlineWorkers++
				totalHashrate += w.Hashrate5m
			}
			totalShares += w.ValidShares
		}

		// Get block count from node
		var blockHeight int64
		if heightResult, err := rpcCall("getblockcount", []interface{}{}); err == nil {
			json.Unmarshal(heightResult, &blockHeight)
		}

		// Get pool blocks from DB
		poolBlocks := stats.GetTotalBlocksDB()

		// Output in Prometheus format
		c.Set("Content-Type", "text/plain; charset=utf-8")
		return c.SendString(fmt.Sprintf(`# HELP pool_hashrate_ths Pool hashrate in TH/s
# TYPE pool_hashrate_ths gauge
pool_hashrate_ths %.6f

# HELP pool_workers_online Number of online workers
# TYPE pool_workers_online gauge
pool_workers_online %d

# HELP pool_workers_total Total number of workers
# TYPE pool_workers_total gauge
pool_workers_total %d

# HELP pool_shares_total Total valid shares submitted
# TYPE pool_shares_total counter
pool_shares_total %d

# HELP pool_blocks_found Total blocks found by pool
# TYPE pool_blocks_found counter
pool_blocks_found %d

# HELP network_block_height Current network block height
# TYPE network_block_height gauge
network_block_height %d

# HELP pool_uptime_seconds Pool uptime in seconds
# TYPE pool_uptime_seconds gauge
pool_uptime_seconds %.0f
`,
			totalHashrate,
			onlineWorkers,
			len(workers),
			totalShares,
			poolBlocks,
			blockHeight,
			time.Since(startTime).Seconds(),
		))
	})

	app.Get("/health", func(c *fiber.Ctx) error {
		// Check components health
		health := fiber.Map{
			"status": "healthy",
			"uptime": time.Since(startTime).String(),
			"checks": fiber.Map{},
		}

		checks := health["checks"].(fiber.Map)

		// Check RPC connection
		rpcHealthy := false
		if heightResult, err := rpcCall("getblockcount", []interface{}{}); err == nil {
			var height int64
			if json.Unmarshal(heightResult, &height) == nil && height > 0 {
				rpcHealthy = true
				checks["node"] = fiber.Map{"status": "healthy", "height": height}
			}
		}
		if !rpcHealthy {
			checks["node"] = fiber.Map{"status": "unhealthy", "error": "Cannot connect to BCH2 node"}
			health["status"] = "degraded"
		}

		// Check stratum connection
		stratumHealthy := false
		if resp, err := internalAPIGet(stratumURL + "/internal/stats"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				stratumHealthy = true
				checks["stratum"] = fiber.Map{"status": "healthy"}
			}
		}
		if !stratumHealthy {
			checks["stratum"] = fiber.Map{"status": "unhealthy", "error": "Cannot connect to stratum server"}
			health["status"] = "degraded"
		}

		// Check database
		if stats.IsDBConnected() {
			checks["database"] = fiber.Map{"status": "healthy"}
		} else {
			checks["database"] = fiber.Map{"status": "unavailable", "error": "Database not connected"}
		}

		// Set HTTP status based on health
		if health["status"] == "healthy" {
			return c.JSON(health)
		}
		return c.Status(503).JSON(health)
	})

	// Static HTML pages
	app.Get("/settings", func(c *fiber.Ctx) error {
		return c.SendFile("./web/dist/settings.html")
	})
	app.Get("/start", func(c *fiber.Ctx) error {
		return c.SendFile("./web/dist/start.html")
	})
	app.Get("/miner/*", func(c *fiber.Ctx) error {
		return c.SendFile("./web/dist/miner.html")
	})
	app.Get("/solo/*", func(c *fiber.Ctx) error {
		return c.SendFile("./web/dist/solo.html")
	})
	app.Get("/blocks", func(c *fiber.Ctx) error {
		return c.SendFile("./web/dist/blocks.html")
	})
	app.Get("/help", func(c *fiber.Ctx) error {
		return c.SendFile("./web/dist/help.html")
	})

	// Static files
	app.Static("/", "./web/dist")

	// Fallback
	app.Get("/*", func(c *fiber.Ctx) error {
		return c.SendFile("./web/dist/index.html")
	})

	go func() {
		if err := app.Listen(":8080"); err != nil {
			zapLogger.Fatal("Server error", zap.Error(err))
		}
	}()

	zapLogger.Info("✅ API server running on :8080")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	zapLogger.Info("Shutting down...")
	app.Shutdown()
}

func rpcCall(method string, params interface{}) (json.RawMessage, error) {
	if rpcUser == "" || rpcPass == "" {
		return nil, fmt.Errorf("RPC credentials not configured - set RPC_USER and RPC_PASSWORD environment variables")
	}

	reqBody, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "1.0",
		"id":      "api",
		"method":  method,
		"params":  params,
	})

	req, err := http.NewRequest("POST", rpcURL, bytes.NewBuffer(reqBody))
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(rpcUser, rpcPass)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var rpcResp struct {
		Result json.RawMessage `json:"result"`
		Error  interface{}     `json:"error"`
	}
	json.Unmarshal(body, &rpcResp)
	return rpcResp.Result, nil
}

// internalAPIGet makes a GET request to internal stratum API with auth token
func internalAPIGet(urlPath string) (*http.Response, error) {
	req, err := http.NewRequest("GET", urlPath, nil)
	if err != nil {
		return nil, err
	}
	if internalAPIToken != "" {
		req.Header.Set("X-Internal-Token", internalAPIToken)
	}
	return internalHTTPClient.Do(req)
}

func getStratumWorkers() []WorkerStats {
	resp, err := internalAPIGet(stratumURL + "/internal/workers")
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	var result struct {
		Workers []WorkerStats `json:"workers"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	return result.Workers
}

// isActiveMiner checks if a miner has submitted shares in the last 10 minutes
// OR has a balance in the database (proving historical mining activity)
func isActiveMiner(address string) bool {
	normalizedAddr := normalizeAddress(address)

	// First check: actively mining (shares in last 10 minutes)
	workers := getStratumWorkers()
	if workers != nil {
		for _, w := range workers {
			if addressMatches(w.MinerID, address) {
				if time.Since(w.LastShareAt) < 10*time.Minute {
					return true
				}
			}
		}
	}

	// Second check: has balance in database (historical mining activity)
	// This proves they mined before and have pending rewards
	balanceURL := fmt.Sprintf("%s/internal/miner-balance?miner=%s&height=0", stratumURL, url.QueryEscape(normalizedAddr))
	resp, err := internalAPIGet(balanceURL)
	if err == nil {
		defer resp.Body.Close()
		var balanceData struct {
			MatureBalance   float64 `json:"matureBalance"`
			ImmatureBalance float64 `json:"immatureBalance"`
		}
		if json.NewDecoder(resp.Body).Decode(&balanceData) == nil {
			if balanceData.MatureBalance > 0 || balanceData.ImmatureBalance > 0 {
				return true
			}
		}
	}

	return false
}

func getPoolStats(c *fiber.Ctx) error {
	heightResult, _ := rpcCall("getblockcount", []interface{}{})
	var height int64
	json.Unmarshal(heightResult, &height)

	diffResult, _ := rpcCall("getdifficulty", []interface{}{})
	var difficulty float64
	json.Unmarshal(diffResult, &difficulty)

	nethashResult, _ := rpcCall("getnetworkhashps", []interface{}{})
	var networkHashrate float64
	json.Unmarshal(nethashResult, &networkHashrate)

	infoResult, _ := rpcCall("getblockchaininfo", []interface{}{})
	var info struct {
		Chain         string `json:"chain"`
		BestBlockHash string `json:"bestblockhash"`
	}
	json.Unmarshal(infoResult, &info)

	// Get worker stats and pool stats from stratum
	workers := getStratumWorkers()
	var totalHashrate float64
	var onlineWorkers int
	minerSet := make(map[string]bool)
	for _, w := range workers {
		if w.Online {
			totalHashrate += w.Hashrate5m // Use 5-minute average for more responsive display
			onlineWorkers++
		}
		minerSet[w.MinerID] = true
	}

	// Get pool blocks found and luck from stratum internal stats
	var blocksFound int64
	var avgLuck float64 = 1.0 // Default to 100% (neutral luck)
	if resp, err := internalAPIGet(stratumURL + "/internal/stats"); err == nil {
		defer resp.Body.Close()
		var poolStats struct {
			BlocksFound int64   `json:"blocks_found"`
			AvgLuck     float64 `json:"avg_luck"`
		}
		json.NewDecoder(resp.Body).Decode(&poolStats)
		blocksFound = poolStats.BlocksFound
		if poolStats.AvgLuck > 0 {
			avgLuck = poolStats.AvgLuck
		}
	}

	// Get rental stats from stratum
	var rentalStats struct {
		NiceHashMiners int64 `json:"nicehash_miners"`
		MRRMiners      int64 `json:"mrr_miners"`
		OtherRentals   int64 `json:"other_rentals"`
		TotalRentals   int64 `json:"total_rentals"`
	}
	if resp, err := internalAPIGet(stratumURL + "/internal/rental-stats"); err == nil {
		defer resp.Body.Close()
		json.NewDecoder(resp.Body).Decode(&rentalStats)
	}

	hashrateStr := "0 H/s"
	if totalHashrate >= 1000 {
		hashrateStr = fmt.Sprintf("%.2f PH/s", totalHashrate/1000)
	} else if totalHashrate >= 1 {
		hashrateStr = fmt.Sprintf("%.1f TH/s", totalHashrate)
	} else if totalHashrate > 0 {
		hashrateStr = fmt.Sprintf("%.2f TH/s", totalHashrate)
	}

	return c.JSON(fiber.Map{
		"hashrate":          hashrateStr,
		"hashrateRaw":       totalHashrate * 1e12,
		"workers":           onlineWorkers,
		"miners":            len(minerSet),
		"blocksFound":       blocksFound,
		"blocksPending":     0,
		"poolFee":           1.0,
		"soloFee":           0.5,
		"minPayout":         5.0,
		"currentHeight":     height,
		"networkDifficulty": difficulty,
		"networkHashrate":   networkHashrate,
		"bestBlockHash":     info.BestBlockHash,
		"uptime":            time.Since(startTime).String(),
		"luck":              avgLuck, // Average luck over recent blocks (1.0 = 100%)
		"rentals": fiber.Map{
			"nicehash": rentalStats.NiceHashMiners,
			"mrr":      rentalStats.MRRMiners,
			"other":    rentalStats.OtherRentals,
			"total":    rentalStats.TotalRentals,
		},
	})
}

// getBlockCached retrieves a block from cache or fetches it from node
func getBlockCached(height int64) (*CachedBlock, error) {
	// Check cache first
	blockCacheMu.RLock()
	if cached, ok := blockCache[height]; ok {
		// Cache blocks for 1 minute (recent) or forever (confirmed)
		if time.Since(cached.CachedAt) < time.Minute {
			blockCacheMu.RUnlock()
			return cached, nil
		}
	}
	blockCacheMu.RUnlock()

	// Fetch from node
	hashResult, err := rpcCall("getblockhash", []interface{}{height})
	if err != nil {
		return nil, err
	}
	var hash string
	if err := json.Unmarshal(hashResult, &hash); err != nil {
		return nil, err
	}

	blockResult, err := rpcCall("getblock", []interface{}{hash})
	if err != nil {
		return nil, err
	}
	var block struct {
		Height int64  `json:"height"`
		Hash   string `json:"hash"`
		Time   int64  `json:"time"`
		Size   int    `json:"size"`
		NumTx  int    `json:"nTx"`
	}
	if err := json.Unmarshal(blockResult, &block); err != nil {
		return nil, err
	}

	cached := &CachedBlock{
		Height:   block.Height,
		Hash:     hash,
		Time:     block.Time,
		Size:     block.Size,
		TxCount:  block.NumTx,
		CachedAt: time.Now(),
	}

	// Store in cache
	blockCacheMu.Lock()
	blockCache[height] = cached
	// Limit cache size to last 1000 blocks
	if len(blockCache) > 1000 {
		// Find and remove oldest entries
		var minHeight int64 = height
		for h := range blockCache {
			if h < minHeight {
				minHeight = h
			}
		}
		delete(blockCache, minHeight)
	}
	blockCacheMu.Unlock()

	return cached, nil
}

func getBlocksAPI(c *fiber.Ctx) error {
	page := c.QueryInt("page", 1)
	limit := c.QueryInt("limit", 25)
	if limit > 100 {
		limit = 100 // Cap at 100 blocks per request
	}
	if limit < 1 {
		limit = 1
	}
	if page < 1 {
		page = 1
	}

	// Fetch pool-mined blocks from stratum internal endpoint
	url := fmt.Sprintf("%s/internal/pool-blocks?page=%d&limit=%d", stratumURL, page, limit)
	resp, err := internalAPIGet(url)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "Failed to fetch pool blocks"})
	}
	defer resp.Body.Close()

	var data struct {
		Blocks []struct {
			Height    int64   `json:"height"`
			Hash      string  `json:"hash"`
			Reward    float64 `json:"reward"`
			MinerAddr string  `json:"miner_address"`
			Status    string  `json:"status"`
			Time      int64   `json:"time"`
			IsSolo    bool    `json:"is_solo"`
		} `json:"blocks"`
		Total int64 `json:"total"`
		Page  int   `json:"page"`
		Limit int   `json:"limit"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "Failed to parse pool blocks"})
	}

	// Get current height for confirmation status
	var currentHeight int64
	if heightResult, err := rpcCall("getblockcount", []interface{}{}); err == nil {
		json.Unmarshal(heightResult, &currentHeight)
	}

	// Transform to API response format
	var blocks []fiber.Map
	for _, b := range data.Blocks {
		blockType := "PPLNS"
		if b.IsSolo {
			blockType = "SOLO"
		}
		blocks = append(blocks, fiber.Map{
			"height":    b.Height,
			"hash":      b.Hash,
			"time":      b.Time,
			"miner":     "Forge Pool",
			"reward":    b.Reward,
			"confirmed": b.Status == "confirmed" || (currentHeight > 0 && currentHeight-b.Height >= 6),
			"type":      blockType,
		})
	}

	return c.JSON(fiber.Map{
		"blocks": blocks,
		"total":  data.Total,
		"page":   data.Page,
		"limit":  data.Limit,
	})
}

func getMiner(c *fiber.Ctx) error {
	address, _ := url.QueryUnescape(c.Params("address"))

	// Validate address format to prevent injection attacks
	if !isValidBCH2Address(address) {
		return c.Status(400).JSON(fiber.Map{"error": "Invalid BCH2 address format"})
	}

	workers := getStratumWorkers()

	var totalHashrate5m, totalHashrate60m float64
	var totalShares, totalRejected int64
	var bestDiff float64
	var athDiff float64
	var totalWork float64
	var lastShare time.Time
	var workerCount int
	var onlineWorkers int

	for _, w := range workers {
		if addressMatches(w.MinerID, address) {
			workerCount++
			totalShares += w.ValidShares
			totalRejected += w.InvalidShares
			totalWork += w.TotalWork
			if w.BestDiff > bestDiff {
				bestDiff = w.BestDiff
			}
			if w.ATHDiff > athDiff {
				athDiff = w.ATHDiff
			}
			if w.LastShareAt.After(lastShare) {
				lastShare = w.LastShareAt
			}
			// Only count hashrate from online workers (consistent with pool stats)
			if w.Online {
				onlineWorkers++
				totalHashrate5m += w.Hashrate5m
				totalHashrate60m += w.Hashrate60m
			}
		}
	}

	// Check settings with both normalized and full address
	settingsMu.RLock()
	settings, hasSettings := minerSettings[address]
	if !hasSettings {
		settings, hasSettings = minerSettings[normalizeAddress(address)]
	}
	settingsMu.RUnlock()

	// Get current height
	currentHeight := int64(0)
	if heightResult, err := rpcCall("getblockcount", []interface{}{}); err == nil {
		json.Unmarshal(heightResult, &currentHeight)
	}

	// Get balance from stratum internal endpoint (use normalized address for lookup)
	matureBalance := 0.0
	immatureBalance := 0.0
	normalizedAddr := normalizeAddress(address)
	balanceURL := fmt.Sprintf("%s/internal/miner-balance?miner=%s&height=%d", stratumURL, url.QueryEscape(normalizedAddr), currentHeight)
	if resp, err := internalAPIGet(balanceURL); err == nil {
		defer resp.Body.Close()
		var balanceData struct {
			MatureBalance   float64 `json:"matureBalance"`
			ImmatureBalance float64 `json:"immatureBalance"`
		}
		json.NewDecoder(resp.Body).Decode(&balanceData)
		matureBalance = balanceData.MatureBalance
		immatureBalance = balanceData.ImmatureBalance
	}

	return c.JSON(fiber.Map{
		"address":         address,
		"hashrate5m":      totalHashrate5m,
		"hashrate60m":     totalHashrate60m,
		"workers":         workerCount,
		"onlineWorkers":   onlineWorkers,
		"validShares":     totalShares,
		"invalidShares":   totalRejected,
		"bestDiff":        bestDiff,
		"athDiff":         athDiff,
		"totalWork":       totalWork,
		"lastShare":       lastShare,
		"soloMining":      hasSettings && settings.SoloMining,
		"balance":         matureBalance + immatureBalance,
		"matureBalance":   matureBalance,
		"immatureBalance": immatureBalance,
		"currentHeight":   currentHeight,
		"paid":            0.0,
	})
}

func getMinerWorkers(c *fiber.Ctx) error {
	address, _ := url.QueryUnescape(c.Params("address"))
	if !isValidBCH2Address(address) {
		return c.Status(400).JSON(fiber.Map{"error": "Invalid BCH2 address format"})
	}
	allWorkers := getStratumWorkers()

	var result []fiber.Map
	for _, w := range allWorkers {
		if addressMatches(w.MinerID, address) {
			rejectRate := 0.0
			if w.ValidShares+w.InvalidShares > 0 {
				rejectRate = float64(w.InvalidShares) / float64(w.ValidShares+w.InvalidShares) * 100
			}

			result = append(result, fiber.Map{
				"name":          w.WorkerName,
				"online":        w.Online,
				"hashrate5m":    w.Hashrate5m,
				"hashrate60m":   w.Hashrate60m,
				"validShares":   w.ValidShares,
				"invalidShares": w.InvalidShares,
				"rejectRate":    rejectRate,
				"bestDiff":      w.BestDiff,
				"roundBestDiff": w.RoundBestDiff,
				"athDiff":       w.ATHDiff,
				"blocksFound":   w.BlocksFound,
				"lastShare":     w.LastShareAt,
				"connectedAt":   w.ConnectedAt,
			})
		}
	}

	return c.JSON(fiber.Map{
		"workers": result,
	})
}

func getAllWorkers(c *fiber.Ctx) error {
	workers := getStratumWorkers()

	var result []fiber.Map
	for _, w := range workers {
		result = append(result, fiber.Map{
			"minerID":       w.MinerID,
			"name":          w.WorkerName,
			"online":        w.Online,
			"hashrate5m":    w.Hashrate5m,
			"hashrate60m":   w.Hashrate60m,
			"validShares":   w.ValidShares,
			"invalidShares": w.InvalidShares,
			"bestDiff":      w.BestDiff,
			"roundBestDiff": w.RoundBestDiff,
			"athDiff":       w.ATHDiff,
			"lastShare":     w.LastShareAt,
		})
	}

	return c.JSON(fiber.Map{
		"workers": result,
	})
}
// loadMinerSettingsFromDB loads all miner settings from database into memory
func loadMinerSettingsFromDB() {
	dbSettings := stats.LoadAllMinerSettings()
	settingsMu.Lock()
	defer settingsMu.Unlock()

	for addr, s := range dbSettings {
		minerSettings[addr] = MinerSetting{
			Address:    s.Address,
			SoloMining: s.SoloMining,
			ManualDiff: s.ManualDiff,
			MinPayout:  s.MinPayout,
		}
	}
}

func saveMinerSettings(c *fiber.Ctx) error {
	var settings MinerSetting
	if err := c.BodyParser(&settings); err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "Invalid request"})
	}

	if settings.Address == "" {
		return c.Status(400).JSON(fiber.Map{"error": "Address required"})
	}

	// Check for admin token (allows pool operator to manage any miner settings)
	adminToken := os.Getenv("INTERNAL_API_TOKEN")
	authHeader := c.Get("Authorization")
	isAdmin := adminToken != "" && authHeader == "Bearer "+adminToken

	// Check if miner already has settings (existing vs new miner)
	settingsMu.RLock()
	existingSettings, hasExistingSettings := minerSettings[settings.Address]
	settingsMu.RUnlock()

	// Check if pool has any active miners at all
	workers := getStratumWorkers()
	poolHasActiveMiners := workers != nil && len(workers) > 0

	// Security: Only require active mining when actually changing the mining mode
	// Allow re-saving same settings (e.g. navigating from start page to dashboard)
	settingsChanging := hasExistingSettings && existingSettings.SoloMining != settings.SoloMining
	if !isAdmin && settingsChanging && poolHasActiveMiners && !isActiveMiner(settings.Address) {
		return c.Status(403).JSON(fiber.Map{
			"error":   "Unauthorized",
			"message": "You must be actively mining to this address to change mining mode. Connect your miner and submit some shares first.",
		})
	}

	// Validate numeric parameters (check for NaN, Inf, and bounds)
	if math.IsNaN(settings.ManualDiff) || math.IsInf(settings.ManualDiff, 0) {
		return c.Status(400).JSON(fiber.Map{"error": "Invalid manual difficulty value"})
	}
	if settings.ManualDiff < 0 {
		return c.Status(400).JSON(fiber.Map{"error": "Manual difficulty cannot be negative"})
	}
	if settings.ManualDiff > 1e15 {
		return c.Status(400).JSON(fiber.Map{"error": "Manual difficulty too high"})
	}
	if math.IsNaN(settings.MinPayout) || math.IsInf(settings.MinPayout, 0) {
		return c.Status(400).JSON(fiber.Map{"error": "Invalid minimum payout value"})
	}
	if settings.MinPayout < 0 {
		return c.Status(400).JSON(fiber.Map{"error": "Minimum payout cannot be negative"})
	}
	if settings.MinPayout > 10000 {
		return c.Status(400).JSON(fiber.Map{"error": "Minimum payout too high"})
	}

	settingsMu.Lock()
	defer settingsMu.Unlock()

	// Check cooldown (15 minutes to prevent rapid mode switching)
	cooldownDuration := 15 * time.Minute
	if lastChange, exists := settingsLastChange[settings.Address]; exists {
		timeSince := time.Since(lastChange)
		if timeSince < cooldownDuration {
			remaining := cooldownDuration - timeSince
			return c.Status(429).JSON(fiber.Map{
				"error":     "Please wait before changing settings again",
				"remaining": int(remaining.Minutes()),
				"message":   fmt.Sprintf("You can change settings again in %d minutes", int(remaining.Minutes())+1),
			})
		}
	}

	// Check if solo mode is actually changing
	oldSettings, hadOldSettings := minerSettings[settings.Address]
	modeChanged := !hadOldSettings || oldSettings.SoloMining != settings.SoloMining

	minerSettings[settings.Address] = settings

	// Save to database for persistence
	dbSettings := &stats.MinerSettings{
		Address:    settings.Address,
		SoloMining: settings.SoloMining,
		ManualDiff: settings.ManualDiff,
		MinPayout:  settings.MinPayout,
	}
	if err := stats.SaveMinerSettings(dbSettings); err != nil {
		// Log but don't fail - memory is already updated
		fmt.Printf("Warning: failed to persist settings to database: %v\n", err)
	}

	// Only update cooldown if mode changed
	if modeChanged {
		settingsLastChange[settings.Address] = time.Now()
	}

	return c.JSON(fiber.Map{"success": true, "message": "Settings saved"})
}


func getMinerSettingsAPI(c *fiber.Ctx) error {
	address, _ := url.QueryUnescape(c.Params("address"))
	if !isValidBCH2Address(address) {
		return c.Status(400).JSON(fiber.Map{"error": "Invalid BCH2 address format"})
	}

	settingsMu.RLock()
	settings, exists := minerSettings[address]
	settingsMu.RUnlock()
	
	if !exists {
		return c.JSON(fiber.Map{
			"exists":      false,
			"solo_mining": false,
			"manual_diff": 0.0,
			"vardiff":     true,
		})
	}
	
	return c.JSON(fiber.Map{
		"exists":      true,
		"address":     settings.Address,
		"solo_mining": settings.SoloMining,
		"manual_diff": settings.ManualDiff,
		"vardiff":     settings.ManualDiff == 0,
	})
}



func getMinerBlocks(c *fiber.Ctx) error {
	address, _ := url.QueryUnescape(c.Params("address"))
	if !isValidBCH2Address(address) {
		return c.Status(400).JSON(fiber.Map{"error": "Invalid BCH2 address format"})
	}

	// Fetch PPLNS block contributions from stratum internal endpoint
	normalizedAddr := normalizeAddress(address)
	resp, err := internalAPIGet(stratumURL + "/internal/miner-contributions?miner=" + url.QueryEscape(normalizedAddr))
	if err != nil {
		return c.JSON(fiber.Map{"blocks": []fiber.Map{}, "total": 0})
	}
	defer resp.Body.Close()

	var data struct {
		Contributions []struct {
			Height   int64   `json:"height"`
			Amount   float64 `json:"amount"`
			SharePct float64 `json:"share_pct"`
			Time     int64   `json:"time"`
			IsPaid   bool    `json:"is_paid"`
		} `json:"contributions"`
		Total int `json:"total"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return c.JSON(fiber.Map{"blocks": []fiber.Map{}, "total": 0})
	}

	// Convert to blocks format for frontend
	blocks := make([]fiber.Map, 0, len(data.Contributions))
	for _, c := range data.Contributions {
		blocks = append(blocks, fiber.Map{
			"height":    c.Height,
			"reward":    c.Amount,
			"share_pct": c.SharePct,
			"time":      c.Time,
			"is_paid":   c.IsPaid,
		})
	}

	return c.JSON(fiber.Map{"blocks": blocks, "total": data.Total})
}

func getMinerSoloBlocks(c *fiber.Ctx) error {
	address, _ := url.QueryUnescape(c.Params("address"))
	if !isValidBCH2Address(address) {
		return c.Status(400).JSON(fiber.Map{"error": "Invalid BCH2 address format"})
	}

	// Fetch solo blocks found by this miner
	normalizedAddr := normalizeAddress(address)
	resp, err := internalAPIGet(stratumURL + "/internal/miner-solo-blocks?miner=" + url.QueryEscape(normalizedAddr))
	if err != nil {
		return c.JSON(fiber.Map{"blocks": []fiber.Map{}, "total": 0})
	}
	defer resp.Body.Close()

	var data map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return c.JSON(fiber.Map{"blocks": []fiber.Map{}, "total": 0})
	}

	return c.JSON(data)
}

func getMinerPayouts(c *fiber.Ctx) error {
	address, _ := url.QueryUnescape(c.Params("address"))
	if !isValidBCH2Address(address) {
		return c.Status(400).JSON(fiber.Map{"error": "Invalid BCH2 address format"})
	}

        // Fetch from stratum internal endpoint (use normalized address for lookup)
        normalizedAddr := normalizeAddress(address)
        payoutsURL := fmt.Sprintf("%s/internal/miner-payouts?miner=%s", stratumURL, url.QueryEscape(normalizedAddr))
        resp, err := internalAPIGet(payoutsURL)
        if err != nil {
                return c.JSON(fiber.Map{
                        "address": address,
                        "payouts": []interface{}{},
                        "total": 0,
                        "totalPaid": 0,
                })
        }
        defer resp.Body.Close()
        
        var data map[string]interface{}
        if err := json.NewDecoder(resp.Body).Decode(&data); err != nil || data == nil {
                return c.JSON(fiber.Map{
                        "address":   address,
                        "payouts":   nil,
                        "total":     0,
                        "totalPaid": 0,
                })
        }

        return c.JSON(fiber.Map{
                "address":   address,
                "payouts":   data["payouts"],
                "total":     data["total"],
                "totalPaid": data["totalPaid"],
        })
}

func getMinerSoloPayouts(c *fiber.Ctx) error {
	address, _ := url.QueryUnescape(c.Params("address"))
	if !isValidBCH2Address(address) {
		return c.Status(400).JSON(fiber.Map{"error": "Invalid BCH2 address format"})
	}

	// Fetch from stratum internal endpoint (use normalized address for lookup)
	normalizedAddr := normalizeAddress(address)
	payoutsURL := fmt.Sprintf("%s/internal/miner-solo-payouts?miner=%s", stratumURL, url.QueryEscape(normalizedAddr))
	resp, err := internalAPIGet(payoutsURL)
	if err != nil {
		return c.JSON(fiber.Map{
			"address":   address,
			"payouts":   []interface{}{},
			"total":     0,
			"totalPaid": 0,
		})
	}
	defer resp.Body.Close()

	var data map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil || data == nil {
		return c.JSON(fiber.Map{
			"address":   address,
			"payouts":   nil,
			"total":     0,
			"totalPaid": 0,
		})
	}

	return c.JSON(fiber.Map{
		"address":   address,
		"payouts":   data["payouts"],
		"total":     data["total"],
		"totalPaid": data["totalPaid"],
	})
}

func requestPayout(c *fiber.Ctx) error {
	address, _ := url.QueryUnescape(c.Params("address"))
	normalizedAddr := normalizeAddress(address)

	// Security: Verify the miner is actively mining (proves ownership)
	if !isActiveMiner(address) {
		return c.Status(403).JSON(fiber.Map{
			"success": false,
			"error":   "Unauthorized",
			"message": "You must be actively mining to this address to request a payout. Submit shares for at least 10 minutes first.",
		})
	}

	// Get current height using shared rpcCall
	currentHeight := int64(0)
	if heightResult, err := rpcCall("getblockcount", []interface{}{}); err == nil {
		json.Unmarshal(heightResult, &currentHeight)
	}

	// Get mature balance from stratum (use normalized address for lookup)
	balanceURL := fmt.Sprintf("%s/internal/miner-balance?miner=%s&height=%d", stratumURL, url.QueryEscape(normalizedAddr), currentHeight)
	resp, err := internalAPIGet(balanceURL)
	if err != nil {
		return c.JSON(fiber.Map{"success": false, "error": "Failed to get balance"})
	}
	defer resp.Body.Close()

	var balanceData struct {
		MatureBalance float64 `json:"matureBalance"`
	}
	json.NewDecoder(resp.Body).Decode(&balanceData)

	minPayout := 5.0 // Minimum payout threshold
	if balanceData.MatureBalance < minPayout {
		return c.JSON(fiber.Map{"success": false, "error": fmt.Sprintf("Minimum payout is %.1f BCH2", minPayout)})
	}

	// Trigger payout via stratum internal endpoint (use normalized address)
	payoutURL := fmt.Sprintf("%s/internal/trigger-payout?miner=%s", stratumURL, url.QueryEscape(normalizedAddr))
	payResp, err := internalAPIGet(payoutURL)
	if err != nil {
		return c.JSON(fiber.Map{"success": false, "error": "Failed to trigger payout"})
	}
	defer payResp.Body.Close()

	var payoutResult struct {
		Success bool   `json:"success"`
		TxID    string `json:"txid"`
		Error   string `json:"error"`
	}
	json.NewDecoder(payResp.Body).Decode(&payoutResult)

	return c.JSON(fiber.Map{
		"success": payoutResult.Success,
		"txid":    payoutResult.TxID,
		"error":   payoutResult.Error,
	})
}

func getNetworkInfo(c *fiber.Ctx) error {
	heightResult, _ := rpcCall("getblockcount", []interface{}{})
	var height int64
	json.Unmarshal(heightResult, &height)

	diffResult, _ := rpcCall("getdifficulty", []interface{}{})
	var difficulty float64
	json.Unmarshal(diffResult, &difficulty)

	// Calculate current halving epoch and next halving block
	currentEpoch := height / halvingInterval
	nextHalvingBlock := (currentEpoch + 1) * halvingInterval
	blocksToHalving := nextHalvingBlock - height

	// Calculate current block reward (halves every halvingInterval blocks)
	reward := 50.0
	for i := int64(0); i < currentEpoch; i++ {
		reward /= 2
	}

	return c.JSON(fiber.Map{
		"height":          height,
		"difficulty":      difficulty,
		"reward":          reward,
		"halvingInterval": halvingInterval,
		"halvingBlock":    nextHalvingBlock,
		"blocksToHalving": blocksToHalving,
		"halvingEpoch":    currentEpoch,
	})
}

func validateAddress(c *fiber.Ctx) error {
	address := c.Query("address")
	if address == "" {
		return c.JSON(fiber.Map{"valid": false, "error": "No address provided"})
	}

	result, err := rpcCall("validateaddress", []interface{}{address})
	if err != nil {
		return c.JSON(fiber.Map{"valid": false, "error": err.Error()})
	}

	var validResult struct {
		IsValid bool `json:"isvalid"`
	}
	json.Unmarshal(result, &validResult)

	return c.JSON(fiber.Map{"valid": validResult.IsValid})
}

// healthCheck returns the health status of the API including database connectivity
func healthCheck(c *fiber.Ctx) error {
	dbConnected := stats.IsDBConnected()

	settingsMu.RLock()
	settingsCount := len(minerSettings)
	settingsMu.RUnlock()

	status := "healthy"
	if !dbConnected {
		status = "degraded"
	}

	return c.JSON(fiber.Map{
		"status":          status,
		"db_connected":    dbConnected,
		"settings_loaded": settingsCount,
	})
}

// getPoolConfig returns the current pool configuration
func getPoolConfig(c *fiber.Ctx) error {
	cfg, err := config.LoadConfig(dataDir)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "Failed to load config: " + err.Error()})
	}

	return c.JSON(fiber.Map{
		"stratum_port": cfg.StratumPort,
		"pool_wallet":  cfg.PoolWallet,
		"pool_name":    cfg.PoolName,
		"pool_fee":     cfg.PoolFee,
		"solo_fee":     cfg.SoloFee,
		"min_payout":   cfg.MinPayout,
		"updated_at":   cfg.UpdatedAt,
	})
}

// savePoolConfig saves the pool configuration
func savePoolConfig(c *fiber.Ctx) error {
	var req struct {
		StratumPort int     `json:"stratum_port"`
		PoolWallet  string  `json:"pool_wallet"`
		PoolName    string  `json:"pool_name"`
		PoolFee     float64 `json:"pool_fee"`
		SoloFee     float64 `json:"solo_fee"`
		MinPayout   float64 `json:"min_payout"`
	}

	if err := c.BodyParser(&req); err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "Invalid request body"})
	}

	cfg := &config.PoolConfig{
		StratumPort: req.StratumPort,
		PoolWallet:  req.PoolWallet,
		PoolName:    req.PoolName,
		PoolFee:     req.PoolFee,
		SoloFee:     req.SoloFee,
		MinPayout:   req.MinPayout,
		UpdatedAt:   time.Now(),
	}

	// Validate config
	if err := config.ValidateConfig(cfg); err != nil {
		return c.Status(400).JSON(fiber.Map{"error": err.Error()})
	}

	// Save config
	if err := config.SaveConfig(dataDir, cfg); err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "Failed to save config: " + err.Error()})
	}

	return c.JSON(fiber.Map{
		"success": true,
		"message": "Configuration saved. Restart stratum service for port changes to take effect.",
	})
}
