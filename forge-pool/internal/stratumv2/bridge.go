package stratumv2

import (
	"context"
	"encoding/hex"
	"strconv"
	"time"
)

// V1ShareProcessor adapts V2 shares to the V1 share processor interface
type V1ShareProcessor struct {
	ProcessShareFunc func(ctx context.Context, share *V1Share) error
	ProcessBlockFunc func(ctx context.Context, block *V1Block) error
}

// V1Share represents the V1 stratum share format
type V1Share struct {
	JobID       string
	MinerID     string
	WorkerName  string
	Difficulty  float64
	ActualDiff  float64
	NetworkDiff float64
	IP          string
	ExtraNonce1 string
	ExtraNonce2 string
	VersionBits string
	NTime       string
	Nonce       string
	IsValid     bool
	IsBlock     bool
	IsSolo      bool
	BlockHash   string
	SubmittedAt time.Time
}

// V1Block represents the V1 block format
type V1Block struct {
	Hash       string
	Height     int64
	MinerID    string
	WorkerName string
	Reward     float64
	Difficulty float64
	FoundAt    time.Time
}

// ProcessShare converts V2 share to V1 format and processes it
func (p *V1ShareProcessor) ProcessShare(ctx context.Context, share *Share) error {
	if p.ProcessShareFunc == nil {
		return nil
	}

	// Convert V2 share to V1 format
	v1Share := &V1Share{
		JobID:       strconv.FormatUint(uint64(share.JobID), 10),
		MinerID:     share.MinerID,
		WorkerName:  share.WorkerName,
		Difficulty:  share.Difficulty,
		ActualDiff:  share.ActualDiff,
		IP:          share.IP,
		ExtraNonce1: hex.EncodeToString(share.Extranonce[:4]),
		ExtraNonce2: hex.EncodeToString(share.Extranonce[4:]),
		NTime:       formatHex32(share.NTime),
		Nonce:       formatHex32(share.Nonce),
		VersionBits: formatHex32(share.Version),
		IsValid:     share.IsValid,
		IsBlock:     share.IsBlock,
		IsSolo:      share.IsSolo,
		BlockHash:   share.BlockHash,
		SubmittedAt: share.SubmittedAt,
	}

	return p.ProcessShareFunc(ctx, v1Share)
}

// ProcessBlock converts V2 block to V1 format and processes it
func (p *V1ShareProcessor) ProcessBlock(ctx context.Context, block *Block) error {
	if p.ProcessBlockFunc == nil {
		return nil
	}

	v1Block := &V1Block{
		Hash:       block.Hash,
		Height:     block.Height,
		MinerID:    block.MinerID,
		WorkerName: block.WorkerName,
		Reward:     block.Reward,
		Difficulty: block.Difficulty,
		FoundAt:    block.FoundAt,
	}

	return p.ProcessBlockFunc(ctx, v1Block)
}

// V1MinerSettings adapts the V1 miner settings interface
type V1MinerSettings struct {
	GetSettingsFunc func(minerID string) (*MinerSettings, error)
}

// GetMinerSettings retrieves miner settings
func (s *V1MinerSettings) GetMinerSettings(minerID string) (*MinerSettings, error) {
	if s.GetSettingsFunc == nil {
		return nil, nil
	}
	return s.GetSettingsFunc(minerID)
}

// ConvertV1JobData converts V1 job data to V2 MiningJobState
// This is used to bridge V1 job generation to V2 format
type V1JobData struct {
	ID               string
	Height           int64
	PrevBlockHash    string   // Hex string
	OriginalPrevHash string   // Hex string (non-stratum-reversed)
	CoinBase1        string   // Hex string
	CoinBase2        string   // Hex string
	MerkleBranches   []string // Hex strings
	Version          string   // Hex string
	NBits            string   // Hex string
	NTime            string   // Hex string
	Target           string   // Hex string
	CleanJobs        bool
	CreatedAt        time.Time
	Transactions     []string // Raw transaction hex strings
}

// ConvertV1ToV2Job converts V1 job format to V2 MiningJobState
func ConvertV1ToV2Job(v1 *V1JobData, jobID uint32) (*MiningJobState, error) {
	job := &MiningJobState{
		JobID:     jobID,
		Height:    v1.Height,
		CleanJobs: v1.CleanJobs,
		CreatedAt: v1.CreatedAt,
	}

	// Parse version
	if v, err := strconv.ParseUint(v1.Version, 16, 32); err == nil {
		job.Version = uint32(v)
	}

	// Parse nBits
	if bits, err := strconv.ParseUint(v1.NBits, 16, 32); err == nil {
		job.NBits = uint32(bits)
	}

	// Parse nTime
	if ntime, err := strconv.ParseUint(v1.NTime, 16, 32); err == nil {
		job.MinNTime = uint32(ntime)
	}

	// Parse previous hash - convert from stratum format (4-byte reversed chunks) to raw
	if prevHashBytes, err := hex.DecodeString(v1.OriginalPrevHash); err == nil && len(prevHashBytes) == 32 {
		copy(job.PrevHash[:], prevHashBytes)
	}

	// Parse coinbase parts
	if cb1, err := hex.DecodeString(v1.CoinBase1); err == nil {
		job.CoinbaseTxPrefix = cb1
	}
	if cb2, err := hex.DecodeString(v1.CoinBase2); err == nil {
		job.CoinbaseTxSuffix = cb2
	}

	// Parse merkle branches
	job.MerklePath = make([][]byte, len(v1.MerkleBranches))
	for i, branchHex := range v1.MerkleBranches {
		if branch, err := hex.DecodeString(branchHex); err == nil {
			job.MerklePath[i] = branch
		}
	}

	// For standard channels, calculate the merkle root from an empty extranonce
	// In practice, each channel gets its own merkle root based on its extranonce
	// For now, we'll compute it when sending to specific channels

	// Parse transactions
	job.Transactions = make([][]byte, len(v1.Transactions))
	for i, txHex := range v1.Transactions {
		if tx, err := hex.DecodeString(txHex); err == nil {
			job.Transactions[i] = tx
		}
	}

	return job, nil
}

// formatHex32 formats a uint32 as an 8-character hex string
func formatHex32(v uint32) string {
	return hex.EncodeToString([]byte{
		byte(v >> 24),
		byte(v >> 16),
		byte(v >> 8),
		byte(v),
	})
}
