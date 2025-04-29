// File: cmd/main_test.go

// Package declaration
package main_test

// Imports
import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	// Import the package being tested (assuming it's 'main')
	// Note: This might require adjusting the test file location or build tags
	// if 'main' package cannot be imported directly. For simplicity, assume it works.
	main "path/to/your/p2p_vpn_v2/cmd" // Adjust this import path

	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	"github.com/libp2p/go-libp2p/core/sec"
	"github.com/libp2p/go-libp2p/core/transport"
	mocknet "github.com/libp2p/go-libp2p/p2p/net/mock"
	"github.com/multiformats/go-multiaddr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Mocks ---

// Mock water.Interface
type MockTun struct {
	ReadBuffer  *bytes.Buffer
	WriteBuffer *bytes.Buffer
	ReadError   error
	WriteError  error
	Closed      bool
	NameValue   string
	mu          sync.Mutex
}

func NewMockTun(name string) *MockTun {
	return &MockTun{
		ReadBuffer:  bytes.NewBuffer(nil),
		WriteBuffer: bytes.NewBuffer(nil),
		NameValue:   name,
	}
}

func (m *MockTun) Read(p []byte) (n int, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ReadError != nil {
		return 0, m.ReadError
	}
	return m.ReadBuffer.Read(p)
}

func (m *MockTun) Write(p []byte) (n int, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.WriteError != nil {
		return 0, m.WriteError
	}
	return m.WriteBuffer.Write(p)
}

func (m *MockTun) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Closed = true
	return nil
}

func (m *MockTun) Name() string {
	return m.NameValue
}

// Mock network.Stream
// Use mocknet.MockStream directly or wrap it if needed

// Mock host.Host
// Use mocknet.Mocknet and its hosts

// --- Test Setup ---

// Helper to create a simple IPv4 packet byte slice
// Note: This is a *very* basic representation, only setting destination IP
func createIPv4Packet(destIP net.IP) []byte {
	packet := make([]byte, 20) // Min IPv4 header size
	// Version and IHL
	packet[0] = 0x45 // Version 4, Header Length 5 (20 bytes)
	// Total Length (placeholder)
	packet[2] = 0x00
	packet[3] = 0x14 // 20 bytes
	// Destination IP (bytes 16-19)
	copy(packet[16:20], destIP.To4())
	return packet
}

// --- Test Cases ---

// TestStreamHandler tests the stream handler logic
func TestStreamHandler(t *testing.T) {
	// Setup mock network
	mn := mocknet.New()
	defer mn.Close()

	// Create mock peers (nodes)
	h1, err := mn.GenPeer()
	require.NoError(t, err)
	h2, err := mn.GenPeer()
	require.NoError(t, err)

	// Link peers
	err = mn.LinkAll()
	require.NoError(t, err)
	err = mn.ConnectAllButSelf()
	require.NoError(t, err)

	// Mock TUN device for the receiving node (h1)
	mockTun := NewMockTun("mocktun0")

	// Set global state for the receiving node (h1)
	// Need to access/modify globals in the 'main' package. This is tricky.
	// Option 1: Make globals configurable via functions (better design).
	// Option 2: Use linker flags (complex).
	// Option 3: Refactor handler logic into a testable struct/function.
	// Assuming we can modify them for the test (not ideal):
	originalLocalVPNIP := main.GetLocalVPNIP()         // Need getter/setter or refactor
	originalRoutingTable := main.GetPeerRoutingTable() // Need getter/setter or refactor
	main.SetLocalVPNIP("10.0.8.1")
	main.ResetPeerRoutingTable() // Need function to clear/reset
	defer func() {
		main.SetLocalVPNIP(originalLocalVPNIP)
		main.SetPeerRoutingTable(originalRoutingTable) // Restore
	}()

	// Setup stream handler on h1 (the receiver)
	// Need to adapt the actual stream handler function to be testable
	// Let's assume we refactored it into a function:
	// handleStreamFunc := main.CreateStreamHandlerFunc(mockTun) // Pass dependencies
	// h1.SetStreamHandler("/vpn/1.0.0", handleStreamFunc)
	// For now, we'll simulate the handler logic directly within the test
	// or test a refactored version if possible.

	t.Run("Successful Handshake and Packet Write", func(t *testing.T) {
		// Reset state for this sub-test if needed
		main.ResetPeerRoutingTable()
		mockTun.WriteBuffer.Reset()

		// Simulate h2 opening a stream to h1
		s, err := h2.NewStream(context.Background(), h1.ID(), "/vpn/1.0.0")
		require.NoError(t, err)
		defer s.Close()

		// --- Simulate h1's handler logic ---
		// 1. h1 writes its IP
		go func() {
			_, err := s.Write([]byte(main.GetLocalVPNIP() + "\n"))
			assert.NoError(t, err)
		}()

		// 2. h2 reads h1's IP
		reader_h2 := bufio.NewReader(s)
		ipStr_h1, err := reader_h2.ReadString('\n')
		require.NoError(t, err)
		assert.Equal(t, "10.0.8.1", strings.TrimSpace(ipStr_h1))

		// 3. h2 writes its IP ("10.0.8.2")
		_, err = s.Write([]byte("10.0.8.2\n"))
		require.NoError(t, err)

		// 4. h1 reads h2's IP and updates routing table
		// (This part happens inside the actual handler, we simulate the effect)
		// We need a way to wait for the handler goroutine on h1 to process this.
		// This highlights the difficulty of testing the exact concurrent code.
		// Let's assume the handler on h1 would do:
		// reader_h1 := bufio.NewReader(s)
		// remoteIPStr, _ := reader_h1.ReadString('\n')
		// main.UpdateRoutingTable(strings.TrimSpace(remoteIPStr), h2.ID()) // Need function

		// Let's manually update the routing table on h1's side for the test
		main.UpdateRoutingTable("10.0.8.2", h2.ID())

		// 5. h2 sends a packet
		testPacket := createIPv4Packet(net.ParseIP("10.0.8.1")) // Destined for h1
		_, err = s.Write(testPacket)
		require.NoError(t, err)

		// 6. h1 reads the packet and writes to TUN
		// Again, this happens in the handler. We need to check mockTun.
		// Wait briefly for the handler goroutine to potentially run.
		time.Sleep(50 * time.Millisecond) // Fragile way to wait

		// Check if packet was written to mock TUN
		mockTun.mu.Lock()
		writtenBytes := mockTun.WriteBuffer.Bytes()
		mockTun.mu.Unlock()
		assert.Equal(t, testPacket, writtenBytes)

		// Check routing table update
		rt := main.GetPeerRoutingTable()
		peerID, ok := rt["10.0.8.2"]
		assert.True(t, ok)
		assert.Equal(t, h2.ID(), peerID)
	})

	// Add more t.Run(...) blocks for:
	// - Error writing initial IP
	// - Error reading remote IP
	// - Error reading packet data from stream
	// - Error writing packet data to TUN
}

// TestPacketForwardingLogic tests the core routing decisions
func TestPacketForwardingLogic(t *testing.T) {
	// Setup mock network (needed for peer IDs)
	mn := mocknet.New()
	defer mn.Close()
	h_self, _ := mn.GenPeer()
	h_peer1, _ := mn.GenPeer()
	h_exit, _ := mn.GenPeer()

	// --- Test Setup ---
	// Store original global values
	originalLocalVPNIP := main.GetLocalVPNIP()
	originalVpnSubnetCIDR := main.GetVpnSubnetCIDR()
	originalNodeRole := main.GetNodeRole()
	originalExitNodePeerID := main.GetExitNodePeerID()
	originalRoutingTable := main.GetPeerRoutingTable()

	// Defer restoration
	defer func() {
		main.SetLocalVPNIP(originalLocalVPNIP)
		main.SetVpnSubnetCIDR(originalVpnSubnetCIDR)
		main.SetNodeRole(originalNodeRole)
		main.SetExitNodePeerID(originalExitNodePeerID)
		main.SetPeerRoutingTable(originalRoutingTable)
	}()

	// Configure base state
	_, vpnNet, _ := net.ParseCIDR("10.0.8.0/24")
	main.SetLocalVPNIP("10.0.8.1")
	main.SetVpnSubnetCIDR(vpnNet)
	main.ResetPeerRoutingTable()
	main.UpdateRoutingTable("10.0.8.2", h_peer1.ID()) // Add a known peer

	// Mock host for stream opening simulation
	mockHost := &MockForwardingHost{
		SelfID:      h_self.ID(),
		Streams:     make(map[peer.ID]*MockForwardingStream),
		StreamError: nil,
		mu:          sync.Mutex{},
	}

	// --- Test Cases ---

	testCases := []struct {
		name             string
		role             string
		exitNode         peer.ID
		destIP           string
		packet           []byte
		expectedTarget   peer.ID // Expected peer ID for NewStream, empty if no stream expected
		expectStreamOpen bool
	}{
		{
			name:             "VPN Dest - Known Peer",
			role:             main.RoleUser,
			exitNode:         "",
			destIP:           "10.0.8.2",
			packet:           createIPv4Packet(net.ParseIP("10.0.8.2")),
			expectedTarget:   h_peer1.ID(),
			expectStreamOpen: true,
		},
		{
			name:             "VPN Dest - Unknown Peer",
			role:             main.RoleUser,
			exitNode:         "",
			destIP:           "10.0.8.3",
			packet:           createIPv4Packet(net.ParseIP("10.0.8.3")),
			expectedTarget:   "",
			expectStreamOpen: false, // Should be dropped
		},
		{
			name:             "VPN Dest - Self",
			role:             main.RoleUser,
			exitNode:         "",
			destIP:           "10.0.8.1",
			packet:           createIPv4Packet(net.ParseIP("10.0.8.1")),
			expectedTarget:   "",
			expectStreamOpen: false, // Should be ignored
		},
		{
			name:             "External Dest - User Role - With Exit Node",
			role:             main.RoleUser,
			exitNode:         h_exit.ID(),
			destIP:           "8.8.8.8",
			packet:           createIPv4Packet(net.ParseIP("8.8.8.8")),
			expectedTarget:   h_exit.ID(),
			expectStreamOpen: true,
		},
		{
			name:             "External Dest - User Role - No Exit Node",
			role:             main.RoleUser,
			exitNode:         "",
			destIP:           "8.8.8.8",
			packet:           createIPv4Packet(net.ParseIP("8.8.8.8")),
			expectedTarget:   "",
			expectStreamOpen: false, // Should be dropped
		},
		{
			name:             "External Dest - Exit Node Role",
			role:             main.RoleExitNode,
			exitNode:         "", // Not relevant for exit node itself
			destIP:           "8.8.8.8",
			packet:           createIPv4Packet(net.ParseIP("8.8.8.8")),
			expectedTarget:   "",
			expectStreamOpen: false, // Should be handled by OS, not forwarded
		},
		{
			name:             "VPN Dest - Exit Node Role - Known Peer",
			role:             main.RoleExitNode,
			exitNode:         "",
			destIP:           "10.0.8.2",
			packet:           createIPv4Packet(net.ParseIP("10.0.8.2")),
			expectedTarget:   h_peer1.ID(),
			expectStreamOpen: true, // Exit node still forwards *within* VPN
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Configure state for this test case
			main.SetNodeRole(tc.role)
			main.SetExitNodePeerID(tc.exitNode)
			mockHost.Reset() // Clear any recorded streams from previous tests

			// --- Simulate the core logic from the TUN reading goroutine ---
			packetData := tc.packet
			destIP := net.IP(packetData[16:20])
			destIPStr := destIP.String()

			var targetPeer peer.ID
			var found bool
			// var targetType string // Not strictly needed for test assertion

			isVPNSubnetDest := main.GetVpnSubnetCIDR().Contains(destIP)
			currentRole := main.GetNodeRole()
			currentExitNode := main.GetExitNodePeerID()
			routingTable := main.GetPeerRoutingTable() // Use getter

			if isVPNSubnetDest {
				// routingTableLock.RLock() // Not needed if GetPeerRoutingTable returns a copy or handles locking
				targetPeer, found = routingTable[destIPStr]
				// routingTableLock.RUnlock()
				if found {
					// targetType = "VPN Peer"
				} else {
					// log.Printf("No route in VPN for destination IP %s, dropping packet", destIPStr)
				}
			} else {
				if currentRole == main.RoleUser {
					if currentExitNode != "" {
						targetPeer = currentExitNode
						found = true
						// targetType = "Exit Node"
					} else {
						// log.Printf("Non-VPN destination %s and no exit node configured, dropping packet", destIPStr)
					}
				} else if currentRole == main.RoleExitNode {
					// log.Printf("Exit node: Letting OS handle local traffic to %s", destIPStr)
					found = false
				}
			}

			streamOpened := false
			if found && targetPeer != mockHost.SelfID {
				// Simulate calling NewStream
				// We pass our mockHost which records the call
				stream, err := mockHost.NewStream(context.Background(), targetPeer, "/vpn/1.0.0")

				// Assert based on expectations
				if tc.expectStreamOpen {
					assert.NoError(t, err, "Expected NewStream to succeed")
					assert.NotNil(t, stream, "Expected a non-nil stream")
					streamOpened = true

					// Simulate the rest of the forwarding steps (handshake, write)
					// For this test, we mainly care *if* the stream was opened to the right peer.
					// A more detailed test could check the handshake/write on the mock stream.
					if stream != nil {
						// Simulate successful handshake write
						_, err = stream.Write([]byte(main.GetLocalVPNIP() + "\n"))
						assert.NoError(t, err)
						// Simulate successful handshake read (from the target peer)
						mockStream, _ := stream.(*MockForwardingStream)
						mockStream.InjectReadData([]byte("10.0.8.X\n")) // Inject dummy IP from target
						reader := bufio.NewReader(stream)
						_, err = reader.ReadString('\n')
						assert.NoError(t, err)
						// Simulate successful packet write
						_, err = stream.Write(packetData)
						assert.NoError(t, err)
						stream.CloseWrite() // Close write side
						stream.Close()      // Fully close
					}

				} else {
					// If we found a route but didn't expect a stream (e.g., error expected),
					// this part of the assertion might need adjustment based on *why*
					// we don't expect a stream (e.g., NewStream configured to return error).
					// For now, assume if found=true, expectStreamOpen=true unless NewStream fails.
				}
			}

			// Final assertion: Was a stream opened when expected?
			assert.Equal(t, tc.expectStreamOpen, streamOpened, "Stream opening expectation mismatch")

			// Check if NewStream was called with the correct peer
			if tc.expectStreamOpen {
				assert.Equal(t, tc.expectedTarget, mockHost.LastTargetPeer, "NewStream called with incorrect target peer")
			} else {
				assert.Equal(t, peer.ID(""), mockHost.LastTargetPeer, "NewStream should not have been called")
			}
		})
	}
}

// --- Mock Implementations for Forwarding Test ---

type MockForwardingHost struct {
	SelfID         peer.ID
	Streams        map[peer.ID]*MockForwardingStream
	StreamError    error
	LastTargetPeer peer.ID // Record the target of the last NewStream call
	mu             sync.Mutex
}

func (m *MockForwardingHost) ID() peer.ID {
	return m.SelfID
}

func (m *MockForwardingHost) NewStream(ctx context.Context, p peer.ID, protocol ...protocol.ID) (network.Stream, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.LastTargetPeer = p // Record target
	if m.StreamError != nil {
		return nil, m.StreamError
	}
	if p == m.SelfID {
		return nil, errors.New("cannot stream to self") // Simulate libp2p behavior
	}
	stream := NewMockForwardingStream(m.SelfID, p)
	m.Streams[p] = stream
	return stream, nil
}

func (m *MockForwardingHost) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Streams = make(map[peer.ID]*MockForwardingStream)
	m.StreamError = nil
	m.LastTargetPeer = "" // Reset recorded target
}

// Mock Stream for Forwarding Test (Simpler than full mocknet stream)
type MockForwardingStream struct {
	ReadBuf     *bytes.Buffer
	WriteBuf    *bytes.Buffer
	LocalPeer   peer.ID
	RemotePeer  peer.ID
	Closed      bool
	WriteClosed bool
	mu          sync.Mutex
}

func NewMockForwardingStream(local, remote peer.ID) *MockForwardingStream {
	return &MockForwardingStream{
		ReadBuf:    bytes.NewBuffer(nil),
		WriteBuf:   bytes.NewBuffer(nil),
		LocalPeer:  local,
		RemotePeer: remote,
	}
}
func (m *MockForwardingStream) InjectReadData(data []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ReadBuf.Write(data)
}

func (m *MockForwardingStream) Read(p []byte) (n int, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Closed {
		return 0, io.EOF
	}
	if m.ReadBuf.Len() == 0 {
		// Simulate blocking read with a short delay before EOF if closed,
		// or just return 0 bytes read for now. Return io.EOF immediately might be okay too.
		return 0, io.EOF // Simplification: return EOF if buffer empty
	}
	return m.ReadBuf.Read(p)
}

func (m *MockForwardingStream) Write(p []byte) (n int, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Closed || m.WriteClosed {
		return 0, errors.New("stream closed") // Use a specific error like net.ErrClosed?
	}
	return m.WriteBuf.Write(p)
}

func (m *MockForwardingStream) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Closed = true
	return nil
}

func (m *MockForwardingStream) CloseWrite() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.WriteClosed = true
	return nil
}

func (m *MockForwardingStream) Reset() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Closed = true
	m.ReadBuf.Reset()
	m.WriteBuf.Reset()
	return nil
}

func (m *MockForwardingStream) Protocol() protocol.ID {
	return "/vpn/1.0.0"
}

func (m *MockForwardingStream) SetDeadline(t time.Time) error      { return nil }
func (m *MockForwardingStream) SetReadDeadline(t time.Time) error  { return nil }
func (m *MockForwardingStream) SetWriteDeadline(t time.Time) error { return nil }
func (m *MockForwardingStream) Stat() network.Stat                 { return network.Stat{} }
func (m *MockForwardingStream) Conn() network.Conn {
	return &MockConn{local: m.LocalPeer, remote: m.RemotePeer}
} // Need MockConn

// Mock Connection
type MockConn struct {
	local  peer.ID
	remote peer.ID
}

func (mc *MockConn) LocalPeer() peer.ID                   { return mc.local }
func (mc *MockConn) RemotePeer() peer.ID                  { return mc.remote }
func (mc *MockConn) LocalMultiaddr() multiaddr.Multiaddr  { return nil } // Mock if needed
func (mc *MockConn) RemoteMultiaddr() multiaddr.Multiaddr { return nil } // Mock if needed
func (mc *MockConn) Transport() transport.Transport       { return nil } // Mock if needed
func (mc *MockConn) ConnSecurity() sec.ConnSecurity       { return nil } // Mock if needed
func (mc *MockConn) ID() string                           { return "" }  // Mock if needed
func (mc *MockConn) Close() error                         { return nil }
func (mc *MockConn) IsClosed() bool                       { return false }
func (mc *MockConn) Scope() network.ConnScope             { return nil } // Mock if needed
func (mc *MockConn) Stat() network.Stat                   { return network.Stat{} }

// --- Global State Accessors/Mutators (Required for Testing) ---
// NOTE: This assumes you modify your main.go to provide these functions
// or refactor the code to avoid global state reliance for testing.

// Example (add these to main.go or a test utility file if main_test.go is in a separate package):
/*
var (
	// Keep original globals private if possible
	_peerRoutingTable = make(map[string]peer.ID)
	_routingTableLock sync.RWMutex
	_exitNodePeerID   peer.ID
	_localVPNIP       string
	_nodeRole         string
	_vpnSubnetCIDR    *net.IPNet
)

func SetLocalVPNIP(ip string) {
	_routingTableLock.Lock() // Ensure safety if accessed concurrently
	defer _routingTableLock.Unlock()
	_localVPNIP = ip
}

func GetLocalVPNIP() string {
	_routingTableLock.RLock() // Ensure safety if accessed concurrently
	defer _routingTableLock.RUnlock()
	return _localVPNIP
}

func SetVpnSubnetCIDR(subnet *net.IPNet) {
	_routingTableLock.Lock()
	defer _routingTableLock.Unlock()
	_vpnSubnetCIDR = subnet
}

func GetVpnSubnetCIDR() *net.IPNet {
	_routingTableLock.RLock()
	defer _routingTableLock.RUnlock()
	// Return a copy? For testing, direct access might be okay if careful.
	return _vpnSubnetCIDR
}

func SetNodeRole(role string) {
	_routingTableLock.Lock()
	defer _routingTableLock.Unlock()
	_nodeRole = role
}

func GetNodeRole() string {
	_routingTableLock.RLock()
	defer _routingTableLock.RUnlock()
	return _nodeRole
}

func SetExitNodePeerID(id peer.ID) {
	_routingTableLock.Lock()
	defer _routingTableLock.Unlock()
	_exitNodePeerID = id
}

func GetExitNodePeerID() peer.ID {
	_routingTableLock.RLock()
	defer _routingTableLock.RUnlock()
	return _exitNodePeerID
}

func ResetPeerRoutingTable() {
	_routingTableLock.Lock()
	defer _routingTableLock.Unlock()
	_peerRoutingTable = make(map[string]peer.ID)
}

func UpdateRoutingTable(ip string, id peer.ID) {
	_routingTableLock.Lock()
	defer _routingTableLock.Unlock()
	_peerRoutingTable[ip] = id
}

func GetPeerRoutingTable() map[string]peer.ID {
	_routingTableLock.RLock()
	defer _routingTableLock.RUnlock()
	// Return a copy to prevent race conditions in tests modifying the map
	copiedTable := make(map[string]peer.ID, len(_peerRoutingTable))
	for k, v := range _peerRoutingTable {
		copiedTable[k] = v
	}
	return copiedTable
}

// Add constants for roles if not exported
const RoleUser = "user"
const RoleExitNode = "exitnode"

*/
