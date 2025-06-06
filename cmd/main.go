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
	"strconv" // Added for parsing user input
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

	// Check if this node is a user node and interactive selection is needed
	isUserRole := nodeRole == roleUser
	needsInteractiveSelection := isUserRole && exitNodePeerID == "" && !exitNodePreconfigured // Check if exit node was preconfigured

	if needsInteractiveSelection {
		// Add to list for potential selection later
		discoveredPeersLock.Lock()
		if _, exists := discoveredPeers[pi.ID]; !exists {
			log.Printf("Adding potential exit node %s to selection list", pi.ID.String())
			discoveredPeers[pi.ID] = pi
		}
		discoveredPeersLock.Unlock()
		// Do not attempt connection here, wait for user selection
		return
	}

	// --- Existing Logic for Pre-configured Exit Node or Non-User Roles ---

	// Check if the found peer is the *pre-configured* exit node
	isConfiguredExitNode := exitNodePreconfigured && pi.ID == exitNodePeerID
	if isConfiguredExitNode {
		log.Printf(">>> Discovered configured Exit Node: %s", pi.ID.String())
	}

	// Connect logic (only if not interactive selection OR if it's the pre-configured exit node)
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
			// If it's the pre-configured exit node, try to establish stream proactively
			if isConfiguredExitNode {
				log.Printf(">>> Successfully connected to Configured Exit Node: %s", pi.ID.String())
				// --- Proactively establish VPN stream after successful connection ---
				log.Printf("Attempting to proactively establish VPN stream with configured exit node %s", pi.ID.String())
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				stream := getOrCreateStream(ctx, n.h, pi.ID)
				if stream != nil {
					log.Printf("Successfully established proactive VPN stream with configured exit node %s (Stream ID: %s)", pi.ID.String(), stream.ID())
				} else {
					log.Printf("Failed to establish proactive VPN stream with configured exit node %s (will retry on demand)", pi.ID.String())
				}
				cancel() // Cancel context after use
				// --- End proactive stream establishment ---
			}
		}
	} else if isConfiguredExitNode { // Only check stream if it's the configured exit node and already connected
		// --- Check if VPN stream exists for the configured exit node, if not, try to establish it ---
		streamLock.Lock()
		_, streamExists := peerStreams[pi.ID]
		streamLock.Unlock()

		if !streamExists {
			log.Printf("Configured Exit Node %s connected, but no active VPN stream found. Attempting proactive stream establishment.", pi.ID.String())
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			stream := getOrCreateStream(ctx, n.h, pi.ID)
			if stream != nil {
				log.Printf("Successfully established proactive VPN stream with configured exit node %s (Stream ID: %s) on re-discovery.", pi.ID.String(), stream.ID())
			} else {
				log.Printf("Failed to establish proactive VPN stream with configured exit node %s on re-discovery (will retry on demand).", pi.ID.String())
			}
			cancel() // Cancel context after use
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

	// For interactive exit node selection
	discoveredPeers        = make(map[peer.ID]peer.AddrInfo)
	discoveredPeersLock    sync.RWMutex
	exitNodeSelectedChan   = make(chan struct{}) // Closed when selection is done
	exitNodePreconfigured  = false               // Flag to track if -exitnode was used
	interactiveSelectionWG sync.WaitGroup        // To wait for selection goroutine
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

// promptUserForExitNode displays discovered peers and prompts the user to select one.
func promptUserForExitNode(node host.Host) {
	defer interactiveSelectionWG.Done() // Signal that this goroutine has finished

	log.Println("Waiting for 10 seconds to discover potential exit nodes...")
	time.Sleep(10 * time.Second)

	discoveredPeersLock.RLock()
	if len(discoveredPeers) == 0 {
		log.Println("No potential exit nodes discovered after 10 seconds.")
		discoveredPeersLock.RUnlock()
		close(exitNodeSelectedChan) // Signal that selection process is over (even if none selected)
		return
	}

	fmt.Println("\n--- Potential Exit Nodes ---")
	peerList := make([]peer.AddrInfo, 0, len(discoveredPeers))
	i := 0
	for _, pi := range discoveredPeers {
		// Filter out self
		if pi.ID == node.ID() {
			continue
		}
		fmt.Printf("[%d] %s\n", i, pi.ID.String())
		peerList = append(peerList, pi)
		i++
	}
	discoveredPeersLock.RUnlock() // Unlock before blocking on input

	if len(peerList) == 0 {
		log.Println("No other peers discovered to select as exit node.")
		close(exitNodeSelectedChan)
		return
	}

	fmt.Print("Select exit node by number (or press Enter to skip): ")
	reader := bufio.NewReader(os.Stdin)
	input, _ := reader.ReadString('\n')
	input = strings.TrimSpace(input)

	if input == "" {
		log.Println("Skipping exit node selection.")
		close(exitNodeSelectedChan)
		return
	}

	choice, err := strconv.Atoi(input)
	if err != nil || choice < 0 || choice >= len(peerList) {
		log.Printf("Invalid selection: %s. No exit node selected.", input)
		close(exitNodeSelectedChan)
		return
	}

	selectedPeerInfo := peerList[choice]
	exitNodePeerID = selectedPeerInfo.ID // Set the global variable
	log.Printf(">>> User selected %s as the exit node.", exitNodePeerID.String())

	// Signal that selection is complete *before* attempting connection
	close(exitNodeSelectedChan)

	// Attempt to connect and establish stream proactively
	log.Printf("Attempting to connect and establish stream with selected exit node %s...", exitNodePeerID)
	// Add addresses to peerstore if not already there (discovery might not have connected yet)
	node.Peerstore().AddAddrs(selectedPeerInfo.ID, selectedPeerInfo.Addrs, time.Hour)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second) // Longer timeout for initial connection + stream
	defer cancel()
	stream := getOrCreateStream(ctx, node, exitNodePeerID)
	if stream != nil {
		log.Printf("Successfully established proactive VPN stream with selected exit node %s (Stream ID: %s)", exitNodePeerID, stream.ID())
	} else {
		log.Printf("Failed to establish proactive VPN stream with selected exit node %s (will retry on demand)", exitNodePeerID)
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

	// Parse Exit Node Peer ID if provided
	if *exitNodeStr != "" {
		var err error
		exitNodePeerID, err = peer.Decode(*exitNodeStr)
		if err != nil {
			log.Fatalf("Invalid exit node Peer ID: %v", err)
		}
		log.Printf("Using pre-configured exit node: %s", exitNodePeerID.String())
		exitNodePreconfigured = true // Mark that it was set via flag
	} else if nodeRole == roleUser {
		log.Println("No exit node pre-configured. Will prompt for selection after discovery.")
		// Don't close exitNodeSelectedChan here, wait for prompt function
	} else {
		// Exit node role or user role with no exit node needed/wanted
		close(exitNodeSelectedChan) // Signal immediately if no selection is needed
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

	// --- Interactive Exit Node Selection (if applicable) ---
	if nodeRole == roleUser && !exitNodePreconfigured {
		interactiveSelectionWG.Add(1)
		go promptUserForExitNode(node) // Start selection in background
		// We don't necessarily need to wait here using interactiveSelectionWG.Wait()
		// The TUN reader loop will function correctly even before selection.
		// However, waiting ensures the prompt appears before potential traffic flow logs.
		// Let's wait for the selection process *goroutine* to finish,
		// but the actual selection might be skipped by the user.
		// The exitNodeSelectedChan is used later if we need to ensure selection happened.
		log.Println("User node started, waiting for exit node selection prompt...")
	} else {
		log.Println("Proceeding without interactive exit node selection.")
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
				// log.Printf("[%s] Waiting to read next packet from peer %s (Stream ID: %s)...", handlerID, remotePeerID, stream.ID()) // Reduced verbosity
				// ... read deadline setting ...

				n, _ := stream.Read(buf) // Use the stream directly now (no longer using buffered reader)

				// ... read deadline clearing ...
				// ... error handling for stream read ...

				if n == 0 { // Should not happen with TCP streams, but check anyway
					// log.Printf("[%s] Read 0 bytes from peer %s (Stream ID: %s), continuing", handlerID, remotePeerID, stream.ID()) // Reduced verbosity
					continue
				}

				// Basic check: Ensure packet has at least an IPv4 header (20 bytes)
				if n < 20 {
					log.Printf("[%s] Received runt packet from %s (Stream ID: %s), size %d, discarding", handlerID, remotePeerID, stream.ID(), n)
					continue
				}

				// Extract source and destination IPs for logging
				sourceIP := net.IP(buf[12:16])
				destIP := net.IP(buf[16:20])
				log.Printf("[%s] Read %d bytes from peer %s (Stream ID: %s) | PKT: %s -> %s. Writing to TUN.", handlerID, n, remotePeerID, stream.ID(), sourceIP, destIP) // Enhanced log

				// Write packet received from peer to the local TUN interface
				_, writeErr := tunIface.Write(buf[:n])
				if writeErr != nil {
					log.Printf("[%s] Error writing %d bytes (PKT: %s -> %s) to TUN from peer %s (Stream ID: %s): %v", handlerID, n, sourceIP, destIP, remotePeerID, stream.ID(), writeErr) // Enhanced log
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
			// Line 592 (approximately) where 'err' is declared:
			n, err := tunIface.Read(buf)
			// Immediately check the error:
			if err != nil {
				log.Println("Error reading TUN:", err)
				// Consider if TUN error is fatal; if temporary, continue
				time.Sleep(100 * time.Millisecond) // Avoid busy-looping on TUN errors
				continue
			}
			// Check for 0 bytes read *after* checking for error:
			if n == 0 {
				// log.Println("Read 0 bytes from TUN, continuing") // Reduced verbosity
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
			// Parse source and destination IP (IPv4 header: bytes 12-15 for src, 16-19 for dest)
			sourceIP := net.IP(packetData[12:16])
			destIP := net.IP(packetData[16:20])
			sourceIPStr := sourceIP.String()
			destIPStr := destIP.String()
			log.Printf("Read %d bytes from TUN | PKT: %s -> %s", n, sourceIPStr, destIPStr) // Enhanced log

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
				targetType = "VPN Peer"
				routingTableLock.RLock()
				targetPeer, found = peerRoutingTable[destIPStr]
				routingTableLock.RUnlock()
				if found {
					log.Printf("Routing lookup for VPN dest %s (from %s): Found %s %s", destIPStr, sourceIPStr, targetType, targetPeer) // Enhanced log
				} else {
					log.Printf("Routing lookup for VPN dest %s (from %s): No route found, dropping packet", destIPStr, sourceIPStr) // Enhanced log
					continue                                                                                                        // No route, drop packet
				}
			} else {
				// Destination is outside the VPN network
				// log.Printf("Packet dest %s (from %s) is outside VPN subnet %s", destIPStr, sourceIPStr, vpnSubnetCIDR.String()) // Optional log
				if nodeRole == roleUser {
					// --- Check if exit node has been selected ---
					// We read the global exitNodePeerID here. If it's empty,
					// non-VPN packets will be dropped. If it's set (either
					// pre-configured or selected by user), we use it.
					currentExitNodeID := exitNodePeerID // Read volatile variable once per packet check
					if currentExitNodeID != "" {
						targetPeer = currentExitNodeID
						found = true
						targetType = "Exit Node"
						// log.Printf("Routing non-VPN packet (from %s) for %s to %s (%s)", sourceIPStr, destIPStr, targetPeer, targetType) // Reduced verbosity
					} else {
						// User node, but no exit node configured OR selection not yet made/skipped
						// log.Printf("Non-VPN destination %s (from %s) and no exit node available, dropping packet", destIPStr, sourceIPStr) // Reduced verbosity
						found = false // Ensure packet is dropped
						continue
					}
				} else if nodeRole == roleExitNode {
					// Exit node: Let the OS handle routing it out the physical interface.
					log.Printf("Exit node letting OS handle non-VPN packet: %s -> %s", sourceIPStr, destIPStr) // Enhanced log
					found = false                                                                              // Prevent forwarding within this loop
					continue                                                                                   // Let OS handle it
				}
			}

			// Forward if a target (specific peer or exit node) was found
			if found {
				if targetPeer == node.ID() { // Avoid sending to self
					log.Printf("Target peer %s is self, skipping forwarding for packet: %s -> %s", targetPeer, sourceIPStr, destIPStr) // Enhanced log
					continue
				}

				// Get or create a persistent stream
				// Use a background context, maybe add timeout later if needed
				// log.Printf("Attempting to get/create stream for target %s (%s) for packet: %s -> %s", targetPeer, targetType, sourceIPStr, destIPStr) // Optional log
				stream := getOrCreateStream(context.Background(), node, targetPeer)
				if stream == nil {
					log.Printf("Failed to get/create stream for target %s (%s), dropping packet: %s -> %s", targetPeer, targetType, sourceIPStr, destIPStr) // Enhanced log
					continue                                                                                                                                // Cannot establish stream, drop packet
				}

				// Write the actual packet data to the persistent stream
				log.Printf("Forwarding %d bytes (PKT: %s -> %s) to %s (%s) via stream %s", len(packetData), sourceIPStr, destIPStr, targetPeer, targetType, stream.ID()) // Enhanced log
				_, err = stream.Write(packetData)
				if err != nil {
					log.Printf("Error writing %d bytes (PKT: %s -> %s) to stream for peer %s (%s): %v. Closing stream.", len(packetData), sourceIPStr, destIPStr, targetPeer, targetType, err) // Enhanced log
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

	// Wait for the selection goroutine to finish if it was started
	if nodeRole == roleUser && !exitNodePreconfigured {
		log.Println("Waiting for selection goroutine to complete...")
		interactiveSelectionWG.Wait()
	}

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
