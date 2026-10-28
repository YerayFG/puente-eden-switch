// SPDX-License-Identifier: GPL-3.0-or-later
// Puente Eden <-> Switch (ldn_mitm)

//go:build !windows

package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"syscall"
	"time"
)

func listenUDPShared(port uint16) (*net.UDPConn, error) { return listenUDPSharedAddr("0.0.0.0", port) }

func listenUDPSharedAddr(host string, port uint16) (*net.UDPConn, error) {
	lc := net.ListenConfig{Control: func(network, address string, c syscall.RawConn) error {
		var serr error
		c.Control(func(fd uintptr) {
			serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_BROADCAST, 1)
			syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
		})
		return serr
	}}
	pc, err := lc.ListenPacket(context.Background(), "udp4", fmt.Sprintf("%s:%d", host, port))
	if err != nil {
		return nil, err
	}
	return pc.(*net.UDPConn), nil
}

func needElevation() bool    { return false }
func relaunchElevated() bool { return false }
func setupFirewall()         {}
func isAdmin() bool          { return true }

// startSniffer uses a raw UDP socket (needs root) to see all incoming UDP.
func startSniffer(lanIP IPv4, cb func(src IPv4, sport, dport uint16, data []byte)) error {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_RAW, syscall.IPPROTO_UDP)
	if err != nil {
		return err
	}
	go func() {
		buf := make([]byte, 65536)
		for {
			n, _, err := syscall.Recvfrom(fd, buf, 0)
			if err != nil {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			p := buf[:n]
			if rawTap != nil {
				rawTap(p)
			}
			if len(p) < 28 || p[0]>>4 != 4 {
				continue
			}
			ihl := int(p[0]&0x0F) * 4
			if len(p) < ihl+8 || binary.BigEndian.Uint16(p[6:])&0x1FFF != 0 {
				continue
			}
			var src, dst IPv4
			copy(src[:], p[12:16])
			copy(dst[:], p[16:20])
			if src == lanIP || (dst != lanIP && dst[3] != 255) {
				continue
			}
			u := p[ihl:]
			ulen := int(binary.BigEndian.Uint16(u[4:]))
			if ulen < 8 || ulen > len(u) {
				continue
			}
			cb(src, binary.BigEndian.Uint16(u[0:]), binary.BigEndian.Uint16(u[2:]), append([]byte(nil), u[8:ulen]...))
		}
	}()
	return nil
}

func firewallStatus() string  { return "unknown" }
func fixFirewallElevated()    {}
func runElevated(string) bool { return false }

func openAppWindow(url string) <-chan struct{} {
	logf("Abre %s en el navegador", url)
	return nil
}

func closeAppWindow() {}

func showConsole() {}
