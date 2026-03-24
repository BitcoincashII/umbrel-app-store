package mining

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"
)

type BlockTemplate struct {
	Version           int64    `json:"version"`
	PreviousBlockHash string   `json:"previousblockhash"`
	Transactions      []TxData `json:"transactions"`
	CoinbaseValue     int64    `json:"coinbasevalue"`
	Target            string   `json:"target"`
	Bits              string   `json:"bits"`
	Height            int64    `json:"height"`
	CurTime           int64    `json:"curtime"`
}

type TxData struct {
	Data string `json:"data"`
	TxID string `json:"txid"`
}

type JobManager struct {
	rpcURL          string
	rpcUser         string
	rpcPassword     string
	pubkeyHash      []byte
	jobCounter      uint64
	extranonce1Size int
	extranonce2Size int
}

func NewJobManager(rpcURL, rpcUser, rpcPassword, poolAddress string, extranonce1Size, extranonce2Size int) *JobManager {
	// Try to get pubkey hash from node's validateaddress RPC with retries
	var pkh []byte
	for i := 0; i < 10; i++ {
		pkh = getPubkeyHashFromNode(rpcURL, rpcUser, rpcPassword, poolAddress)
		if pkh != nil {
			break
		}
		if i < 9 {
			fmt.Printf("Waiting for node RPC to be ready... (attempt %d/10)\n", i+1)
			time.Sleep(2 * time.Second)
		}
	}
	if pkh == nil {
		// Fallback to local parsing if RPC fails
		pkh = parseAddressToPubkeyHash(poolAddress)
		if pkh != nil {
			fmt.Printf("Pool address pubkey hash (from local parser): %s\n", hex.EncodeToString(pkh))
		}
	}
	if pkh == nil {
		// No fallback - pool address is required
		panic("FATAL: Could not parse pool address. Set a valid BCH2 address in Settings.")
	}

	// Use defaults if not specified
	if extranonce1Size <= 0 {
		extranonce1Size = 4
	}
	if extranonce2Size <= 0 {
		extranonce2Size = 4
	}

	return &JobManager{
		rpcURL:          rpcURL,
		rpcUser:         rpcUser,
		rpcPassword:     rpcPassword,
		pubkeyHash:      pkh,
		extranonce1Size: extranonce1Size,
		extranonce2Size: extranonce2Size,
	}
}

// getPubkeyHashFromNode extracts pubkey hash via node RPC validateaddress
func getPubkeyHashFromNode(rpcURL, rpcUser, rpcPassword, address string) []byte {
	reqBody := fmt.Sprintf(`{"jsonrpc":"1.0","id":"pkh","method":"validateaddress","params":["%s"]}`, address)

	req, err := http.NewRequest("POST", rpcURL, bytes.NewBufferString(reqBody))
	if err != nil {
		return nil
	}
	req.SetBasicAuth(rpcUser, rpcPassword)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil
	}

	var rpcResp struct {
		Result struct {
			IsValid      bool   `json:"isvalid"`
			ScriptPubKey string `json:"scriptPubKey"`
		} `json:"result"`
	}

	if err := json.Unmarshal(body, &rpcResp); err != nil {
		return nil
	}

	if !rpcResp.Result.IsValid {
		return nil
	}

	// Extract pubkey hash from P2PKH scriptPubKey: 76a914<20-byte-hash>88ac
	spk := rpcResp.Result.ScriptPubKey
	if len(spk) == 50 && spk[:6] == "76a914" && spk[46:] == "88ac" {
		pkh, err := hex.DecodeString(spk[6:46])
		if err == nil && len(pkh) == 20 {
			fmt.Printf("Pool address pubkey hash (from node): %s\n", spk[6:46])
			return pkh
		}
	}

	return nil
}

// parseAddressToPubkeyHash extracts the 20-byte pubkey hash from a BCH2 address
func parseAddressToPubkeyHash(address string) []byte {
	// Remove prefix - support all valid BCH/BCH2 prefixes
	addr := address
	prefixes := []string{"bitcoincashii:", "bitcoincash:", "bchtest:"}
	for _, prefix := range prefixes {
		if len(address) > len(prefix) && address[:len(prefix)] == prefix {
			addr = address[len(prefix):]
			break
		}
	}
	// Decode cashaddr format (base32)
	// BCH/BCH2 uses cashaddr which is base32 with charset: qpzry9x8gf2tvdw0s3jn54khce6mua7l
	charset := "qpzry9x8gf2tvdw0s3jn54khce6mua7l"

	var data []byte
	for _, c := range addr {
		idx := -1
		for i, ch := range charset {
			if ch == c {
				idx = i
				break
			}
		}
		if idx == -1 {
			return nil
		}
		data = append(data, byte(idx))
	}

	if len(data) < 8 {
		return nil
	}

	// Remove checksum (last 8 characters = 40 bits)
	data = data[:len(data)-8]

	// Convert from 5-bit to 8-bit
	var result []byte
	acc := 0
	bits := 0
	for _, d := range data {
		acc = (acc << 5) | int(d)
		bits += 5
		for bits >= 8 {
			bits -= 8
			result = append(result, byte(acc>>bits))
			acc &= (1 << bits) - 1
		}
	}

	// First byte is version, rest is 20-byte hash
	if len(result) >= 21 {
		return result[1:21]
	}
	// Some CashAddr variants encode version in first char, resulting in exactly 20 bytes
	if len(result) == 20 {
		return result
	}
	return nil
}

func (jm *JobManager) GetBlockTemplate() (*BlockTemplate, error) {
	// BCH2 is pure Bitcoin Cash without SegWit - no rules needed
	reqBody := `{"jsonrpc":"1.0","id":"forge","method":"getblocktemplate","params":[{}]}`
	
	req, err := http.NewRequest("POST", jm.rpcURL, bytes.NewBufferString(reqBody))
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(jm.rpcUser, jm.rpcPassword)
	req.Header.Set("Content-Type", "application/json")
	
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	
	var rpcResp struct {
		Result BlockTemplate `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	
	if err := json.Unmarshal(body, &rpcResp); err != nil {
		return nil, err
	}
	
	if rpcResp.Error != nil {
		return nil, fmt.Errorf("rpc error: %s", rpcResp.Error.Message)
	}
	
	return &rpcResp.Result, nil
}

func (jm *JobManager) CreateJob(template *BlockTemplate) *Job {
	jobID := fmt.Sprintf("%x", atomic.AddUint64(&jm.jobCounter, 1))

	coinbase1, coinbase2 := jm.buildCoinbase(template)

	// Full byte reversal for stratum prevhash
	prevHash := stratumPrevHash(template.PreviousBlockHash)
	originalPrevHash := template.PreviousBlockHash

	// Extract txids and raw tx data for merkle branches and block building
	var txids []string
	var txData []string
	for _, tx := range template.Transactions {
		txids = append(txids, tx.TxID)
		txData = append(txData, tx.Data)
	}

	// Build merkle branches from transaction txids
	merkleBranches := buildMerkleBranches(txids)

	return &Job{
		ID:               jobID,
		Height:           template.Height,
		PrevBlockHash:    prevHash,
		CoinBase1:        coinbase1,
		CoinBase2:        coinbase2,
		MerkleBranches:   merkleBranches,
		Version:          fmt.Sprintf("%08x", template.Version),
		NBits:            template.Bits,
		NTime:            fmt.Sprintf("%08x", template.CurTime),
		CleanJobs:        true,
		Target:           template.Target,
		OriginalPrevHash: originalPrevHash,
		Transactions:     txData,
	}
}

func (jm *JobManager) buildCoinbase(template *BlockTemplate) (string, string) {
	heightBytes := makeHeightScript(template.Height)
	poolMsg := []byte("Forge")
	scriptLen := len(heightBytes) + jm.extranonce1Size + jm.extranonce2Size + len(poolMsg)
	
	var cb1 bytes.Buffer
	binary.Write(&cb1, binary.LittleEndian, uint32(1))
	cb1.WriteByte(0x01)
	cb1.Write(make([]byte, 32))
	binary.Write(&cb1, binary.LittleEndian, uint32(0xffffffff))
	cb1.WriteByte(byte(scriptLen))
	cb1.Write(heightBytes)
	
	var cb2 bytes.Buffer
	cb2.Write(poolMsg)
	binary.Write(&cb2, binary.LittleEndian, uint32(0xffffffff))
	cb2.WriteByte(0x01)
	binary.Write(&cb2, binary.LittleEndian, uint64(template.CoinbaseValue))
	cb2.WriteByte(0x19)
	cb2.WriteByte(0x76)
	cb2.WriteByte(0xa9)
	cb2.WriteByte(0x14)
	cb2.Write(jm.pubkeyHash)
	cb2.WriteByte(0x88)
	cb2.WriteByte(0xac)
	binary.Write(&cb2, binary.LittleEndian, uint32(0))
	
	return hex.EncodeToString(cb1.Bytes()), hex.EncodeToString(cb2.Bytes())
}

func makeHeightScript(height int64) []byte {
	if height <= 0 {
		return []byte{0x01, 0x00}
	}
	var heightBytes []byte
	h := height
	for h > 0 {
		heightBytes = append(heightBytes, byte(h&0xff))
		h >>= 8
	}
	if len(heightBytes) > 0 && heightBytes[len(heightBytes)-1] >= 0x80 {
		heightBytes = append(heightBytes, 0x00)
	}
	return append([]byte{byte(len(heightBytes))}, heightBytes...)
}

// stratumPrevHash converts getblocktemplate previousblockhash to stratum format
func stratumPrevHash(gbtHash string) string {
	b, _ := hex.DecodeString(gbtHash)
	if len(b) != 32 {
		return gbtHash
	}
	// First fully reverse (big-endian to little-endian)
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
	// Then 4-byte swap (so miners swap gets back to little-endian)
	for i := 0; i < 32; i += 4 {
		b[i], b[i+1], b[i+2], b[i+3] = b[i+3], b[i+2], b[i+1], b[i]
	}
	return hex.EncodeToString(b)
}

type Job struct {
	ID               string
	Height           int64
	PrevBlockHash    string
	OriginalPrevHash string
	CoinBase1        string
	CoinBase2        string
	MerkleBranches   []string
	Version          string
	NBits            string
	NTime            string
	CleanJobs        bool
	Target           string
	Transactions     []string // Raw transaction hex data for block building
}

// doubleSHA256 computes double SHA256 hash
func doubleSHA256(data []byte) []byte {
	first := sha256.Sum256(data)
	second := sha256.Sum256(first[:])
	return second[:]
}

// buildMerkleBranches calculates the merkle branches for stratum
// These are the sibling hashes needed to compute merkle root from coinbase
func buildMerkleBranches(txids []string) []string {
	if len(txids) == 0 {
		return []string{}
	}

	// Convert txids to bytes (they come as big-endian hex, need little-endian for merkle)
	var hashes [][]byte
	for _, txid := range txids {
		h, err := hex.DecodeString(txid)
		if err != nil {
			continue
		}
		// Reverse to little-endian for merkle calculation
		for i, j := 0, len(h)-1; i < j; i, j = i+1, j-1 {
			h[i], h[j] = h[j], h[i]
		}
		hashes = append(hashes, h)
	}

	if len(hashes) == 0 {
		return []string{}
	}

	// Build merkle branches - collect the sibling at each level
	// At each level, first hash is our sibling (branch), then we compute
	// the subtree hash from remaining hashes for the next level
	var branches []string

	for len(hashes) > 0 {
		// First hash at current level is our branch (sibling to coinbase path)
		branches = append(branches, hex.EncodeToString(hashes[0]))

		// Remove the branch hash and compute next level from remaining
		hashes = hashes[1:]
		if len(hashes) == 0 {
			break
		}

		// Compute next level from remaining hashes
		var nextLevel [][]byte
		for i := 0; i < len(hashes); i += 2 {
			var combined []byte
			if i+1 < len(hashes) {
				combined = append(hashes[i], hashes[i+1]...)
			} else {
				// Odd number - duplicate last hash
				combined = append(hashes[i], hashes[i]...)
			}
			nextLevel = append(nextLevel, doubleSHA256(combined))
		}
		hashes = nextLevel
	}

	return branches
}
