package main

import (
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
	log.Println("Found peer:", pi.ID.String())
	if len(n.h.Peerstore().Addrs(pi.ID)) == 0 {
		n.h.Connect(context.Background(), pi)
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

// Global routing table and exit node info
var (
	peerRoutingTable = make(map[string]peer.ID)
	routingTableLock sync.RWMutex
	exitNodePeerID   peer.ID
	localVPNIP       string
)

func main() {
	// Define command-line flags
	discoveryTag := flag.String("tag", "p2p-vpn-example", "Unique discovery tag for the VPN service")
	vpnSubnet := flag.String("subnet", "10.0.8.0/24", "Subnet for the VPN network (e.g., 10.0.8.0/24)")
	localVPNIPNet := flag.String("ip", "10.0.8.1/24", "Local IP address for this node within the VPN subnet (e.g., 10.0.8.1/24)")
	exitNodeStr := flag.String("exitnode", "", "Optional Peer ID of the exit node for non-VPN traffic") // Added exit node flag

	flag.Parse() // Parse the flags

	// Extract local IP from CIDR
	ip, _, err := net.ParseCIDR(*localVPNIPNet)
	if err != nil {
		log.Fatalf("Invalid local VPN IP format: %v", err)
	}
	localVPNIP = ip.String() // Store the local IP

	// Parse Exit Node Peer ID if provided
	if *exitNodeStr != "" {
		var err error
		exitNodePeerID, err = peer.Decode(*exitNodeStr)
		if err != nil {
			log.Fatalf("Invalid exit node Peer ID: %v", err)
		}
		log.Printf("Using exit node: %s", exitNodePeerID.String())
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

	notifee := &discoveryNotifee{h: node}
	// Use flag value for discovery tag
	srv := mdns.NewMdnsService(node, *discoveryTag, notifee)
	if err := srv.Start(); err != nil {
		log.Fatal(err)
	}

	node.SetStreamHandler("/vpn/1.0.0", func(s network.Stream) {
		// TODO: Implement a mechanism for peers to announce their VPN IP
		// when a connection/stream is established. This is needed to populate
		// the peerRoutingTable.
		// Example: Read the first message, expect it to be the peer's VPN IP,
		// then add it to the table.
		// routingTableLock.Lock()
		// peerRoutingTable[remotePeerVPNIP] = s.Conn().RemotePeer()
		// routingTableLock.Unlock()

		go func() {
			defer s.Close()
			buf := make([]byte, 2000) // MTU size buffer
			for {
				n, err := s.Read(buf)
				if err != nil {
					// Log read errors, e.g., stream closed, reset
					// log.Printf("Error reading from stream peer %s: %v", s.Conn().RemotePeer(), err)
					return // Exit goroutine on error
				}

				// Basic check: Ensure packet has at least an IPv4 header (20 bytes)
				if n < 20 {
					log.Printf("Received runt packet from %s, size %d, discarding", s.Conn().RemotePeer(), n)
					continue
				}

				// Optimization: Check destination IP. If it's not localVPNIP,
				// this node might be acting as a relay. For now, we assume
				// packets arriving via stream are destined for the local TUN.
				// More complex routing (peer -> peer via relay) is not handled here.

				// Write packet received from peer to the local TUN interface
				_, err = tunIface.Write(buf[:n])
				if err != nil {
					log.Printf("Error writing to TUN from peer %s: %v", s.Conn().RemotePeer(), err)
					// Decide if we should continue or return based on the error
				}
			}
		}()
	})

	go func() {
		buf := make([]byte, 2000) // MTU size buffer
		for {
			n, err := tunIface.Read(buf)
			if err != nil {
				log.Println("Error reading TUN:", err)
				continue // Continue reading after error
			}
			if n == 0 {
				continue // Skip empty packets
			}

			packetData := buf[:n]

			// Basic check: Ensure packet has at least an IPv4 header (20 bytes)
			if n < 20 {
				log.Printf("Read runt packet from TUN, size %d, discarding", n)
				continue
			}

			// --- Packet Forwarding Logic ---
			// Parse destination IP (IPv4 header: bytes 16-19)
			destIP := net.IP(packetData[16:20])
			destIPStr := destIP.String()

			// If destination is self, skip forwarding (kernel handles it)
			if destIPStr == localVPNIP {
				// log.Printf("Packet destined for local node %s, skipping forwarding", localVPNIP)
				continue
			}

			var targetPeer peer.ID
			var found bool

			// 1. Check routing table for specific peer
			routingTableLock.RLock()
			targetPeer, found = peerRoutingTable[destIPStr]
			routingTableLock.RUnlock()

			if found {
				// log.Printf("Forwarding packet to specific peer %s for IP %s", targetPeer, destIPStr)
			} else if exitNodePeerID != "" {
				// 2. If not found, check if an exit node is configured
				targetPeer = exitNodePeerID
				found = true // Mark as found to proceed with forwarding
				// log.Printf("Forwarding packet for IP %s to exit node %s", destIPStr, targetPeer)
			}

			// 3. Forward if a target (specific peer or exit node) was found
			if found {
				if targetPeer == node.ID() { // Avoid sending to self
					continue
				}
				stream, err := node.NewStream(context.Background(), targetPeer, "/vpn/1.0.0")
				if err != nil {
					log.Printf("Failed to open stream to target peer %s for IP %s: %v", targetPeer, destIPStr, err)
					continue // Try next packet
				}

				_, err = stream.Write(packetData)
				if err != nil {
					log.Printf("Error writing packet to peer %s for IP %s: %v", targetPeer, destIPStr, err)
					stream.Reset()
				} else {
					stream.CloseWrite() // Gracefully close write side
				}
				// stream.Close() // Close might be too soon, CloseWrite is better
			} else {
				// 4. Destination unknown and no exit node
				log.Printf("No route for destination IP %s, dropping packet", destIPStr)
			}

			// --- End Packet Forwarding Logic ---

			// Remove the old broadcast logic:
			/*
				for _, p := range node.Peerstore().Peers() {
					if p == node.ID() {
						continue
					}
					// ... old stream opening and writing ...
			*/
		}
	}()

	// Handle interrupts
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
	<-c
	fmt.Println("Shutting down...")
	node.Close()
}
