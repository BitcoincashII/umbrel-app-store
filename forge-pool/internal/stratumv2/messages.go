package stratumv2

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math/big"
	"time"
)

// Response message senders

func (s *Server) sendSetupConnectionSuccess(client *Client, version uint16, flags uint32) error {
	e := NewEncoder()
	e.WriteU16(version)
	e.WriteU32(flags)

	return s.sendFrame(client, MsgTypeSetupConnectionSuccess, e.Bytes())
}

func (s *Server) sendSetupConnectionError(client *Client, flags uint32, code string) error {
	e := NewEncoder()
	e.WriteU32(flags)
	e.WriteSTR0_255(code)

	return s.sendFrame(client, MsgTypeSetupConnectionError, e.Bytes())
}

func (s *Server) sendOpenStandardMiningChannelSuccess(client *Client, requestID, channelID, targetNBits uint32, extranoncePrefix []byte, groupChannelID uint32) error {
	e := NewEncoder()
	e.WriteU32(requestID)
	e.WriteU32(channelID)
	e.WriteU32(targetNBits)
	e.WriteB0_255(extranoncePrefix)
	e.WriteU32(groupChannelID)

	return s.sendFrame(client, MsgTypeOpenStandardMiningChannelSuccess, e.Bytes())
}

func (s *Server) sendOpenStandardMiningChannelError(client *Client, requestID uint32, code string) error {
	e := NewEncoder()
	e.WriteU32(requestID)
	e.WriteSTR0_255(code)

	return s.sendFrame(client, MsgTypeOpenStandardMiningChannelError, e.Bytes())
}

func (s *Server) sendOpenExtendedMiningChannelSuccess(client *Client, requestID, channelID, targetNBits uint32, extranonceSuffixSize uint16, extranoncePrefix []byte, groupChannelID uint32) error {
	e := NewEncoder()
	e.WriteU32(requestID)
	e.WriteU32(channelID)
	e.WriteU32(targetNBits)
	e.WriteU16(extranonceSuffixSize)
	e.WriteB0_255(extranoncePrefix)
	e.WriteU32(groupChannelID)

	return s.sendFrame(client, MsgTypeOpenExtendedMiningChannelSuccess, e.Bytes())
}

func (s *Server) sendOpenExtendedMiningChannelError(client *Client, requestID uint32, code string) error {
	e := NewEncoder()
	e.WriteU32(requestID)
	e.WriteSTR0_255(code)

	return s.sendFrame(client, MsgTypeOpenExtendedMiningChannelError, e.Bytes())
}

func (s *Server) sendUpdateChannelError(client *Client, channelID uint32, code string) error {
	e := NewEncoder()
	e.WriteU32(channelID)
	e.WriteSTR0_255(code)

	return s.sendFrame(client, MsgTypeUpdateChannelError, e.Bytes())
}

func (s *Server) sendSubmitSharesSuccess(client *Client, channelID, lastSequenceNo, newSubmitsCount uint32, newSharesSum uint64) error {
	e := NewEncoder()
	e.WriteU32(channelID)
	e.WriteU32(lastSequenceNo)
	e.WriteU32(newSubmitsCount)
	e.WriteU64(newSharesSum)

	// Channel message - set high bit
	return s.sendFrame(client, MsgTypeSubmitSharesSuccess|ChannelBitChannel, e.Bytes())
}

func (s *Server) sendSubmitSharesError(client *Client, channelID, sequenceNo uint32, code string) error {
	e := NewEncoder()
	e.WriteU32(channelID)
	e.WriteU32(sequenceNo)
	e.WriteSTR0_255(code)

	return s.sendFrame(client, MsgTypeSubmitSharesError|ChannelBitChannel, e.Bytes())
}

func (s *Server) sendNewMiningJob(client *Client, channel *MiningChannel, job *MiningJobState) error {
	e := NewEncoder()
	e.WriteU32(channel.ID)
	e.WriteU32(job.JobID)
	e.WriteU32(job.MinNTime)
	e.WriteU32(job.Version)
	e.WriteBytes(job.MerkleRoot[:])

	// Channel message
	return s.sendFrame(client, MsgTypeNewMiningJob|ChannelBitChannel, e.Bytes())
}

func (s *Server) sendNewExtendedMiningJob(client *Client, channel *MiningChannel, job *MiningJobState) error {
	e := NewEncoder()
	e.WriteU32(channel.ID)
	e.WriteU32(job.JobID)
	e.WriteU32(job.MinNTime)
	e.WriteU32(job.Version)
	e.WriteU32(0xFFFFFFFF) // Version rolling mask (allow all bits)

	// Merkle path - sequence of 32-byte hashes
	e.WriteU8(uint8(len(job.MerklePath)))
	for _, hash := range job.MerklePath {
		e.WriteBytes(hash)
	}

	// Coinbase transaction parts
	e.WriteB0_255(job.CoinbaseTxPrefix)
	e.WriteB0_255(job.CoinbaseTxSuffix)

	// Channel message
	return s.sendFrame(client, MsgTypeNewExtendedMiningJob|ChannelBitChannel, e.Bytes())
}

func (s *Server) sendSetNewPrevHash(client *Client, job *MiningJobState) error {
	e := NewEncoder()
	e.WriteU32(0) // Channel ID 0 = all channels
	e.WriteU32(job.JobID)
	e.WriteBytes(job.PrevHash[:])
	e.WriteU32(job.MinNTime)
	e.WriteU32(job.NBits)

	// Channel message for prev hash
	return s.sendFrame(client, MsgTypeSetNewPrevHash|ChannelBitChannel, e.Bytes())
}

func (s *Server) sendSetTarget(client *Client, channelID uint32, target [32]byte) error {
	e := NewEncoder()
	e.WriteU32(channelID)
	e.WriteBytes(target[:])

	return s.sendFrame(client, MsgTypeSetTarget|ChannelBitChannel, e.Bytes())
}

func (s *Server) sendReconnect(client *Client, host string, port uint16) error {
	e := NewEncoder()
	e.WriteSTR0_255(host)
	e.WriteU16(port)

	return s.sendFrame(client, MsgTypeReconnect, e.Bytes())
}

func (s *Server) sendFrame(client *Client, msgType uint8, payload []byte) error {
	frame := &Frame{
		ExtensionType: ExtTypeStandard,
		MsgType:       msgType,
		Payload:       payload,
	}

	return WriteFrame(client.noiseConn, frame)
}

// Share validation

func (s *Server) validateShareStandard(channel *MiningChannel, job *MiningJobState, nonce, ntime, version uint32) (bool, float64, []byte, error) {
	// Build block header
	header := make([]byte, 80)

	// Version (4 bytes, little-endian)
	binary.LittleEndian.PutUint32(header[0:4], version)

	// Previous block hash (32 bytes)
	copy(header[4:36], job.PrevHash[:])

	// Merkle root (32 bytes)
	copy(header[36:68], job.MerkleRoot[:])

	// Time (4 bytes, little-endian)
	binary.LittleEndian.PutUint32(header[68:72], ntime)

	// Bits (4 bytes, little-endian)
	binary.LittleEndian.PutUint32(header[72:76], job.NBits)

	// Nonce (4 bytes, little-endian)
	binary.LittleEndian.PutUint32(header[76:80], nonce)

	// Double SHA256
	blockHash := doubleSHA256(header)

	// Calculate difficulty from hash
	actualDiff := hashToDifficulty(blockHash)

	// Check if meets target
	isValid := actualDiff >= channel.TargetDiff

	// Reverse hash for display (Bitcoin convention)
	displayHash := make([]byte, 32)
	copy(displayHash, blockHash)
	reverseBytes(displayHash)

	return isValid, actualDiff, displayHash, nil
}

func (s *Server) validateShareExtended(channel *MiningChannel, job *MiningJobState, nonce, ntime, version uint32, extranonce []byte) (bool, float64, []byte, error) {
	// Build coinbase transaction
	coinbase := make([]byte, 0, len(job.CoinbaseTxPrefix)+len(extranonce)+len(job.CoinbaseTxSuffix))
	coinbase = append(coinbase, job.CoinbaseTxPrefix...)
	coinbase = append(coinbase, extranonce...)
	coinbase = append(coinbase, job.CoinbaseTxSuffix...)

	// Calculate merkle root from coinbase and path
	merkleRoot := doubleSHA256(coinbase)
	for _, branch := range job.MerklePath {
		combined := make([]byte, 64)
		copy(combined[:32], merkleRoot)
		copy(combined[32:], branch)
		merkleRoot = doubleSHA256(combined)
	}

	// Build block header
	header := make([]byte, 80)
	binary.LittleEndian.PutUint32(header[0:4], version)
	copy(header[4:36], job.PrevHash[:])
	copy(header[36:68], merkleRoot)
	binary.LittleEndian.PutUint32(header[68:72], ntime)
	binary.LittleEndian.PutUint32(header[72:76], job.NBits)
	binary.LittleEndian.PutUint32(header[76:80], nonce)

	blockHash := doubleSHA256(header)
	actualDiff := hashToDifficulty(blockHash)
	isValid := actualDiff >= channel.TargetDiff

	displayHash := make([]byte, 32)
	copy(displayHash, blockHash)
	reverseBytes(displayHash)

	return isValid, actualDiff, displayHash, nil
}

// Utility functions

func doubleSHA256(data []byte) []byte {
	first := sha256.Sum256(data)
	second := sha256.Sum256(first[:])
	return second[:]
}

func reverseBytes(b []byte) {
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
}

func hashToDifficulty(hash []byte) float64 {
	// Difficulty 1 target
	diff1Target := new(big.Int)
	diff1Target.SetString("00000000FFFF0000000000000000000000000000000000000000000000000000", 16)

	// Hash is in internal byte order, reverse for big.Int
	hashReversed := make([]byte, 32)
	copy(hashReversed, hash)
	reverseBytes(hashReversed)

	hashInt := new(big.Int).SetBytes(hashReversed)
	if hashInt.Sign() == 0 {
		return 0
	}

	diff1Float := new(big.Float).SetInt(diff1Target)
	hashFloat := new(big.Float).SetInt(hashInt)
	result := new(big.Float).Quo(diff1Float, hashFloat)
	difficulty, _ := result.Float64()

	return difficulty
}

func difficultyToTarget(diff float64) [32]byte {
	var target [32]byte
	if diff <= 0 {
		return target
	}

	diff1Target := new(big.Int)
	diff1Target.SetString("00000000FFFF0000000000000000000000000000000000000000000000000000", 16)

	diff1Float := new(big.Float).SetInt(diff1Target)
	diffFloat := new(big.Float).SetFloat64(diff)
	targetFloat := new(big.Float).Quo(diff1Float, diffFloat)
	targetInt, _ := targetFloat.Int(nil)

	// Convert to 32-byte array
	targetBytes := targetInt.Bytes()
	if len(targetBytes) <= 32 {
		copy(target[32-len(targetBytes):], targetBytes)
	}

	return target
}

func difficultyToNBits(diff float64) uint32 {
	// Convert difficulty to compact nBits format
	target := difficultyToTarget(diff)

	// Find first non-zero byte
	var firstNonZero int
	for i := 0; i < 32; i++ {
		if target[i] != 0 {
			firstNonZero = i
			break
		}
	}

	// Extract mantissa (3 bytes)
	var mantissa uint32
	if firstNonZero < 30 {
		mantissa = uint32(target[firstNonZero])<<16 |
			uint32(target[firstNonZero+1])<<8 |
			uint32(target[firstNonZero+2])
	}

	// Calculate exponent
	exponent := uint32(32 - firstNonZero)

	// Handle mantissa overflow (if high bit set, shift right and increase exp)
	if mantissa&0x800000 != 0 {
		mantissa >>= 8
		exponent++
	}

	return (exponent << 24) | mantissa
}

func nbitsToDifficulty(nbits uint32) float64 {
	// Extract exponent and mantissa
	exponent := int(nbits >> 24)
	mantissa := nbits & 0x007FFFFF

	// Calculate target
	var target big.Int
	target.SetUint64(uint64(mantissa))
	target.Lsh(&target, uint(8*(exponent-3)))

	// Calculate difficulty
	diff1Target := new(big.Int)
	diff1Target.SetString("00000000FFFF0000000000000000000000000000000000000000000000000000", 16)

	if target.Sign() == 0 {
		return 0
	}

	diff1Float := new(big.Float).SetInt(diff1Target)
	targetFloat := new(big.Float).SetInt(&target)
	result := new(big.Float).Quo(diff1Float, targetFloat)
	difficulty, _ := result.Float64()

	return difficulty
}

// ConvertV1JobToV2 converts a V1 job format to V2 MiningJobState
func ConvertV1JobToV2(v1Job *V1Job) (*MiningJobState, error) {
	if v1Job == nil {
		return nil, fmt.Errorf("nil job")
	}

	job := &MiningJobState{
		JobID:     v1Job.ID,
		Height:    v1Job.Height,
		MinNTime:  v1Job.NTime,
		Version:   v1Job.Version,
		NBits:     v1Job.NBits,
		CleanJobs: v1Job.CleanJobs,
		CreatedAt: v1Job.CreatedAt,
	}

	// Copy prev hash
	copy(job.PrevHash[:], v1Job.PrevHash[:])

	// Copy merkle root for standard channels
	copy(job.MerkleRoot[:], v1Job.MerkleRoot[:])

	// For extended channels, copy merkle path
	job.MerklePath = make([][]byte, len(v1Job.MerklePath))
	for i, branch := range v1Job.MerklePath {
		job.MerklePath[i] = make([]byte, len(branch))
		copy(job.MerklePath[i], branch)
	}

	// Copy coinbase parts
	job.CoinbaseTxPrefix = make([]byte, len(v1Job.CoinbaseTxPrefix))
	copy(job.CoinbaseTxPrefix, v1Job.CoinbaseTxPrefix)

	job.CoinbaseTxSuffix = make([]byte, len(v1Job.CoinbaseTxSuffix))
	copy(job.CoinbaseTxSuffix, v1Job.CoinbaseTxSuffix)

	// Calculate target from difficulty
	job.Target = difficultyToTarget(v1Job.NetworkDiff)

	return job, nil
}

// V1Job represents job data from the V1 stratum server
type V1Job struct {
	ID               uint32
	Height           int64
	PrevHash         [32]byte
	MerkleRoot       [32]byte
	MerklePath       [][]byte
	CoinbaseTxPrefix []byte
	CoinbaseTxSuffix []byte
	Version          uint32
	NBits            uint32
	NTime            uint32
	NetworkDiff      float64
	CleanJobs        bool
	CreatedAt        time.Time
}
