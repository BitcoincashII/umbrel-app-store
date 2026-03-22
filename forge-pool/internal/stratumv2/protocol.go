// Package stratumv2 implements the Stratum V2 mining protocol
// Reference: https://github.com/stratum-mining/sv2-spec
package stratumv2

// Protocol constants
const (
	// Extension types
	ExtTypeStandard uint16 = 0x0000

	// Channel bits in message type
	ChannelBitNone    uint8 = 0x00
	ChannelBitChannel uint8 = 0x80

	// Message types - Setup Connection (0x00-0x02)
	MsgTypeSetupConnection        uint8 = 0x00
	MsgTypeSetupConnectionSuccess uint8 = 0x01
	MsgTypeSetupConnectionError   uint8 = 0x02

	// Message types - Mining Protocol (0x10-0x1f)
	MsgTypeOpenStandardMiningChannel        uint8 = 0x10
	MsgTypeOpenStandardMiningChannelSuccess uint8 = 0x11
	MsgTypeOpenStandardMiningChannelError   uint8 = 0x12
	MsgTypeOpenExtendedMiningChannel        uint8 = 0x13
	MsgTypeOpenExtendedMiningChannelSuccess uint8 = 0x14
	MsgTypeOpenExtendedMiningChannelError   uint8 = 0x15
	MsgTypeUpdateChannel                    uint8 = 0x16
	MsgTypeUpdateChannelError               uint8 = 0x17
	MsgTypeCloseChannel                     uint8 = 0x18
	MsgTypeSetExtranoncePrefix              uint8 = 0x19
	MsgTypeSubmitSharesStandard             uint8 = 0x1a
	MsgTypeSubmitSharesExtended             uint8 = 0x1b
	MsgTypeSubmitSharesSuccess              uint8 = 0x1c
	MsgTypeSubmitSharesError                uint8 = 0x1d
	MsgTypeNewMiningJob                     uint8 = 0x1e
	MsgTypeNewExtendedMiningJob             uint8 = 0x1f
	MsgTypeSetNewPrevHash                   uint8 = 0x20
	MsgTypeSetTarget                        uint8 = 0x21
	MsgTypeReconnect                        uint8 = 0x25

	// Protocol version
	ProtocolVersionMin uint16 = 2
	ProtocolVersionMax uint16 = 2

	// Setup connection flags
	SetupFlagRequiresStandardJobs uint32 = 0x01
	SetupFlagRequiresWorkSelection uint32 = 0x02
	SetupFlagRequiresVersionRolling uint32 = 0x04
)

// SetupConnection is sent by client to initiate connection
type SetupConnection struct {
	Protocol        uint8    // 0 = Mining Protocol
	MinVersion      uint16   // Minimum supported version
	MaxVersion      uint16   // Maximum supported version
	Flags           uint32   // Feature flags
	Endpoint        string   // Host:port of pool (STR0_255)
	Vendor          string   // Mining software vendor (STR0_255)
	HardwareVersion string   // Hardware version (STR0_255)
	FirmwareVersion string   // Firmware version (STR0_255)
	DeviceID        string   // Unique device ID (STR0_255)
}

// SetupConnectionSuccess confirms connection setup
type SetupConnectionSuccess struct {
	UsedVersion uint16 // Selected protocol version
	Flags       uint32 // Accepted feature flags
}

// SetupConnectionError rejects connection setup
type SetupConnectionError struct {
	Flags uint32 // Flags that caused error
	Code  string // Error code (STR0_255)
}

// OpenStandardMiningChannel requests a mining channel
type OpenStandardMiningChannel struct {
	RequestID            uint32  // Client-assigned request ID
	User                 string  // Mining user identity (STR0_255)
	NominalHashrate      float32 // Expected hashrate in h/s
	MaxTargetNBits       uint32  // Maximum target difficulty
}

// OpenStandardMiningChannelSuccess confirms channel open
type OpenStandardMiningChannelSuccess struct {
	RequestID         uint32   // Echoed request ID
	ChannelID         uint32   // Server-assigned channel ID
	TargetNBits       uint32   // Initial target (nBits format)
	ExtranoncePrefix  []byte   // Pool-assigned extranonce prefix
	GroupChannelID    uint32   // Group channel ID (0 if none)
}

// OpenStandardMiningChannelError rejects channel open
type OpenStandardMiningChannelError struct {
	RequestID uint32 // Echoed request ID
	Code      string // Error code (STR0_255)
}

// NewMiningJob distributes a new mining job
type NewMiningJob struct {
	ChannelID      uint32 // Target channel
	JobID          uint32 // Unique job ID
	MinNTime       uint32 // Minimum nTime allowed
	Version        uint32 // Block version
	MerkleRoot     [32]byte // Merkle root of transactions
}

// NewExtendedMiningJob for extended channels (includes coinbase)
type NewExtendedMiningJob struct {
	ChannelID           uint32   // Target channel
	JobID               uint32   // Unique job ID
	MinNTime            uint32   // Minimum nTime allowed
	Version             uint32   // Block version
	VersionRollingMask  uint32   // Bits that can be rolled
	MerklePath          [][]byte // Merkle path for coinbase
	CoinbaseTxPrefix    []byte   // Coinbase before extranonce
	CoinbaseTxSuffix    []byte   // Coinbase after extranonce
}

// SetNewPrevHash updates the previous block hash
type SetNewPrevHash struct {
	ChannelID   uint32   // Target channel (0 for all)
	JobID       uint32   // Job ID this applies to
	PrevHash    [32]byte // New previous block hash
	MinNTime    uint32   // Minimum nTime
	NBits       uint32   // Current difficulty target
}

// SetTarget updates the share difficulty target
type SetTarget struct {
	ChannelID uint32 // Target channel
	MaxTarget [32]byte // Maximum target (lower = harder)
}

// SubmitSharesStandard submits shares for standard channels
type SubmitSharesStandard struct {
	ChannelID  uint32 // Channel ID
	SequenceNo uint32 // Submission sequence number
	JobID      uint32 // Job ID
	Nonce      uint32 // Nonce found
	NTime      uint32 // nTime used
	Version    uint32 // Version used (if rolling)
}

// SubmitSharesExtended submits shares for extended channels
type SubmitSharesExtended struct {
	ChannelID    uint32 // Channel ID
	SequenceNo   uint32 // Submission sequence number
	JobID        uint32 // Job ID
	Nonce        uint32 // Nonce found
	NTime        uint32 // nTime used
	Version      uint32 // Version used
	Extranonce   []byte // Full extranonce
}

// SubmitSharesSuccess confirms share acceptance
type SubmitSharesSuccess struct {
	ChannelID       uint32 // Channel ID
	LastSequenceNo  uint32 // Last accepted sequence number
	NewSubmitsCount uint32 // Number of newly accepted shares
	NewSharesSum    uint64 // Sum of new share difficulties
}

// SubmitSharesError reports share rejection
type SubmitSharesError struct {
	ChannelID  uint32 // Channel ID
	SequenceNo uint32 // Rejected sequence number
	Code       string // Error code (STR0_255)
}

// Reconnect instructs client to connect elsewhere
type Reconnect struct {
	NewPoolHost string // New pool hostname (STR0_255)
	NewPoolPort uint16 // New pool port
}
