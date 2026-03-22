package stratumv2

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
)

// Frame header size: 2 (ext type) + 1 (msg type) + 3 (length) = 6 bytes
const FrameHeaderSize = 6

// MaxFrameSize is the maximum allowed frame payload size (16MB)
const MaxFrameSize = 16 * 1024 * 1024

// Frame represents a Stratum V2 message frame
type Frame struct {
	ExtensionType uint16 // Extension type (0x0000 for standard)
	MsgType       uint8  // Message type (high bit = channel message)
	Payload       []byte // Message payload
}

// IsChannelMessage returns true if this is a channel-specific message
func (f *Frame) IsChannelMessage() bool {
	return (f.MsgType & ChannelBitChannel) != 0
}

// BaseMessageType returns the message type without the channel bit
func (f *Frame) BaseMessageType() uint8 {
	return f.MsgType & 0x7F
}

// EncodeFrame serializes a frame to wire format
// Format: [ext_type:2][msg_type:1][length:3][payload:N]
func EncodeFrame(f *Frame) ([]byte, error) {
	payloadLen := len(f.Payload)
	if payloadLen > MaxFrameSize {
		return nil, fmt.Errorf("payload too large: %d > %d", payloadLen, MaxFrameSize)
	}

	buf := make([]byte, FrameHeaderSize+payloadLen)

	// Extension type (2 bytes, little-endian)
	binary.LittleEndian.PutUint16(buf[0:2], f.ExtensionType)

	// Message type (1 byte)
	buf[2] = f.MsgType

	// Payload length (3 bytes, little-endian, 24-bit)
	buf[3] = byte(payloadLen & 0xFF)
	buf[4] = byte((payloadLen >> 8) & 0xFF)
	buf[5] = byte((payloadLen >> 16) & 0xFF)

	// Payload
	copy(buf[FrameHeaderSize:], f.Payload)

	return buf, nil
}

// DecodeFrameHeader reads and parses a frame header
// Returns extension type, message type, and payload length
func DecodeFrameHeader(r io.Reader) (extType uint16, msgType uint8, length uint32, err error) {
	header := make([]byte, FrameHeaderSize)
	if _, err = io.ReadFull(r, header); err != nil {
		return 0, 0, 0, fmt.Errorf("failed to read frame header: %w", err)
	}

	extType = binary.LittleEndian.Uint16(header[0:2])
	msgType = header[2]
	length = uint32(header[3]) | uint32(header[4])<<8 | uint32(header[5])<<16

	if length > MaxFrameSize {
		return 0, 0, 0, fmt.Errorf("frame too large: %d > %d", length, MaxFrameSize)
	}

	return extType, msgType, length, nil
}

// ReadFrame reads a complete frame from a reader
func ReadFrame(r io.Reader) (*Frame, error) {
	extType, msgType, length, err := DecodeFrameHeader(r)
	if err != nil {
		return nil, err
	}

	payload := make([]byte, length)
	if length > 0 {
		if _, err = io.ReadFull(r, payload); err != nil {
			return nil, fmt.Errorf("failed to read payload: %w", err)
		}
	}

	return &Frame{
		ExtensionType: extType,
		MsgType:       msgType,
		Payload:       payload,
	}, nil
}

// WriteFrame writes a frame to a writer
func WriteFrame(w io.Writer, f *Frame) error {
	data, err := EncodeFrame(f)
	if err != nil {
		return err
	}

	_, err = w.Write(data)
	return err
}

// Binary encoding helpers for SV2 data types

// EncodeU8 encodes a uint8
func EncodeU8(v uint8) []byte {
	return []byte{v}
}

// EncodeU16 encodes a uint16 (little-endian)
func EncodeU16(v uint16) []byte {
	buf := make([]byte, 2)
	binary.LittleEndian.PutUint16(buf, v)
	return buf
}

// EncodeU32 encodes a uint32 (little-endian)
func EncodeU32(v uint32) []byte {
	buf := make([]byte, 4)
	binary.LittleEndian.PutUint32(buf, v)
	return buf
}

// EncodeU64 encodes a uint64 (little-endian)
func EncodeU64(v uint64) []byte {
	buf := make([]byte, 8)
	binary.LittleEndian.PutUint64(buf, v)
	return buf
}

// EncodeF32 encodes a float32 (little-endian IEEE 754)
func EncodeF32(v float32) []byte {
	buf := make([]byte, 4)
	bits := math.Float32bits(v)
	binary.LittleEndian.PutUint32(buf, bits)
	return buf
}

// EncodeSTR0_255 encodes a string with 1-byte length prefix (max 255 chars)
func EncodeSTR0_255(s string) []byte {
	if len(s) > 255 {
		s = s[:255]
	}
	buf := make([]byte, 1+len(s))
	buf[0] = byte(len(s))
	copy(buf[1:], s)
	return buf
}

// EncodeBytes encodes a byte slice with length prefix
func EncodeB0_255(b []byte) []byte {
	if len(b) > 255 {
		b = b[:255]
	}
	buf := make([]byte, 1+len(b))
	buf[0] = byte(len(b))
	copy(buf[1:], b)
	return buf
}

// EncodeB0_64K encodes a byte slice with 2-byte length prefix
func EncodeB0_64K(b []byte) []byte {
	if len(b) > 65535 {
		b = b[:65535]
	}
	buf := make([]byte, 2+len(b))
	binary.LittleEndian.PutUint16(buf[0:2], uint16(len(b)))
	copy(buf[2:], b)
	return buf
}

// Decoder helps parse binary payloads
type Decoder struct {
	data []byte
	pos  int
}

// NewDecoder creates a new decoder for the given data
func NewDecoder(data []byte) *Decoder {
	return &Decoder{data: data, pos: 0}
}

// Remaining returns bytes remaining
func (d *Decoder) Remaining() int {
	return len(d.data) - d.pos
}

// ReadU8 reads a uint8
func (d *Decoder) ReadU8() (uint8, error) {
	if d.pos+1 > len(d.data) {
		return 0, io.ErrUnexpectedEOF
	}
	v := d.data[d.pos]
	d.pos++
	return v, nil
}

// ReadU16 reads a uint16 (little-endian)
func (d *Decoder) ReadU16() (uint16, error) {
	if d.pos+2 > len(d.data) {
		return 0, io.ErrUnexpectedEOF
	}
	v := binary.LittleEndian.Uint16(d.data[d.pos:])
	d.pos += 2
	return v, nil
}

// ReadU32 reads a uint32 (little-endian)
func (d *Decoder) ReadU32() (uint32, error) {
	if d.pos+4 > len(d.data) {
		return 0, io.ErrUnexpectedEOF
	}
	v := binary.LittleEndian.Uint32(d.data[d.pos:])
	d.pos += 4
	return v, nil
}

// ReadU64 reads a uint64 (little-endian)
func (d *Decoder) ReadU64() (uint64, error) {
	if d.pos+8 > len(d.data) {
		return 0, io.ErrUnexpectedEOF
	}
	v := binary.LittleEndian.Uint64(d.data[d.pos:])
	d.pos += 8
	return v, nil
}

// ReadF32 reads a float32 (little-endian IEEE 754)
func (d *Decoder) ReadF32() (float32, error) {
	if d.pos+4 > len(d.data) {
		return 0, io.ErrUnexpectedEOF
	}
	bits := binary.LittleEndian.Uint32(d.data[d.pos:])
	d.pos += 4
	return math.Float32frombits(bits), nil
}

// ReadSTR0_255 reads a string with 1-byte length prefix
func (d *Decoder) ReadSTR0_255() (string, error) {
	if d.pos+1 > len(d.data) {
		return "", io.ErrUnexpectedEOF
	}
	length := int(d.data[d.pos])
	d.pos++
	if d.pos+length > len(d.data) {
		return "", io.ErrUnexpectedEOF
	}
	s := string(d.data[d.pos : d.pos+length])
	d.pos += length
	return s, nil
}

// ReadB0_255 reads bytes with 1-byte length prefix
func (d *Decoder) ReadB0_255() ([]byte, error) {
	if d.pos+1 > len(d.data) {
		return nil, io.ErrUnexpectedEOF
	}
	length := int(d.data[d.pos])
	d.pos++
	if d.pos+length > len(d.data) {
		return nil, io.ErrUnexpectedEOF
	}
	b := make([]byte, length)
	copy(b, d.data[d.pos:d.pos+length])
	d.pos += length
	return b, nil
}

// ReadBytes reads a fixed number of bytes
func (d *Decoder) ReadBytes(n int) ([]byte, error) {
	if d.pos+n > len(d.data) {
		return nil, io.ErrUnexpectedEOF
	}
	b := make([]byte, n)
	copy(b, d.data[d.pos:d.pos+n])
	d.pos += n
	return b, nil
}

// Encoder helps build binary payloads
type Encoder struct {
	buf []byte
}

// NewEncoder creates a new encoder
func NewEncoder() *Encoder {
	return &Encoder{buf: make([]byte, 0, 256)}
}

// Bytes returns the encoded data
func (e *Encoder) Bytes() []byte {
	return e.buf
}

// WriteU8 writes a uint8
func (e *Encoder) WriteU8(v uint8) {
	e.buf = append(e.buf, v)
}

// WriteU16 writes a uint16 (little-endian)
func (e *Encoder) WriteU16(v uint16) {
	e.buf = append(e.buf, EncodeU16(v)...)
}

// WriteU32 writes a uint32 (little-endian)
func (e *Encoder) WriteU32(v uint32) {
	e.buf = append(e.buf, EncodeU32(v)...)
}

// WriteU64 writes a uint64 (little-endian)
func (e *Encoder) WriteU64(v uint64) {
	e.buf = append(e.buf, EncodeU64(v)...)
}

// WriteSTR0_255 writes a string with 1-byte length prefix
func (e *Encoder) WriteSTR0_255(s string) {
	e.buf = append(e.buf, EncodeSTR0_255(s)...)
}

// WriteB0_255 writes bytes with 1-byte length prefix
func (e *Encoder) WriteB0_255(b []byte) {
	e.buf = append(e.buf, EncodeB0_255(b)...)
}

// WriteBytes writes raw bytes
func (e *Encoder) WriteBytes(b []byte) {
	e.buf = append(e.buf, b...)
}
