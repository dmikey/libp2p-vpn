package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"runtime" // Added import
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

func main() {
	ctx := context.Background()

	tunIface, err := setupTun()
	if err != nil {
		log.Fatal(err)
	}
	log.Println("TUN device set up:", tunIface.Name())

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
				tunIface.Write(buf[:n])
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

			for _, p := range node.Peerstore().Peers() {
				if p == node.ID() {
					continue
				}
				stream, err := node.NewStream(ctx, p, "/vpn/1.0.0")
				if err != nil {
					log.Println("Stream error:", err)
					continue
				}
				_, err = stream.Write(buf[:n])
				if err != nil {
					log.Println("Write error:", err)
				}
				stream.Close()
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
