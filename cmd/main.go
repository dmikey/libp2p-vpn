package main

import (
	"bufio" // Added import
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time" // Added for potential timeouts/retries

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/discovery/mdns"
	"github.com/songgao/water"
)

const discoveryServiceTag = "p2p-vpn-example"

type discoveryNotifee struct {
	h host.Host
}

func (n *discoveryNotifee) HandlePeerFound(pi peer.AddrInfo) {
	// Log regardless of whether it's the exit node or not
	log.Printf("mDNS Found peer: %s with %d addresses", pi.ID.String(), len(pi.Addrs))

	// Check if the found peer is the configured exit node
	isExitNode := exitNodePeerID != "" && pi.ID == exitNodePeerID
	if isExitNode {
		log.Printf(">>> Discovered configured Exit Node: %s", pi.ID.String())
	}

	// Check if we already have addresses for this peer in the peerstore
	// Also check if we are already connected
	if len(n.h.Peerstore().Addrs(pi.ID)) == 0 || n.h.Network().Connectedness(pi.ID) != network.Connected {
		log.Printf("Peer %s not connected or no addresses known, attempting connection...", pi.ID.String())
		// Attempt to connect to the newly found peer to populate addresses and establish connection
		err := n.h.Connect(context.Background(), pi)
		if err != nil {
			log.Printf("Failed to connect to newly found peer %s: %v", pi.ID.String(), err)
		} else {
			log.Printf("Successfully connected to %s (addresses should now be in peerstore)", pi.ID.String())
			if isExitNode {
				log.Printf(">>> Successfully connected to Exit Node: %s", pi.ID.String())
				// --- Proactively establish VPN stream after successful connection ---
				log.Printf("Attempting to proactively establish VPN stream with %s", pi.ID.String())
				// Use a short timeout context for this attempt
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second) // Increased timeout slightly
				defer cancel()
				stream := getOrCreateStream(ctx, n.h, pi.ID)
				if stream != nil {
					log.Printf("Successfully established proactive VPN stream with %s (Stream ID: %s)", pi.ID.String(), stream.ID())
					// Note: getOrCreateStream handles adding to peerStreams map
				} else {
					log.Printf("Failed to establish proactive VPN stream with %s (will retry on demand)", pi.ID.String())
					// No need to remove stream here, getOrCreateStream handles cleanup on failure
				}
				// --- End proactive stream establishment ---
			}
		}
	} else {
		// Optional: Log if the peer was found again but we were already connected
		// log.Printf("Peer %s found by mDNS, already connected.", pi.ID.String())

		// --- Check if VPN stream exists, if not, try to establish it ---
		// This handles cases where the initial connection might exist, but the VPN stream dropped or wasn't established.
		streamLock.Lock()
		_, streamExists := peerStreams[pi.ID]
		streamLock.Unlock()

		if !streamExists {
			log.Printf("Peer %s connected, but no active VPN stream found. Attempting proactive stream establishment.", pi.ID.String())
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			stream := getOrCreateStream(ctx, n.h, pi.ID)
			if stream != nil {
				log.Printf("Successfully established proactive VPN stream with %s (Stream ID: %s) on re-discovery.", pi.ID.String(), stream.ID())
			} else {
				log.Printf("Failed to establish proactive VPN stream with %s on re-discovery (will retry on demand).", pi.ID.String())
			}
		}
		// --- End stream check ---
	}
}

func setupTun() (*water.Interface, error) {
	config := water.Config{
		DeviceType: water.TUN,
	}
	// Set TUN device name based on OS
	if runtime.GOOS == "darwin" {
		// On macOS, leave Name empty to let the system assign the next available utun device
		// config.Name = "utun0" // Removed this line
	} else {
		config.Name = "p2pvpn0" // Default name for other OS (e.g., Linux)
	}
	iface, err := water.New(config)
	if err != nil {
		return nil, err
	}
	return iface, nil
}

// configureTunDevice sets the IP address and adds routes for the TUN device.
func configureTunDevice(ifaceName, ipNet, subnet string) error { // Added subnet parameter
	ipAddr := strings.Split(ipNet, "/")[0]
	// subnet := ipNet // e.g., 10.0.8.0/24 // Removed this line, use parameter instead

	var cmd *exec.Cmd
	var routeCmd *exec.Cmd

	log.Printf("Configuring TUN device %s with IP %s", ifaceName, ipNet)

	switch runtime.GOOS {
	case "darwin":
		// Assign IP address
		cmd = exec.Command("ifconfig", ifaceName, "inet", ipAddr, ipAddr, "up")
		log.Printf("Running command: %s", cmd.String())
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("failed to assign IP address on macOS: %w", err)
		}
		// Add route
		routeCmd = exec.Command("route", "add", "-net", subnet, ipAddr) // Use subnet parameter
		log.Printf("Running command: %s", routeCmd.String())
		if err := routeCmd.Run(); err != nil {
			// Don't fatal on route error, might already exist or have permission issues
			log.Printf("Warning: failed to add route on macOS: %v", err)
		}
	case "linux":
		// Assign IP address and bring up interface
		cmd = exec.Command("ip", "addr", "add", ipNet, "dev", ifaceName)
		log.Printf("Running command: %s", cmd.String())
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("failed to assign IP address on Linux: %w", err)
		}
		upCmd := exec.Command("ip", "link", "set", "dev", ifaceName, "up")
		log.Printf("Running command: %s", upCmd.String())
		if err := upCmd.Run(); err != nil {
			return fmt.Errorf("failed to bring up interface %s on Linux: %w", ifaceName, err)
		}
		// Add route
		routeCmd = exec.Command("ip", "route", "add", subnet, "dev", ifaceName) // Use subnet parameter
		log.Printf("Running command: %s", routeCmd.String())
		if err := routeCmd.Run(); err != nil {
			// Don't fatal on route error
			log.Printf("Warning: failed to add route on Linux: %v", err)
		}
	default:
		return fmt.Errorf("unsupported OS: %s", runtime.GOOS)
	}

	log.Printf("Successfully configured TUN device %s", ifaceName)
	return nil
}

// cleanupTunDevice removes the IP address and routes for the TUN device.
func cleanupTunDevice(ifaceName, ipNet, subnet string) { // Added subnet parameter
	ipAddr := strings.Split(ipNet, "/")[0]
	// subnet := ipNet // e.g., 10.0.8.0/24 // Removed this line, use parameter instead

	log.Printf("Cleaning up TUN device %s (IP: %s, Subnet: %s)", ifaceName, ipNet, subnet)

	var routeCmd *exec.Cmd
	var ipCmd *exec.Cmd
	var downCmd *exec.Cmd

	switch runtime.GOOS {
	case "darwin":
		// Delete route first
		routeCmd = exec.Command("route", "delete", "-net", subnet, ipAddr) // Use subnet parameter
		log.Printf("Running command: %s", routeCmd.String())
		if err := routeCmd.Run(); err != nil {
			// Log error but continue cleanup
			log.Printf("Warning: failed to delete route on macOS: %v", err)
		}
		// Bring interface down (might implicitly remove IP or make it easier)
		downCmd = exec.Command("ifconfig", ifaceName, "down")
		log.Printf("Running command: %s", downCmd.String())
		if err := downCmd.Run(); err != nil {
			log.Printf("Warning: failed to bring down interface %s on macOS: %v", ifaceName, err)
		}
		// Attempt to delete IP (might fail if 'down' removed it, which is fine)
		// Note: 'ifconfig delete' might not be standard. Bringing down is usually sufficient.
		// We'll skip explicit IP deletion on macOS for now as 'down' often handles it.
		// ipCmd = exec.Command("ifconfig", ifaceName, "inet", ipAddr, "-alias") // Alternative?
		// log.Printf("Running command: %s", ipCmd.String())
		// if err := ipCmd.Run(); err != nil {
		// 	log.Printf("Warning: failed to remove IP address on macOS: %v", err)
		// }

	case "linux":
		// Delete route
		routeCmd = exec.Command("ip", "route", "del", subnet, "dev", ifaceName) // Use subnet parameter
		log.Printf("Running command: %s", routeCmd.String())
		if err := routeCmd.Run(); err != nil {
			// Log error but continue cleanup
			log.Printf("Warning: failed to delete route on Linux: %v", err)
		}
		// Remove IP address
		ipCmd = exec.Command("ip", "addr", "del", ipNet, "dev", ifaceName)
		log.Printf("Running command: %s", ipCmd.String())
		if err := ipCmd.Run(); err != nil {
			log.Printf("Warning: failed to remove IP address on Linux: %v", err)
		}
		// Bring interface down
		downCmd = exec.Command("ip", "link", "set", "dev", ifaceName, "down")
		log.Printf("Running command: %s", downCmd.String())
		if err := downCmd.Run(); err != nil {
			log.Printf("Warning: failed to bring down interface %s on Linux: %v", ifaceName, err)
		}
	default:
		log.Printf("Cleanup not implemented for OS: %s", runtime.GOOS)
	}

	log.Printf("Finished cleanup attempt for TUN device %s", ifaceName)
}

// Global routing table, exit node info, role, subnet info, and stream management
var (
	peerRoutingTable = make(map[string]peer.ID)
	routingTableLock sync.RWMutex
	exitNodePeerID   peer.ID
	localVPNIP       string
	nodeRole         string     // Added: "user" or "exitnode"
	vpnSubnetCIDR    *net.IPNet // Added: Parsed VPN subnet

	// Map to store active outgoing streams to peers
	peerStreams = make(map[peer.ID]network.Stream)
	streamLock  sync.Mutex
)

const (
	roleUser     = "user"
	roleExitNode = "exitnode"
)

// getOrCreateStream finds an existing stream to the target peer or creates a new one.
// Performs handshake for new streams. Returns nil if a stream cannot be established.
func getOrCreateStream(ctx context.Context, node host.Host, targetPeer peer.ID) network.Stream {
	// Add a check to prevent dialing self
	if targetPeer == node.ID() {
		log.Printf("Attempted to get/create stream to self (%s), skipping.", targetPeer)
		return nil
	}

	streamLock.Lock()
	stream, ok := peerStreams[targetPeer]
	streamLock.Unlock()

	if ok {
		// TODO: Add a check here to see if the stream is still valid?
		// Simplest check: try a zero-byte write or check stats if available.
		// For now, assume it's valid; if subsequent writes fail, it will be removed then.
		log.Printf("Reusing existing stream to %s (Stream ID: %s)", targetPeer, stream.ID()) // Added log
		return stream
	}

	// --- Check if we know the peer's addresses before trying to open a stream ---
	peerAddrs := node.Peerstore().Addrs(targetPeer) // Get addresses
	if len(peerAddrs) == 0 {                        // Check length
		log.Printf("Cannot open stream: No known addresses for peer %s. Discovery might be pending.", targetPeer)
		// Don't attempt node.Connect here, let discovery handle the initial connection.
		return nil // Indicate failure to establish stream for now
	}
	// --- End address check ---

	// --- Check connectedness before creating stream ---
	if node.Network().Connectedness(targetPeer) != network.Connected {
		log.Printf("Cannot open stream: Peer %s is not connected. Waiting for connection.", targetPeer)
		// Attempting a connect here might be redundant if discovery is running,
		// but could help if discovery missed it or the connection dropped.
		// Let's try connecting explicitly if not connected.
		connectCtx, cancel := context.WithTimeout(ctx, 10*time.Second) // Use parent ctx for timeout
		defer cancel()
		log.Printf("Attempting explicit connect to %s before creating stream...", targetPeer)
		err := node.Connect(connectCtx, peer.AddrInfo{ID: targetPeer, Addrs: peerAddrs})
		if err != nil {
			log.Printf("Explicit connect to %s failed: %v. Cannot create stream.", targetPeer, err)
			return nil
		}
		log.Printf("Explicit connect to %s successful.", targetPeer)
		// Re-check connectedness just in case
		if node.Network().Connectedness(targetPeer) != network.Connected {
			log.Printf("Peer %s still not connected after explicit attempt. Cannot create stream.", targetPeer)
			return nil
		}
	}
	// --- End connectedness check ---

	// No existing stream, create a new one
	// Log the addresses we know for debugging
	log.Printf("Attempting to open NEW stream to %s (addresses known, peer connected)", targetPeer) // Modified log
	newStream, err := node.NewStream(ctx, targetPeer, "/vpn/1.0.0")                                 // Use new variable name
	if err != nil {
		// Log the specific error from NewStream
		log.Printf("Failed to open new stream to target %s: %v", targetPeer, err)
		// Check if the error is due to lack of addresses again, although the check above should prevent this.
		if strings.Contains(err.Error(), "no addresses") {
			log.Printf("Stream opening failed specifically due to 'no addresses' for %s, despite earlier check.", targetPeer)
		} else if strings.Contains(err.Error(), "context deadline exceeded") {
			log.Printf("Stream opening to %s failed due to context deadline exceeded.", targetPeer)
		} else if strings.Contains(err.Error(), "dial backoff") {
			log.Printf("Stream opening to %s failed due to dial backoff.", targetPeer)
		}
		return nil // Indicate failure
	}
	// Log success immediately after creation attempt, before handshake
	log.Printf("Successfully initiated new stream to %s (Stream ID: %s), attempting handshake...", targetPeer, newStream.ID())

	// Perform handshake for the new stream
	// 1. Write local VPN IP
	// Set deadline for write
	writeDeadline := time.Now().Add(10 * time.Second)
	if err := newStream.SetWriteDeadline(writeDeadline); err != nil {
		log.Printf("Handshake Error: Failed setting write deadline for peer %s (Stream ID: %s): %v", targetPeer, newStream.ID(), err)
		newStream.Reset()
		return nil
	}
	_, err = newStream.Write([]byte(localVPNIP + "\n")) // Use newStream
	if err != nil {
		// Log the specific error from Write
		log.Printf("Handshake Error: Failed writing IP to peer %s (Stream ID: %s): %v", targetPeer, newStream.ID(), err)
		newStream.Reset() // Close the new stream if handshake fails
		return nil
	}
	// Reset deadline after successful write
	if err := newStream.SetWriteDeadline(time.Time{}); err != nil { // Zero time removes deadline
		log.Printf("Handshake Warning: Failed resetting write deadline for peer %s (Stream ID: %s): %v", targetPeer, newStream.ID(), err)
		// Continue anyway, but log the warning
	}
	// log.Printf("Sent local IP %s to %s (new stream handshake, Stream ID: %s)", localVPNIP, targetPeer, newStream.ID())

	// 2. Read remote peer's VPN IP
	// Use a timeout for the handshake read - context passed in should handle this, but SetReadDeadline is more direct for the I/O op
	readDeadline := time.Now().Add(10 * time.Second)
	if err := newStream.SetReadDeadline(readDeadline); err != nil {
		log.Printf("Handshake Error: Failed setting read deadline for peer %s (Stream ID: %s): %v", targetPeer, newStream.ID(), err)
		newStream.Reset()
		return nil
	}
	reader := bufio.NewReader(newStream) // Use newStream, buffered reader just for the handshake line
	remoteIPStr, err := reader.ReadString('\n')
	// Reset deadline immediately after read attempt, regardless of success/failure
	if errReset := newStream.SetReadDeadline(time.Time{}); errReset != nil {
		log.Printf("Handshake Warning: Failed resetting read deadline for peer %s (Stream ID: %s): %v", targetPeer, newStream.ID(), errReset)
	}
	// Now check the error from ReadString
	if err != nil {
		log.Printf("Handshake Error: Failed reading IP from peer %s (Stream ID: %s): %v", targetPeer, newStream.ID(), err)
		newStream.Reset() // Close the new stream
		return nil
	}
	remoteIPStr = strings.TrimSpace(remoteIPStr)
	// log.Printf("Received IP %s from %s (new stream handshake, Stream ID: %s)", remoteIPStr, targetPeer, newStream.ID())

	// 3. Update routing table (ensure mapping is correct)
	routingTableLock.Lock()
	peerRoutingTable[remoteIPStr] = targetPeer
	routingTableLock.Unlock()
	log.Printf("Updated routing table (new stream): %s -> %s", remoteIPStr, targetPeer)

	// Store the newly created and handshaked stream, replacing any existing one.
	streamLock.Lock()
	existingStream, exists := peerStreams[targetPeer]
	if exists {
		// An existing stream was found. Close it and replace it with the new one.
		log.Printf("Replacing existing stream (ID: %s) with newly handshaked stream (ID: %s) for peer %s", existingStream.ID(), newStream.ID(), targetPeer)
		existingStream.Reset() // Close the old stream gracefully
	}
	// Store the new stream in the map
	peerStreams[targetPeer] = newStream
	streamLock.Unlock()
	log.Printf("Stored new/updated stream for peer %s (Stream ID: %s)", targetPeer, newStream.ID())

	return newStream // Return the newly handshaked stream
}

func removeStream(targetPeer peer.ID) {
	streamLock.Lock()
	defer streamLock.Unlock()

	if stream, ok := peerStreams[targetPeer]; ok {
		log.Printf("Removing and closing stream for peer %s", targetPeer)
		stream.Reset() // Use Reset for forceful closure
		delete(peerStreams, targetPeer)
	}
}

func main() {
	// Define command-line flags
	discoveryTag := flag.String("tag", "p2p-vpn-example", "Unique discovery tag for the VPN service")
	vpnSubnet := flag.String("subnet", "10.0.8.0/24", "Subnet for the VPN network (e.g., 10.0.8.0/24)")
	localVPNIPNet := flag.String("ip", "10.0.8.1/24", "Local IP address for this node within the VPN subnet (e.g., 10.0.8.1/24)")
	exitNodeStr := flag.String("exitnode", "", "Optional Peer ID of the exit node for non-VPN traffic (used by 'user' role)")
	flag.StringVar(&nodeRole, "role", roleUser, "Role of this node: 'user' or 'exitnode'") // Added role flag

	flag.Parse() // Parse the flags

	// Validate role
	if nodeRole != roleUser && nodeRole != roleExitNode {
		log.Fatalf("Invalid role specified: %s. Must be '%s' or '%s'", nodeRole, roleUser, roleExitNode)
	}
	log.Printf("Starting node with role: %s", nodeRole)

	// Extract local IP from CIDR
	ip, _, err := net.ParseCIDR(*localVPNIPNet)
	if err != nil {
		log.Fatalf("Invalid local VPN IP format: %v", err)
	}
	localVPNIP = ip.String() // Store the local IP

	// Parse the VPN Subnet CIDR
	_, vpnSubnetCIDR, err = net.ParseCIDR(*vpnSubnet)
	if err != nil {
		log.Fatalf("Invalid VPN subnet format: %v", err)
	}
	log.Printf("Using VPN Subnet: %s", vpnSubnetCIDR.String())

	// Parse Exit Node Peer ID if provided (relevant for 'user' role)
	if *exitNodeStr != "" {
		exitNodePeerID, err = peer.Decode(*exitNodeStr)
		if err != nil {
			log.Fatalf("Invalid exit node Peer ID: %v", err)
		}
		log.Printf("Configured exit node: %s", exitNodePeerID.String())
	} else if nodeRole == roleUser {
		log.Println("No exit node configured. Only VPN subnet traffic will be routed.")
	}

	// Add warning for exit node configuration
	if nodeRole == roleExitNode {
		log.Println("--- EXIT NODE WARNING ---")
		log.Println("Ensure OS-level IP forwarding and NAT rules are configured correctly")
		log.Printf("Example (Linux): sysctl -w net.ipv4.ip_forward=1")
		log.Printf("Example (Linux): iptables -t nat -A POSTROUTING -s %s -o <wan_interface> -j MASQUERADE", vpnSubnetCIDR.String())
		log.Println("Example (macOS): sysctl -w net.inet.ip.forwarding=1")
		log.Println("Example (macOS): Add rule to /etc/pf.conf: nat on <wan_interface> from <tun_interface>:network to any -> (<wan_interface>)")
		log.Println("-------------------------")
	}

	tunIface, err := setupTun()
	if err != nil {
		log.Fatal(err)
	}
	log.Println("TUN device created:", tunIface.Name())

	// Configure the TUN device IP and routes using flag values
	if err := configureTunDevice(tunIface.Name(), *localVPNIPNet, *vpnSubnet); err != nil { // Use flag values
		log.Fatalf("Error configuring TUN device: %v", err)
	}

	node, err := libp2p.New()
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("Node started with ID: %s", node.ID().String())
	for _, addr := range node.Addrs() {
		log.Printf("Listening on address: %s/p2p/%s", addr, node.ID().String())
	}

	notifee := &discoveryNotifee{h: node}
	// Use flag value for discovery tag
	srv := mdns.NewMdnsService(node, *discoveryTag, notifee)
	if err := srv.Start(); err != nil {
		log.Fatal(err)
	}

	// --- Stream Handler ---
	// (The stream handler logic remains largely the same: it receives packets from a peer
	// and writes them to the local TUN interface. The OS then routes them.)
	node.SetStreamHandler("/vpn/1.0.0", func(s network.Stream) {
		remotePeerID := s.Conn().RemotePeer()
		// Use a unique identifier for this handler instance for easier log tracking
		handlerID := fmt.Sprintf("%s->%s", remotePeerID.ShortString(), node.ID().ShortString())
		log.Printf("[%s] New INCOMING stream connection from %s (Stream ID: %s)", handlerID, remotePeerID, s.ID())

		// Perform handshake: Write local IP, Read remote IP
		go func(stream network.Stream) { // Pass stream explicitly
			defer func() {
				log.Printf("[%s] Closing stream handler for %s (Stream ID: %s)", handlerID, remotePeerID, stream.ID())
				stream.Close() // Ensure stream is closed when handler exits
				// Remove peer from routing table on disconnect
				routingTableLock.Lock()
				var removedIP string
				for ip, pid := range peerRoutingTable {
					if pid == remotePeerID {
						delete(peerRoutingTable, ip)
						removedIP = ip
						break
					}
				}
				routingTableLock.Unlock()
				if removedIP != "" {
					log.Printf("[%s] Removed route for %s (%s) due to stream close", handlerID, removedIP, remotePeerID)
				}
				// Also remove any outgoing stream we might have had for them
				removeStream(remotePeerID) // removeStream already logs
			}()

			// --- Handshake Timeout Context ---
			// Use a context for the overall handshake process within the handler
			handlerHandshakeCtx, handlerCancel := context.WithTimeout(context.Background(), 20*time.Second) // Slightly longer timeout for handler side
			defer handlerCancel()

			// 1. Write local VPN IP to the peer
			log.Printf("[%s] Handshake Step 1: Writing local IP %s to %s", handlerID, localVPNIP, remotePeerID)
			// Set deadline for write
			writeDeadline := time.Now().Add(10 * time.Second)
			if err := stream.SetWriteDeadline(writeDeadline); err != nil {
				log.Printf("[%s] Handshake Step 1 FAILED: Error setting write deadline for peer %s: %v", handlerID, remotePeerID, err)
				stream.Reset()
				return
			}
			_, err := stream.Write([]byte(localVPNIP + "\n"))
			// Reset deadline immediately
			if errReset := stream.SetWriteDeadline(time.Time{}); errReset != nil {
				log.Printf("[%s] Handshake Warning: Failed resetting write deadline for peer %s: %v", handlerID, remotePeerID, errReset)
			}
			// Check write error
			if err != nil {
				log.Printf("[%s] Handshake Step 1 FAILED: Error writing IP to peer %s: %v", handlerID, remotePeerID, err)
				stream.Reset() // Reset stream on error
				return
			}
			log.Printf("[%s] Handshake Step 1 SUCCESS: Sent local IP %s to %s", handlerID, localVPNIP, remotePeerID)

			// 2. Read remote peer's VPN IP
			log.Printf("[%s] Handshake Step 2: Reading remote IP from %s", handlerID, remotePeerID)
			// Set read deadline
			readDeadline := time.Now().Add(10 * time.Second)
			if err := stream.SetReadDeadline(readDeadline); err != nil {
				log.Printf("[%s] Handshake Step 2 FAILED: Error setting read deadline for peer %s: %v", handlerID, remotePeerID, err)
				stream.Reset()
				return
			}
			reader := bufio.NewReader(stream) // Buffered reader for handshake line
			remoteIPStr, err := reader.ReadString('\n')
			// Reset deadline immediately
			if errReset := stream.SetReadDeadline(time.Time{}); errReset != nil {
				log.Printf("[%s] Handshake Warning: Failed resetting read deadline for peer %s: %v", handlerID, remotePeerID, errReset)
			}
			// Check read error
			if err != nil {
				// Check if the error is due to the overall handler context timeout
				if handlerHandshakeCtx.Err() == context.DeadlineExceeded {
					log.Printf("[%s] Handshake Step 2 FAILED: Overall handshake timeout reading IP from peer %s", handlerID, remotePeerID)
				} else {
					log.Printf("[%s] Handshake Step 2 FAILED: Error reading IP from peer %s: %v", handlerID, remotePeerID, err)
				}
				stream.Reset()
				return
			}
			remoteIPStr = strings.TrimSpace(remoteIPStr)
			log.Printf("[%s] Handshake Step 2 SUCCESS: Received IP %s from %s", handlerID, remoteIPStr, remotePeerID)

			// 3. Update routing table
			log.Printf("[%s] Handshake Step 3: Updating routing table %s -> %s", handlerID, remoteIPStr, remotePeerID)
			routingTableLock.Lock()
			peerRoutingTable[remoteIPStr] = remotePeerID
			routingTableLock.Unlock()
			log.Printf("[%s] Handshake Step 3 SUCCESS: Updated routing table: %s -> %s", handlerID, remoteIPStr, remotePeerID)

			// --- Handshake Complete ---
			log.Printf("[%s] Handshake COMPLETE for incoming stream from %s. Entering packet read loop.", handlerID, remotePeerID)

			// 4. Now handle packet forwarding from this peer
			buf := make([]byte, 2000) // MTU size buffer
			for {
				// Check context before blocking read
				select {
				case <-handlerHandshakeCtx.Done(): // Reuse handshake context for overall stream lifetime? Maybe not ideal. Let's remove this check for now.
					// log.Printf("[%s] Stream handler context done for peer %s. Exiting read loop.", handlerID, remotePeerID)
					// return
				default:
					// Context is still active, proceed with read
				}

				// Log before blocking on Read
				log.Printf("[%s] Waiting to read next packet from peer %s (Stream ID: %s)...", handlerID, remotePeerID, stream.ID())
				// Set a read deadline for packet reading? Could be useful to detect dead streams.
				// Let's add a longer deadline for regular packet reads.
				packetReadDeadline := time.Now().Add(2 * time.Minute) // Example: 2 minutes idle timeout
				if err := stream.SetReadDeadline(packetReadDeadline); err != nil {
					log.Printf("[%s] Error setting packet read deadline for peer %s: %v. Continuing without deadline.", handlerID, remotePeerID, err)
					// Don't reset the stream here, just log and continue
					stream.SetReadDeadline(time.Time{}) // Attempt to clear deadline
				}

				n, err := stream.Read(buf) // Use the stream directly now (no longer using buffered reader)

				// Clear deadline after read attempt
				if errClear := stream.SetReadDeadline(time.Time{}); errClear != nil {
					log.Printf("[%s] Warning: Failed to clear packet read deadline for peer %s: %v", handlerID, remotePeerID, errClear)
				}

				if err != nil {
					// Check for timeout error specifically
					if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
						log.Printf("[%s] Read timeout from peer %s (Stream ID: %s). Assuming idle, continuing read loop.", handlerID, remotePeerID, stream.ID())
						// Don't return, just continue waiting for the next packet
						continue
					}

					// More specific logging for common errors
					if err == network.ErrReset || strings.Contains(err.Error(), "stream reset") { // Check string for robustness
						log.Printf("[%s] Stream reset by peer %s (Stream ID: %s)", handlerID, remotePeerID, stream.ID())
					} else if err == context.Canceled || err == context.DeadlineExceeded {
						log.Printf("[%s] Stream context error for peer %s (Stream ID: %s): %v", handlerID, remotePeerID, stream.ID(), err)
					} else if err.Error() == "EOF" {
						log.Printf("[%s] Stream closed by peer %s (EOF) (Stream ID: %s)", handlerID, remotePeerID, stream.ID())
					} else {
						log.Printf("[%s] Stream read error from peer %s (Stream ID: %s): %v", handlerID, remotePeerID, stream.ID(), err)
					}
					// Error automatically leads to defer cleanup
					return // Exit goroutine on error
				}

				if n == 0 { // Should not happen with TCP streams, but check anyway
					log.Printf("[%s] Read 0 bytes from peer %s (Stream ID: %s), continuing", handlerID, remotePeerID, stream.ID())
					continue
				}

				// Basic check: Ensure packet has at least an IPv4 header (20 bytes)
				if n < 20 {
					log.Printf("[%s] Received runt packet from %s (Stream ID: %s), size %d, discarding", handlerID, remotePeerID, stream.ID(), n)
					continue
				}

				// Log packet reception from peer before writing to TUN
				destIP := net.IP(buf[16:20]) // Peek at destination IP for logging
				log.Printf("[%s] Read %d bytes from peer %s (Stream ID: %s) for dest %s. Writing to TUN.", handlerID, n, remotePeerID, stream.ID(), destIP.String())

				// Write packet received from peer to the local TUN interface
				_, writeErr := tunIface.Write(buf[:n])
				if writeErr != nil {
					log.Printf("[%s] Error writing %d bytes to TUN from peer %s (Stream ID: %s): %v", handlerID, n, remotePeerID, stream.ID(), writeErr)
					// Consider if TUN write error should close the stream? For now, continue.
				} else {
					// log.Printf("[%s] Successfully wrote %d bytes from peer %s (Stream ID: %s) to TUN", handlerID, n, remotePeerID, stream.ID()) // Optional success log
				}
			}
		}(s) // Pass the stream to the goroutine
	})

	// --- TUN Reading and Packet Forwarding Goroutine ---
	go func() {
		buf := make([]byte, 2000) // MTU size buffer
		for {
			n, err := tunIface.Read(buf)
			if err != nil {
				log.Println("Error reading TUN:", err)
				// Consider if TUN error is fatal; if temporary, continue
				time.Sleep(100 * time.Millisecond) // Avoid busy-looping on TUN errors
				continue
			}
			if n == 0 {
				log.Println("Read 0 bytes from TUN, continuing")
				continue // Skip empty packets
			}

			packetData := make([]byte, n) // Create a copy to avoid race conditions if buf is reused quickly
			copy(packetData, buf[:n])

			// Basic check: Ensure packet has at least an IPv4 header (20 bytes)
			if n < 20 {
				log.Printf("Read runt packet from TUN, size %d, discarding", n)
				continue
			}

			// --- Packet Forwarding Logic ---
			// Parse destination IP (IPv4 header: bytes 16-19)
			destIP := net.IP(packetData[16:20])
			destIPStr := destIP.String()
			log.Printf("Read %d bytes from TUN for dest %s", n, destIPStr) // Log packet read from TUN

			// If destination is self, skip forwarding (kernel handles it)
			if destIPStr == localVPNIP {
				// log.Printf("Packet dest %s is local VPN IP, skipping forwarding", destIPStr) // Optional log
				continue
			}

			var targetPeer peer.ID
			var found bool
			var targetType string // For logging

			// Check if destination is within the VPN subnet
			isVPNSubnetDest := vpnSubnetCIDR.Contains(destIP)

			if isVPNSubnetDest {
				// Destination is within our VPN network
				routingTableLock.RLock()
				targetPeer, found = peerRoutingTable[destIPStr]
				routingTableLock.RUnlock()
				if found {
					targetType = "VPN Peer"
					log.Printf("Routing lookup for VPN dest %s: Found peer %s", destIPStr, targetPeer)
				} else {
					log.Printf("Routing lookup for VPN dest %s: No route found, dropping packet", destIPStr) // More specific log
					continue                                                                                 // No route, drop packet
				}
			} else {
				// Destination is outside the VPN network
				log.Printf("Packet dest %s is outside VPN subnet %s", destIPStr, vpnSubnetCIDR.String())
				if nodeRole == roleUser {
					if exitNodePeerID != "" {
						// User node forwards non-VPN traffic to the exit node
						targetPeer = exitNodePeerID
						found = true
						targetType = "Exit Node"
						log.Printf("Routing non-VPN packet for %s to %s (%s)", destIPStr, targetPeer, targetType)
					} else {
						// User node, but no exit node configured
						log.Printf("Non-VPN destination %s and no exit node configured, dropping packet", destIPStr)
						continue // No exit node, drop packet
					}
				} else if nodeRole == roleExitNode {
					// Exit node: Let the OS handle routing it out the physical interface.
					log.Printf("Exit node letting OS handle non-VPN packet for %s", destIPStr)
					found = false // Prevent forwarding within this loop
					continue      // Let OS handle it
				}
			}

			// Forward if a target (specific peer or exit node) was found
			if found {
				if targetPeer == node.ID() { // Avoid sending to self
					log.Printf("Target peer %s is self, skipping forwarding for %s", targetPeer, destIPStr)
					continue
				}

				// Get or create a persistent stream
				// Use a background context, maybe add timeout later if needed
				log.Printf("Attempting to get/create stream for target %s (%s) for packet to %s", targetPeer, targetType, destIPStr)
				stream := getOrCreateStream(context.Background(), node, targetPeer)
				if stream == nil {
					// Log remains the same, but the error from getOrCreateStream should be more specific now
					log.Printf("Failed to get/create stream for target %s (%s), dropping packet for %s", targetPeer, targetType, destIPStr)
					continue // Cannot establish stream, drop packet
				}

				// Write the actual packet data to the persistent stream
				log.Printf("Forwarding %d bytes for %s to %s (%s) via stream %s", len(packetData), destIPStr, targetPeer, targetType, stream.ID())
				_, err = stream.Write(packetData)
				if err != nil {
					log.Printf("Error writing %d bytes to stream for peer %s (%s) for IP %s: %v. Closing stream.", len(packetData), targetPeer, targetType, destIPStr, err)
					// Assume stream is dead, remove it. getOrCreateStream will make a new one next time.
					removeStream(targetPeer)
					// Don't continue to next packet immediately, let the loop run again
				} else {
					// log.Printf("Successfully forwarded %d bytes for %s to peer %s (%s)", len(packetData), destIPStr, targetPeer, targetType) // Optional success log
					// Do NOT close the stream here. Keep it open for reuse.
				}
			}
			// else: Packet was dropped or handled by OS (exit node local traffic)
		}
	}()

	// Handle interrupts
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
	<-c
	fmt.Println("\nShutting down...")

	// Close all active peer streams first to stop traffic
	log.Println("Closing peer streams...")
	streamLock.Lock()
	for pid, stream := range peerStreams {
		log.Printf("Closing stream to %s", pid)
		stream.Reset() // Force close
	}
	peerStreams = make(map[peer.ID]network.Stream) // Clear the map
	streamLock.Unlock()

	// Cleanup TUN device configuration (IP, routes) before closing the interface
	if tunIface != nil {
		log.Println("Cleaning up TUN device configuration...")
		cleanupTunDevice(tunIface.Name(), *localVPNIPNet, *vpnSubnet) // Call cleanup function
	}

	// Close TUN interface (best effort)
	if tunIface != nil {
		log.Println("Closing TUN interface...")
		tunIface.Close()
	}

	// Close the libp2p node
	log.Println("Closing libp2p node...")
	if err := node.Close(); err != nil {
		log.Printf("Error closing libp2p node: %v", err)
	}

	log.Println("Shutdown complete.")
	// TODO: Remove routes/IP config from TUN? (Might require elevated privileges and OS-specific commands)
}
