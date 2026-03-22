package stratum

import (
	"encoding/json"
	"net"
	"sync"
	"time"
)

const (
	MethodSubscribe     = "mining.subscribe"
	MethodAuthorize     = "mining.authorize"
	MethodSubmit        = "mining.submit"
	MethodSetDifficulty = "mining.set_difficulty"
	MethodNotify        = "mining.notify"
)

type Request struct {
	ID     interface{}     `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type Response struct {
	ID     interface{} `json:"id"`
	Result interface{} `json:"result"`
	Error  *Error      `json:"error"`
}

// MarshalJSON ensures both result and error fields are always present per stratum spec
func (r *Response) MarshalJSON() ([]byte, error) {
	type Alias Response
	return json.Marshal(&struct {
		ID     interface{} `json:"id"`
		Result interface{} `json:"result"`
		Error  interface{} `json:"error"`
	}{
		ID:     r.ID,
		Result: r.Result,
		Error:  r.Error, // Will be null if nil, or the error array if set
	})
}

type Notification struct {
	ID     interface{}   `json:"id"`     // Must be null for notifications per stratum spec
	Method string        `json:"method"`
	Params []interface{} `json:"params"`
}

// Error represents a stratum error as array [code, message, null]
type Error struct {
	Code    int
	Message string
}

// MarshalJSON serializes Error as [code, message, null] per stratum spec
func (e *Error) MarshalJSON() ([]byte, error) {
	return json.Marshal([]interface{}{e.Code, e.Message, nil})
}

var (
	ErrUnknown        = &Error{20, "Unknown error"}
	ErrJobNotFound    = &Error{21, "Job not found"}
	ErrDuplicateShare = &Error{22, "Duplicate share"}
	ErrLowDifficulty  = &Error{23, "Low difficulty share"}
	ErrUnauthorized   = &Error{24, "Unauthorized worker"}
	ErrNotSubscribed  = &Error{25, "Not subscribed"}
)

// RentalService identifies the rental platform a miner is using
type RentalService int

const (
	RentalNone RentalService = iota
	RentalNiceHash
	RentalMRR // Mining Rig Rentals
	RentalOther
)

func (r RentalService) String() string {
	switch r {
	case RentalNiceHash:
		return "NiceHash"
	case RentalMRR:
		return "MRR"
	case RentalOther:
		return "Other"
	default:
		return "None"
	}
}

type Client struct {
	ID                  string
	Conn                net.Conn
	IP                  string
	Subscribed          bool
	SubscriptionID      string
	ExtraNonce1         string
	ExtraNonce2Size     int
	Authorized          bool
	MinerID             string
	WorkerName          string
	Difficulty          float64
	PreviousDifficulty  float64
	DifficultyChangedAt     time.Time
	DifficultyReducedAt     time.Time // When difficulty was reduced due to high rejection rate
	DifficultyReducedFrom   float64   // The difficulty level that caused high rejections
	LastDifficultySentAt    time.Time // When difficulty notification was last sent (to avoid duplicates)
	LastShareTime           time.Time
	ShareCount          int64
	ConnectedAt         time.Time
	LastActivity        time.Time
	ValidShares         int64
	InvalidShares       int64
	StaleShares         int64
	ShareTimes          []time.Time
	RecentSubmissions   []bool // true=accepted, false=rejected for last N submissions
	SoloMining          bool
	ManualDiff          float64
	LastSettingsRefresh time.Time // When settings were last refreshed from API

	// Rental service detection
	RentalService         RentalService
	UserAgent             string
	SupportsExtranonce    bool  // Client subscribed to extranonce updates
	SupportsVersionRolling bool // Client supports version rolling (AsicBoost)
	VersionRollingMask    string

	mu                  sync.RWMutex
}

type Job struct {
	ID               string
	Height           int64
	PrevBlockHash    string
	OriginalPrevHash string   // Original prevhash for block building
	CoinBase1        string
	CoinBase2        string
	MerkleBranches   []string
	Version          string
	NBits            string
	NTime            string
	CleanJobs        bool
	Target           string
	CreatedAt        time.Time
	Transactions     []string // Raw transaction hex for block building
}

type Share struct {
	ID          int64
	JobID       string
	MinerID     string
	WorkerName  string
	Difficulty  float64 // Target difficulty (for hashrate calculation)
	ActualDiff  float64 // Actual share difficulty (for block candidate detection)
	NetworkDiff float64
	IP          string
	ExtraNonce2 string
	ExtraNonce1 string
	VersionBits string
	NTime       string
	Nonce       string
	IsValid     bool
	IsBlock     bool
	IsSolo      bool
	BlockHash   string
	SubmittedAt time.Time
}

type Block struct {
	ID            int64
	Hash          string
	Height        int64
	MinerID       string
	WorkerName    string
	Reward        float64
	Difficulty    float64
	Status        string
	Confirmations int
	FoundAt       time.Time
	ConfirmedAt   *time.Time
}

const (
	MethodConfigure = "mining.configure"
)
