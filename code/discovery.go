package main

// Zero-config LAN discovery: the client broadcasts a magic string on UDP port+2, every
// hub on the subnet answers with its chat port (and whether it speaks TLS). Nothing
// secret travels here, and it should stay firewalled off on a public hub.

import (
	"fmt"
	"net"
	"strings"
	"time"
)

const discoverMagic = "BACKCHANNEL?"

func (h *hub) serveDiscovery(port int) {
	pc, err := net.ListenPacket("udp4", fmt.Sprintf(":%d", port))
	if err != nil {
		return
	}
	buf := make([]byte, 64)
	for {
		n, addr, err := pc.ReadFrom(buf)
		if err != nil {
			continue
		}
		if string(buf[:n]) == discoverMagic {
			flag := ""
			if h.tlsCfg != nil {
				flag = " tls"
			}
			pc.WriteTo([]byte(fmt.Sprintf("BACKCHANNEL %d%s", h.tcpPort, flag)), addr)
		}
	}
}

// discover broadcasts on the LAN and returns "ip:port" (or "tls://ip:port") of hubs that answer.
func discover(timeoutMs int) []string {
	pc, err := net.ListenPacket("udp4", ":0")
	if err != nil {
		return nil
	}
	defer pc.Close()
	for _, p := range []int{7779} {
		pc.WriteTo([]byte(discoverMagic), &net.UDPAddr{IP: net.IPv4bcast, Port: p})
		for _, bc := range broadcastAddrs() { // some Wi-Fi drops 255.255.255.255
			pc.WriteTo([]byte(discoverMagic), &net.UDPAddr{IP: bc, Port: p})
		}
	}
	pc.SetReadDeadline(time.Now().Add(time.Duration(timeoutMs) * time.Millisecond))
	seen := map[string]bool{}
	var out []string
	buf := make([]byte, 64)
	for {
		n, addr, err := pc.ReadFrom(buf)
		if err != nil {
			break
		}
		var port int
		if _, err := fmt.Sscanf(string(buf[:n]), "BACKCHANNEL %d", &port); err == nil {
			host := addr.(*net.UDPAddr).IP.String()
			s := fmt.Sprintf("%s:%d", host, port)
			if strings.HasSuffix(string(buf[:n]), " tls") {
				s = "tls://" + s
			}
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	return out
}

func localIPs() []string {
	var out []string
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLoopback() && !ipn.IP.IsLinkLocalUnicast() {
			out = append(out, ipn.IP.String()) // skip 169.254.x (virtual adapters, no route to anyone)
		}
	}
	return out
}

func broadcastAddrs() []net.IP {
	var out []net.IP
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok || ipn.IP.To4() == nil || ipn.IP.IsLoopback() {
			continue
		}
		ip := ipn.IP.To4()
		mask := ipn.Mask
		if len(mask) == 16 {
			mask = mask[12:]
		}
		bc := make(net.IP, 4)
		for i := 0; i < 4; i++ {
			bc[i] = ip[i] | ^mask[i]
		}
		out = append(out, bc)
	}
	return out
}
