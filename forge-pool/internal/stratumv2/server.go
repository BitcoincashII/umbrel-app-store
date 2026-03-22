package stratumv2

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/flynn/noise"
	"go.uber.org/zap"
)

// ServerConfig holds V2 server configuration
type ServerConfig struct {
	Host               string
	Port               int
	MaxConnections     int
	MinDiff            float64
	MaxDiff            float64
	TargetShareTime    int  // Seconds between shares
	RetargetTime       int  // Seconds between vardiff adjustments
	RequireEncryption  bool // Require Noise encryption
	ExtranonceSize     int  // Size of extranonce prefix (default 8)
}

// ShareProcessor processes accepted shares
type ShareProcessor interface {
	ProcessShare(ctx context.Context, share *Share) error
	ProcessBlock(ctx context.Context, block *Block) error
}

// MinerSettingsStore retrieves miner settings
type MinerSettingsStore interface {
	GetMinerSettings(minerID string) (*MinerSettings, error)
}

// MinerSettings holds per-miner configuration
type MinerSettings struct {
	MinerID    string
	SoloMining bool
	ManualDiff float64
	MinPayout  float64
}

// Share represents a submitted share
type Share struct {
	JobID       uint32
	MinerID     string
	WorkerName  string
	Difficulty  float64
	ActualDiff  float64
	IP          string
	Extranonce  []byte
	NTime       uint32
	Nonce       uint32
	Version     uint32
	IsValid     bool
	IsBlock     bool
	IsSolo      bool
	BlockHash   string
	SubmittedAt time.Time
}

// Block represents a found block
type Block struct {
	Hash       string
	Height     int64
	MinerID    string
	WorkerName string
	Reward     float64
	Difficulty float64
	FoundAt    time.Time
}

// Server is the Stratum V2 mining server
type Server struct {
	config         *ServerConfig
	logger         *zap.Logger
	listener       net.Listener
	noiseKey       noise.DHKey
	clients        sync.Map // map[string]*Client
	clientCount    int64
	channels       sync.Map // map[uint32]*MiningChannel
	nextChannelID  uint32
	currentJob     atomic.Value // *MiningJobState
	jobHistory     sync.Map     // map[uint32]*MiningJobState
	nextJobID      uint32
	extraNonce     uint64
	extraNonceMu   sync.Mutex
	shareProcessor ShareProcessor
	minerSettings  MinerSettingsStore
	shutdownCh     chan struct{}
	stats          *ServerStats

	// Duplicate share detection
	submittedShares sync.Map // map[shareKey]time.Time
}

// ServerStats holds server statistics
type ServerStats struct {
	TotalConnections  int64
	ActiveConnections int64
	TotalChannels     int64
	ActiveChannels    int64
	TotalShares       int64
	ValidShares       int64
	InvalidShares     int64
	BlocksFound       int64
}

// MiningJobState holds job state for validation
type MiningJobState struct {
	JobID           uint32
	Height          int64
	PrevHash        [32]byte
	Version         uint32
	NBits           uint32
	MinNTime        uint32
	MerkleRoot      [32]byte // For standard channels
	MerklePath      [][]byte // For extended channels
	CoinbaseTxPrefix []byte
	CoinbaseTxSuffix []byte
	Target          [32]byte
	CleanJobs       bool
	CreatedAt       time.Time
	Transactions    [][]byte // Raw transactions for block building
}

// MiningChannel represents an open mining channel
type MiningChannel struct {
	ID               uint32
	ClientID         string
	MinerID          string
	WorkerName       string
	TargetDiff       float64
	ExtranoncePrefix []byte
	ExtranonceSuffix []byte // For extended channels
	IsExtended       bool
	GroupChannelID   uint32
	NominalHashrate  float32
	CreatedAt        time.Time
	LastShareAt      time.Time
	ShareCount       int64
	ValidShares      int64
	InvalidShares    int64
	SoloMining       bool
}

// Client represents a connected V2 client
type Client struct {
	ID              string
	conn            net.Conn
	noiseConn       *NoiseConn
	IP              string
	SetupComplete   bool
	UsedVersion     uint16
	Flags           uint32
	Vendor          string
	HardwareVersion string
	FirmwareVersion string
	DeviceID        string
	Channels        map[uint32]*MiningChannel
	ConnectedAt     time.Time
	LastActivity    time.Time
	mu              sync.RWMutex
}

// NewServer creates a new Stratum V2 server
func NewServer(config *ServerConfig, logger *zap.Logger, sp ShareProcessor, ms MinerSettingsStore) (*Server, error) {
	// Set defaults
	if config.ExtranonceSize == 0 {
		config.ExtranonceSize = 8 // 8 bytes for V2 (Braiins compatible)
	}
	if config.MinDiff == 0 {
		config.MinDiff = 32768
	}
	if config.TargetShareTime == 0 {
		config.TargetShareTime = 10
	}
	if config.RetargetTime == 0 {
		config.RetargetTime = 60
	}

	// Generate static key for Noise protocol
	key, err := GenerateStaticKey()
	if err != nil {
		return nil, fmt.Errorf("failed to generate noise key: %w", err)
	}

	s := &Server{
		config:         config,
		logger:         logger,
		noiseKey:       key,
		shareProcessor: sp,
		minerSettings:  ms,
		shutdownCh:     make(chan struct{}),
		stats:          &ServerStats{},
		nextChannelID:  1,
		nextJobID:      1,
	}

	logger.Info("Stratum V2 server initialized",
		zap.String("public_key", hex.EncodeToString(key.Public)),
		zap.Int("extranonce_size", config.ExtranonceSize))

	return s, nil
}

// Start begins accepting connections
func (s *Server) Start() error {
	addr := fmt.Sprintf("%s:%d", s.config.Host, s.config.Port)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("failed to listen: %w", err)
	}
	s.listener = listener
	s.logger.Info("Stratum V2 server started",
		zap.String("addr", addr),
		zap.Bool("encryption_required", s.config.RequireEncryption))

	go s.acceptLoop()
	go s.shareCleanupLoop()

	return nil
}

// Stop gracefully shuts down the server
func (s *Server) Stop() {
	s.logger.Info("Initiating graceful shutdown...")
	close(s.shutdownCh)

	if s.listener != nil {
		s.listener.Close()
	}

	// Wait for clients to disconnect
	deadline := time.Now().Add(30 * time.Second)
	for atomic.LoadInt64(&s.clientCount) > 0 {
		if time.Now().After(deadline) {
			s.logger.Warn("Shutdown timeout, forcing disconnect",
				zap.Int64("remaining_clients", atomic.LoadInt64(&s.clientCount)))
			s.clients.Range(func(key, value interface{}) bool {
				client := value.(*Client)
				client.noiseConn.Close()
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
			select {
			case <-s.shutdownCh:
				return
			default:
				s.logger.Debug("Accept error", zap.Error(err))
				continue
			}
		}

		if atomic.LoadInt64(&s.clientCount) >= int64(s.config.MaxConnections) {
			conn.Close()
			continue
		}

		go s.handleConnection(conn)
	}
}

func (s *Server) handleConnection(conn net.Conn) {
	// Configure TCP
	if tc, ok := conn.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
		tc.SetKeepAlive(true)
		tc.SetKeepAlivePeriod(30 * time.Second)
	}

	atomic.AddInt64(&s.clientCount, 1)
	atomic.AddInt64(&s.stats.ActiveConnections, 1)
	defer func() {
		atomic.AddInt64(&s.clientCount, -1)
		atomic.AddInt64(&s.stats.ActiveConnections, -1)
		conn.Close()
	}()

	// Perform Noise handshake
	noiseConfig := &NoiseConfig{
		StaticKey:         s.noiseKey,
		RequireEncryption: s.config.RequireEncryption,
	}

	var noiseConn *NoiseConn
	var err error

	if s.config.RequireEncryption {
		noiseConn, err = ServerHandshake(conn, noiseConfig)
		if err != nil {
			s.logger.Debug("Noise handshake failed",
				zap.String("ip", conn.RemoteAddr().String()),
				zap.Error(err))
			return
		}
	} else {
		// Allow unencrypted for testing
		noiseConn = WrapUnencrypted(conn)
	}

	client := &Client{
		ID:          fmt.Sprintf("v2_%d", time.Now().UnixNano()),
		conn:        conn,
		noiseConn:   noiseConn,
		IP:          conn.RemoteAddr().String(),
		Channels:    make(map[uint32]*MiningChannel),
		ConnectedAt: time.Now(),
	}

	s.clients.Store(client.ID, client)
	defer func() {
		s.clients.Delete(client.ID)
		// Close all channels
		client.mu.Lock()
		for chID := range client.Channels {
			s.channels.Delete(chID)
			atomic.AddInt64(&s.stats.ActiveChannels, -1)
		}
		client.mu.Unlock()
	}()

	s.logger.Info("V2 client connected",
		zap.String("ip", client.IP),
		zap.Bool("encrypted", noiseConn.IsEncrypted()))

	// Handle messages
	s.handleClient(client)

	// Log disconnection
	client.mu.RLock()
	vendor := client.Vendor
	channelCount := len(client.Channels)
	client.mu.RUnlock()

	s.logger.Info("V2 client disconnected",
		zap.String("ip", client.IP),
		zap.String("vendor", vendor),
		zap.Int("channels", channelCount),
		zap.Duration("connected", time.Since(client.ConnectedAt)))
}

func (s *Server) handleClient(client *Client) {
	for {
		select {
		case <-s.shutdownCh:
			return
		default:
		}

		// Read frame
		frame, err := ReadFrame(client.noiseConn)
		if err != nil {
			s.logger.Debug("Failed to read frame",
				zap.String("ip", client.IP),
				zap.Error(err))
			return
		}

		client.mu.Lock()
		client.LastActivity = time.Now()
		client.mu.Unlock()

		// Handle message based on type
		if err := s.handleMessage(client, frame); err != nil {
			s.logger.Warn("Failed to handle message",
				zap.String("ip", client.IP),
				zap.Uint8("msg_type", frame.MsgType),
				zap.Error(err))
		}
	}
}

func (s *Server) handleMessage(client *Client, frame *Frame) error {
	// Check if this is a channel message (high bit set)
	isChannelMsg := frame.IsChannelMessage()

	switch frame.BaseMessageType() {
	case MsgTypeSetupConnection:
		return s.handleSetupConnection(client, frame)

	case MsgTypeOpenStandardMiningChannel:
		return s.handleOpenStandardMiningChannel(client, frame)

	case MsgTypeOpenExtendedMiningChannel:
		return s.handleOpenExtendedMiningChannel(client, frame)

	case MsgTypeSubmitSharesStandard:
		if !isChannelMsg {
			return fmt.Errorf("submit shares must be channel message")
		}
		return s.handleSubmitSharesStandard(client, frame)

	case MsgTypeSubmitSharesExtended:
		if !isChannelMsg {
			return fmt.Errorf("submit shares must be channel message")
		}
		return s.handleSubmitSharesExtended(client, frame)

	case MsgTypeUpdateChannel:
		return s.handleUpdateChannel(client, frame)

	case MsgTypeCloseChannel:
		return s.handleCloseChannel(client, frame)

	default:
		s.logger.Debug("Unknown message type",
			zap.Uint8("msg_type", frame.MsgType),
			zap.String("ip", client.IP))
		return nil // Ignore unknown messages
	}
}

func (s *Server) handleSetupConnection(client *Client, frame *Frame) error {
	d := NewDecoder(frame.Payload)

	// Parse SetupConnection message
	protocol, err := d.ReadU8()
	if err != nil {
		return fmt.Errorf("failed to read protocol: %w", err)
	}
	if protocol != 0 {
		return s.sendSetupConnectionError(client, 0, "unsupported-protocol")
	}

	minVersion, _ := d.ReadU16()
	maxVersion, _ := d.ReadU16()
	flags, _ := d.ReadU32()
	endpoint, _ := d.ReadSTR0_255()
	vendor, _ := d.ReadSTR0_255()
	hwVersion, _ := d.ReadSTR0_255()
	fwVersion, _ := d.ReadSTR0_255()
	deviceID, _ := d.ReadSTR0_255()

	// Version negotiation
	if maxVersion < ProtocolVersionMin || minVersion > ProtocolVersionMax {
		return s.sendSetupConnectionError(client, flags, "unsupported-protocol-version")
	}

	usedVersion := ProtocolVersionMax
	if maxVersion < usedVersion {
		usedVersion = maxVersion
	}

	// We support standard jobs and version rolling
	acceptedFlags := flags & (SetupFlagRequiresStandardJobs | SetupFlagRequiresVersionRolling)

	client.mu.Lock()
	client.SetupComplete = true
	client.UsedVersion = usedVersion
	client.Flags = acceptedFlags
	client.Vendor = vendor
	client.HardwareVersion = hwVersion
	client.FirmwareVersion = fwVersion
	client.DeviceID = deviceID
	client.mu.Unlock()

	s.logger.Info("V2 setup complete",
		zap.String("ip", client.IP),
		zap.String("vendor", vendor),
		zap.String("endpoint", endpoint),
		zap.Uint16("version", usedVersion),
		zap.Uint32("flags", acceptedFlags))

	// Send success response
	return s.sendSetupConnectionSuccess(client, usedVersion, acceptedFlags)
}

func (s *Server) handleOpenStandardMiningChannel(client *Client, frame *Frame) error {
	client.mu.RLock()
	if !client.SetupComplete {
		client.mu.RUnlock()
		return fmt.Errorf("setup not complete")
	}
	client.mu.RUnlock()

	d := NewDecoder(frame.Payload)

	requestID, _ := d.ReadU32()
	user, _ := d.ReadSTR0_255()
	nominalHashrate, _ := d.ReadF32()
	maxTargetNBits, _ := d.ReadU32()

	// Parse user string (minerID.workerName format)
	minerID, workerName := parseUsername(user)
	if minerID == "" {
		return s.sendOpenStandardMiningChannelError(client, requestID, "invalid-user")
	}

	// Get miner settings
	var soloMining bool
	var manualDiff float64
	if s.minerSettings != nil {
		if settings, err := s.minerSettings.GetMinerSettings(minerID); err == nil && settings != nil {
			soloMining = settings.SoloMining
			manualDiff = settings.ManualDiff
		}
	}

	// Allocate channel
	channelID := atomic.AddUint32(&s.nextChannelID, 1)

	// Generate extranonce prefix
	s.extraNonceMu.Lock()
	s.extraNonce++
	extranoncePrefix := make([]byte, s.config.ExtranonceSize)
	// Use top 4 bytes for pool prefix, bottom 4 for channel
	extranoncePrefix[0] = byte(s.extraNonce >> 24)
	extranoncePrefix[1] = byte(s.extraNonce >> 16)
	extranoncePrefix[2] = byte(s.extraNonce >> 8)
	extranoncePrefix[3] = byte(s.extraNonce)
	extranoncePrefix[4] = byte(channelID >> 24)
	extranoncePrefix[5] = byte(channelID >> 16)
	extranoncePrefix[6] = byte(channelID >> 8)
	extranoncePrefix[7] = byte(channelID)
	s.extraNonceMu.Unlock()

	// Calculate initial difficulty
	initialDiff := s.config.MinDiff
	if manualDiff > 0 {
		initialDiff = manualDiff
	}

	// Convert difficulty to nBits format
	targetNBits := difficultyToNBits(initialDiff)
	if maxTargetNBits != 0 && maxTargetNBits < targetNBits {
		targetNBits = maxTargetNBits
	}

	channel := &MiningChannel{
		ID:               channelID,
		ClientID:         client.ID,
		MinerID:          minerID,
		WorkerName:       workerName,
		TargetDiff:       initialDiff,
		ExtranoncePrefix: extranoncePrefix,
		IsExtended:       false,
		NominalHashrate:  nominalHashrate,
		CreatedAt:        time.Now(),
		SoloMining:       soloMining,
	}

	// Store channel
	s.channels.Store(channelID, channel)
	atomic.AddInt64(&s.stats.ActiveChannels, 1)
	atomic.AddInt64(&s.stats.TotalChannels, 1)

	client.mu.Lock()
	client.Channels[channelID] = channel
	client.mu.Unlock()

	s.logger.Info("Standard mining channel opened",
		zap.String("miner", minerID),
		zap.String("worker", workerName),
		zap.Uint32("channel_id", channelID),
		zap.Float64("initial_diff", initialDiff),
		zap.Bool("solo", soloMining))

	// Send success response
	if err := s.sendOpenStandardMiningChannelSuccess(client, requestID, channelID, targetNBits, extranoncePrefix, 0); err != nil {
		return err
	}

	// Send initial job if available
	if job := s.currentJob.Load(); job != nil {
		jobState := job.(*MiningJobState)
		return s.sendNewMiningJob(client, channel, jobState)
	}

	return nil
}

func (s *Server) handleOpenExtendedMiningChannel(client *Client, frame *Frame) error {
	client.mu.RLock()
	if !client.SetupComplete {
		client.mu.RUnlock()
		return fmt.Errorf("setup not complete")
	}
	client.mu.RUnlock()

	d := NewDecoder(frame.Payload)

	requestID, _ := d.ReadU32()
	user, _ := d.ReadSTR0_255()
	nominalHashrate, _ := d.ReadF32()
	maxTargetNBits, _ := d.ReadU32()
	minExtranonceSize, _ := d.ReadU16()

	minerID, workerName := parseUsername(user)
	if minerID == "" {
		return s.sendOpenExtendedMiningChannelError(client, requestID, "invalid-user")
	}

	// Ensure we can provide requested extranonce size
	if int(minExtranonceSize) > s.config.ExtranonceSize {
		return s.sendOpenExtendedMiningChannelError(client, requestID, "extranonce-too-small")
	}

	var soloMining bool
	var manualDiff float64
	if s.minerSettings != nil {
		if settings, err := s.minerSettings.GetMinerSettings(minerID); err == nil && settings != nil {
			soloMining = settings.SoloMining
			manualDiff = settings.ManualDiff
		}
	}

	channelID := atomic.AddUint32(&s.nextChannelID, 1)

	// For extended channels, prefix is pool portion, miner can fill the rest
	s.extraNonceMu.Lock()
	s.extraNonce++
	prefixSize := s.config.ExtranonceSize / 2 // Split between pool and miner
	extranoncePrefix := make([]byte, prefixSize)
	extranoncePrefix[0] = byte(s.extraNonce >> 24)
	extranoncePrefix[1] = byte(s.extraNonce >> 16)
	extranoncePrefix[2] = byte(s.extraNonce >> 8)
	extranoncePrefix[3] = byte(s.extraNonce)
	s.extraNonceMu.Unlock()

	initialDiff := s.config.MinDiff
	if manualDiff > 0 {
		initialDiff = manualDiff
	}

	targetNBits := difficultyToNBits(initialDiff)
	if maxTargetNBits != 0 && maxTargetNBits < targetNBits {
		targetNBits = maxTargetNBits
	}

	channel := &MiningChannel{
		ID:               channelID,
		ClientID:         client.ID,
		MinerID:          minerID,
		WorkerName:       workerName,
		TargetDiff:       initialDiff,
		ExtranoncePrefix: extranoncePrefix,
		IsExtended:       true,
		NominalHashrate:  nominalHashrate,
		CreatedAt:        time.Now(),
		SoloMining:       soloMining,
	}

	s.channels.Store(channelID, channel)
	atomic.AddInt64(&s.stats.ActiveChannels, 1)
	atomic.AddInt64(&s.stats.TotalChannels, 1)

	client.mu.Lock()
	client.Channels[channelID] = channel
	client.mu.Unlock()

	s.logger.Info("Extended mining channel opened",
		zap.String("miner", minerID),
		zap.String("worker", workerName),
		zap.Uint32("channel_id", channelID),
		zap.Float64("initial_diff", initialDiff),
		zap.Int("extranonce_prefix_size", len(extranoncePrefix)))

	// Send success
	extranonceSuffixSize := uint16(s.config.ExtranonceSize - len(extranoncePrefix))
	if err := s.sendOpenExtendedMiningChannelSuccess(client, requestID, channelID, targetNBits, extranonceSuffixSize, extranoncePrefix, 0); err != nil {
		return err
	}

	// Send initial job
	if job := s.currentJob.Load(); job != nil {
		jobState := job.(*MiningJobState)
		return s.sendNewExtendedMiningJob(client, channel, jobState)
	}

	return nil
}

func (s *Server) handleSubmitSharesStandard(client *Client, frame *Frame) error {
	d := NewDecoder(frame.Payload)

	channelID, _ := d.ReadU32()
	sequenceNo, _ := d.ReadU32()
	jobID, _ := d.ReadU32()
	nonce, _ := d.ReadU32()
	ntime, _ := d.ReadU32()
	version, _ := d.ReadU32()

	// Look up channel
	chInterface, exists := s.channels.Load(channelID)
	if !exists {
		return s.sendSubmitSharesError(client, channelID, sequenceNo, "channel-not-found")
	}
	channel := chInterface.(*MiningChannel)

	// Look up job
	jobInterface, exists := s.jobHistory.Load(jobID)
	if !exists {
		return s.sendSubmitSharesError(client, channelID, sequenceNo, "job-not-found")
	}
	job := jobInterface.(*MiningJobState)

	// Build and validate share
	isValid, actualDiff, blockHash, err := s.validateShareStandard(channel, job, nonce, ntime, version)
	if err != nil {
		atomic.AddInt64(&s.stats.InvalidShares, 1)
		return s.sendSubmitSharesError(client, channelID, sequenceNo, "invalid-share")
	}

	if !isValid {
		atomic.AddInt64(&s.stats.InvalidShares, 1)
		atomic.AddInt64(&channel.InvalidShares, 1)
		return s.sendSubmitSharesError(client, channelID, sequenceNo, "low-difficulty")
	}

	// Share accepted
	atomic.AddInt64(&s.stats.ValidShares, 1)
	atomic.AddInt64(&channel.ValidShares, 1)
	channel.LastShareAt = time.Now()

	share := &Share{
		JobID:       jobID,
		MinerID:     channel.MinerID,
		WorkerName:  channel.WorkerName,
		Difficulty:  channel.TargetDiff,
		ActualDiff:  actualDiff,
		IP:          client.IP,
		Extranonce:  channel.ExtranoncePrefix,
		NTime:       ntime,
		Nonce:       nonce,
		Version:     version,
		IsValid:     true,
		IsSolo:      channel.SoloMining,
		BlockHash:   hex.EncodeToString(blockHash),
		SubmittedAt: time.Now(),
	}

	if s.shareProcessor != nil {
		go s.shareProcessor.ProcessShare(context.Background(), share)
	}

	s.logger.Info("V2 share accepted",
		zap.String("miner", channel.MinerID),
		zap.Uint32("channel", channelID),
		zap.Float64("diff", actualDiff),
		zap.Uint32("job", jobID))

	return s.sendSubmitSharesSuccess(client, channelID, sequenceNo, 1, uint64(actualDiff))
}

func (s *Server) handleSubmitSharesExtended(client *Client, frame *Frame) error {
	d := NewDecoder(frame.Payload)

	channelID, _ := d.ReadU32()
	sequenceNo, _ := d.ReadU32()
	jobID, _ := d.ReadU32()
	nonce, _ := d.ReadU32()
	ntime, _ := d.ReadU32()
	version, _ := d.ReadU32()
	extranonce, _ := d.ReadB0_255()

	chInterface, exists := s.channels.Load(channelID)
	if !exists {
		return s.sendSubmitSharesError(client, channelID, sequenceNo, "channel-not-found")
	}
	channel := chInterface.(*MiningChannel)

	jobInterface, exists := s.jobHistory.Load(jobID)
	if !exists {
		return s.sendSubmitSharesError(client, channelID, sequenceNo, "job-not-found")
	}
	job := jobInterface.(*MiningJobState)

	// Validate extended share with full extranonce
	fullExtranonce := append(channel.ExtranoncePrefix, extranonce...)
	isValid, actualDiff, blockHash, err := s.validateShareExtended(channel, job, nonce, ntime, version, fullExtranonce)
	if err != nil {
		atomic.AddInt64(&s.stats.InvalidShares, 1)
		return s.sendSubmitSharesError(client, channelID, sequenceNo, "invalid-share")
	}

	if !isValid {
		atomic.AddInt64(&s.stats.InvalidShares, 1)
		atomic.AddInt64(&channel.InvalidShares, 1)
		return s.sendSubmitSharesError(client, channelID, sequenceNo, "low-difficulty")
	}

	atomic.AddInt64(&s.stats.ValidShares, 1)
	atomic.AddInt64(&channel.ValidShares, 1)
	channel.LastShareAt = time.Now()

	share := &Share{
		JobID:       jobID,
		MinerID:     channel.MinerID,
		WorkerName:  channel.WorkerName,
		Difficulty:  channel.TargetDiff,
		ActualDiff:  actualDiff,
		IP:          client.IP,
		Extranonce:  fullExtranonce,
		NTime:       ntime,
		Nonce:       nonce,
		Version:     version,
		IsValid:     true,
		IsSolo:      channel.SoloMining,
		BlockHash:   hex.EncodeToString(blockHash),
		SubmittedAt: time.Now(),
	}

	if s.shareProcessor != nil {
		go s.shareProcessor.ProcessShare(context.Background(), share)
	}

	s.logger.Info("V2 extended share accepted",
		zap.String("miner", channel.MinerID),
		zap.Uint32("channel", channelID),
		zap.Float64("diff", actualDiff))

	return s.sendSubmitSharesSuccess(client, channelID, sequenceNo, 1, uint64(actualDiff))
}

func (s *Server) handleUpdateChannel(client *Client, frame *Frame) error {
	d := NewDecoder(frame.Payload)

	channelID, _ := d.ReadU32()
	nominalHashrate, _ := d.ReadF32()
	maxTargetNBits, _ := d.ReadU32()

	chInterface, exists := s.channels.Load(channelID)
	if !exists {
		return s.sendUpdateChannelError(client, channelID, "channel-not-found")
	}
	channel := chInterface.(*MiningChannel)

	// Update channel parameters
	channel.NominalHashrate = nominalHashrate
	if maxTargetNBits != 0 {
		newDiff := nbitsToDifficulty(maxTargetNBits)
		if newDiff > s.config.MinDiff {
			channel.TargetDiff = newDiff
		}
	}

	s.logger.Info("Channel updated",
		zap.Uint32("channel_id", channelID),
		zap.Float32("hashrate", nominalHashrate))

	return nil
}

func (s *Server) handleCloseChannel(client *Client, frame *Frame) error {
	d := NewDecoder(frame.Payload)

	channelID, _ := d.ReadU32()

	s.channels.Delete(channelID)
	atomic.AddInt64(&s.stats.ActiveChannels, -1)

	client.mu.Lock()
	delete(client.Channels, channelID)
	client.mu.Unlock()

	s.logger.Info("Channel closed", zap.Uint32("channel_id", channelID))

	return nil
}

func (s *Server) shareCleanupLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-s.shutdownCh:
			return
		case <-ticker.C:
			cutoff := time.Now().Add(-5 * time.Minute)
			s.submittedShares.Range(func(key, value interface{}) bool {
				if t, ok := value.(time.Time); ok && t.Before(cutoff) {
					s.submittedShares.Delete(key)
				}
				return true
			})
		}
	}
}

// BroadcastJob sends a new job to all connected mining channels
func (s *Server) BroadcastJob(job *MiningJobState) {
	s.currentJob.Store(job)
	s.jobHistory.Store(job.JobID, job)

	s.clients.Range(func(key, value interface{}) bool {
		client := value.(*Client)
		client.mu.RLock()
		channels := make([]*MiningChannel, 0, len(client.Channels))
		for _, ch := range client.Channels {
			channels = append(channels, ch)
		}
		client.mu.RUnlock()

		for _, channel := range channels {
			var err error
			if channel.IsExtended {
				err = s.sendNewExtendedMiningJob(client, channel, job)
			} else {
				err = s.sendNewMiningJob(client, channel, job)
			}
			if err != nil {
				s.logger.Warn("Failed to send job",
					zap.Uint32("channel_id", channel.ID),
					zap.Error(err))
			}
		}

		// Send SetNewPrevHash if clean job
		if job.CleanJobs {
			s.sendSetNewPrevHash(client, job)
		}

		return true
	})

	s.logger.Debug("Job broadcast complete",
		zap.Uint32("job_id", job.JobID),
		zap.Bool("clean", job.CleanJobs))
}

// GetStats returns current server statistics
func (s *Server) GetStats() *ServerStats {
	return &ServerStats{
		ActiveConnections: atomic.LoadInt64(&s.stats.ActiveConnections),
		ActiveChannels:    atomic.LoadInt64(&s.stats.ActiveChannels),
		TotalChannels:     atomic.LoadInt64(&s.stats.TotalChannels),
		ValidShares:       atomic.LoadInt64(&s.stats.ValidShares),
		InvalidShares:     atomic.LoadInt64(&s.stats.InvalidShares),
		BlocksFound:       atomic.LoadInt64(&s.stats.BlocksFound),
	}
}

// parseUsername splits "minerID.workerName" format
func parseUsername(username string) (minerID, workerName string) {
	for i, c := range username {
		if c == '.' {
			return username[:i], username[i+1:]
		}
	}
	return username, "default"
}

// generateRequestID generates a random request ID
func generateRequestID() []byte {
	id := make([]byte, 4)
	rand.Read(id)
	return id
}
