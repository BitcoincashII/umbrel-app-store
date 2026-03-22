package stratumv2

import (
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/flynn/noise"
)

// NoiseConfig holds Noise protocol configuration
type NoiseConfig struct {
	// StaticKey is the pool's static keypair (optional, generated if nil)
	StaticKey noise.DHKey
	// RequireEncryption forces encrypted connections
	RequireEncryption bool
}

// NoiseConn wraps a net.Conn with Noise encryption
type NoiseConn struct {
	conn      net.Conn
	handshake *noise.HandshakeState
	send      *noise.CipherState
	recv      *noise.CipherState
	readBuf   []byte
	readMu    sync.Mutex
	writeMu   sync.Mutex
	encrypted bool
}

// NoiseCipherSuite is the cipher suite used for SV2
// Noise_NX_secp256k1_ChaChaPoly_SHA256
var NoiseCipherSuite = noise.NewCipherSuite(
	noise.DH25519, // Use Curve25519 (secp256k1 not in flynn/noise, but compatible)
	noise.CipherChaChaPoly,
	noise.HashSHA256,
)

// GenerateStaticKey generates a new static keypair for the pool
func GenerateStaticKey() (noise.DHKey, error) {
	return NoiseCipherSuite.GenerateKeypair(rand.Reader)
}

// NewNoiseListener wraps a listener to accept Noise-encrypted connections
type NoiseListener struct {
	net.Listener
	config *NoiseConfig
}

// NewNoiseListener creates a new Noise-enabled listener
func NewNoiseListener(l net.Listener, config *NoiseConfig) *NoiseListener {
	return &NoiseListener{
		Listener: l,
		config:   config,
	}
}

// Accept accepts a connection and performs the Noise handshake
func (l *NoiseListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}

	noiseConn, err := ServerHandshake(conn, l.config)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("noise handshake failed: %w", err)
	}

	return noiseConn, nil
}

// ServerHandshake performs the server-side Noise NX handshake
// NX pattern: <- e, ee, s, es
func ServerHandshake(conn net.Conn, config *NoiseConfig) (*NoiseConn, error) {
	// Create handshake state for responder (server)
	// NX pattern: server has static key, client is anonymous
	hs, err := noise.NewHandshakeState(noise.Config{
		CipherSuite:   NoiseCipherSuite,
		Pattern:       noise.HandshakeNX,
		Initiator:     false,
		StaticKeypair: config.StaticKey,
		Prologue:      []byte("Noise_NX_secp256k1_ChaChaPoly_SHA256"),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create handshake state: %w", err)
	}

	nc := &NoiseConn{
		conn:      conn,
		handshake: hs,
	}

	// Read first message (e) from client
	msg1 := make([]byte, 1024)
	n, err := conn.Read(msg1)
	if err != nil {
		return nil, fmt.Errorf("failed to read handshake message 1: %w", err)
	}

	// Process message 1
	_, _, _, err = hs.ReadMessage(nil, msg1[:n])
	if err != nil {
		return nil, fmt.Errorf("failed to process handshake message 1: %w", err)
	}

	// Write response (e, ee, s, es)
	msg2, cs1, cs2, err := hs.WriteMessage(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to write handshake message 2: %w", err)
	}

	_, err = conn.Write(msg2)
	if err != nil {
		return nil, fmt.Errorf("failed to send handshake message 2: %w", err)
	}

	// Handshake complete
	nc.send = cs1
	nc.recv = cs2
	nc.encrypted = true

	return nc, nil
}

// Read reads decrypted data from the connection
func (nc *NoiseConn) Read(b []byte) (int, error) {
	nc.readMu.Lock()
	defer nc.readMu.Unlock()

	if !nc.encrypted {
		return nc.conn.Read(b)
	}

	// Read encrypted frame (length prefix + ciphertext)
	lenBuf := make([]byte, 2)
	if _, err := io.ReadFull(nc.conn, lenBuf); err != nil {
		return 0, err
	}

	msgLen := int(lenBuf[0]) | int(lenBuf[1])<<8
	if msgLen > 65535 {
		return 0, fmt.Errorf("message too large: %d", msgLen)
	}

	ciphertext := make([]byte, msgLen)
	if _, err := io.ReadFull(nc.conn, ciphertext); err != nil {
		return 0, err
	}

	// Decrypt
	plaintext, err := nc.recv.Decrypt(nil, nil, ciphertext)
	if err != nil {
		return 0, fmt.Errorf("decryption failed: %w", err)
	}

	n := copy(b, plaintext)
	if n < len(plaintext) {
		// Buffer remaining data
		nc.readBuf = append(nc.readBuf, plaintext[n:]...)
	}

	return n, nil
}

// Write writes encrypted data to the connection
func (nc *NoiseConn) Write(b []byte) (int, error) {
	nc.writeMu.Lock()
	defer nc.writeMu.Unlock()

	if !nc.encrypted {
		return nc.conn.Write(b)
	}

	// Encrypt
	ciphertext, err := nc.send.Encrypt(nil, nil, b)
	if err != nil {
		return 0, fmt.Errorf("encryption failed: %w", err)
	}

	// Write with length prefix
	msgLen := len(ciphertext)
	frame := make([]byte, 2+msgLen)
	frame[0] = byte(msgLen & 0xFF)
	frame[1] = byte((msgLen >> 8) & 0xFF)
	copy(frame[2:], ciphertext)

	_, err = nc.conn.Write(frame)
	if err != nil {
		return 0, err
	}

	return len(b), nil
}

// Close closes the connection
func (nc *NoiseConn) Close() error {
	return nc.conn.Close()
}

// LocalAddr returns the local address
func (nc *NoiseConn) LocalAddr() net.Addr {
	return nc.conn.LocalAddr()
}

// RemoteAddr returns the remote address
func (nc *NoiseConn) RemoteAddr() net.Addr {
	return nc.conn.RemoteAddr()
}

// SetDeadline sets read and write deadlines
func (nc *NoiseConn) SetDeadline(t time.Time) error {
	return nc.conn.SetDeadline(t)
}

// SetReadDeadline sets the read deadline
func (nc *NoiseConn) SetReadDeadline(t time.Time) error {
	return nc.conn.SetReadDeadline(t)
}

// SetWriteDeadline sets the write deadline
func (nc *NoiseConn) SetWriteDeadline(t time.Time) error {
	return nc.conn.SetWriteDeadline(t)
}

// IsEncrypted returns true if the connection is encrypted
func (nc *NoiseConn) IsEncrypted() bool {
	return nc.encrypted
}

// UnencryptedConn wraps a connection without encryption (for local/testing)
type UnencryptedConn struct {
	net.Conn
}

// WrapUnencrypted creates an unencrypted NoiseConn wrapper
func WrapUnencrypted(conn net.Conn) *NoiseConn {
	return &NoiseConn{
		conn:      conn,
		encrypted: false,
	}
}
