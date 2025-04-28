package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec" // Added import
	"os/signal"
	"runtime" // Added import
	"strings" // Added import
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

func main() {
	ctx := context.Background()
	vpnSubnet := "10.0.8.0/24"  // Define the VPN subnet
	localVPNIP := "10.0.8.1/24" // Assign the first IP to this node

	tunIface, err := setupTun()
	if err != nil {
		log.Fatal(err)
	}
	log.Println("TUN device created:", tunIface.Name())

	// Configure the TUN device IP and routes
	if err := configureTunDevice(tunIface.Name(), localVPNIP, vpnSubnet); err != nil { // Pass vpnSubnet
		log.Fatalf("Error configuring TUN device: %v", err)
	}

	node, err := libp2p.New()
	if err != nil {
		log.Fatal(err)
	}

	notifee := &discoveryNotifee{h: node}
	srv := mdns.NewMdnsService(node, discoveryServiceTag, notifee)
	if err := srv.Start(); err != nil {
		log.Fatal(err)
	}

	node.SetStreamHandler("/vpn/1.0.0", func(s network.Stream) {
		go func() {
			defer s.Close()
			buf := make([]byte, 2000)
			for {
				n, err := s.Read(buf)
				if err != nil {
					return
				}
				// TODO: Implement routing logic here.
				// Before writing to TUN, check if the destination IP in the packet
				// belongs to the local node's VPN IP. If not, forward it
				// to the appropriate peer based on a routing table.
				// For now, we assume all traffic is for the local TUN.
				_, err = tunIface.Write(buf[:n])
				if err != nil {
					log.Printf("Error writing to TUN: %v", err)
				}
			}
		}()
	})

	go func() {
		buf := make([]byte, 2000)
		for {
			n, err := tunIface.Read(buf)
			if err != nil {
				log.Println("Error reading TUN:", err)
				continue
			}

			// TODO: Implement routing logic here.
			// Read the destination IP from the packet header (buf[:n]).
			// Determine which peer corresponds to that destination IP.
			// This requires a mechanism for peers to share their assigned VPN IPs.
			// For now, broadcast to all connected peers.

			packetData := buf[:n] // Keep a copy of the packet data

			for _, p := range node.Peerstore().Peers() {
				if p == node.ID() {
					continue
				}
				stream, err := node.NewStream(ctx, p, "/vpn/1.0.0")
				if err != nil {
					log.Println("Stream error:", err)
					continue
				}
				// Write the original packet data
				_, err = stream.Write(packetData)
				if err != nil {
					log.Println("Write error:", err)
					stream.Reset() // Reset stream on write error
				} else {
					stream.CloseWrite() // Close the write side gracefully
				}
				// It's generally better practice to handle stream closure
				// after ensuring data is sent or an error occurred.
				// Closing immediately might cut off transmission.
				// stream.Close() // Removed immediate close
			}
		}
	}()

	// Handle interrupts
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
	<-c
	fmt.Println("Shutting down...")
	node.Close()
}
