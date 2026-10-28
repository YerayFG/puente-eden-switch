// SPDX-License-Identifier: GPL-3.0-or-later
// Puente Eden <-> Switch (ldn_mitm)

package main

// ldn_mitm LAN protocol (UDP/TCP port 11452) and Switch LDN structure helpers.

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
)

const (
	lanMagic      = 0x11451400
	lanPort       = 11452
	lanHeaderSize = 12

	netInfoSize  = 0x480
	nodeInfoSize = 0x40
	nodeCountMax = 8

	offBssid     = 0x20
	offNodeCount = 0x67
	offNodes     = 0x68
)

func lanCompress(in []byte) ([]byte, bool) {
	out := make([]byte, 0, len(in))
	i := 0
	for i < len(in) {
		if len(out) >= len(in) {
			return nil, false
		}
		c := in[i]
		i++
		if c == 0 {
			count := 0
			for i < len(in) && in[i] == 0 && count < 0xFF {
				count++
				i++
			}
			out = append(out, 0)
			if len(out) >= len(in) {
				return nil, false
			}
			out = append(out, byte(count))
		} else {
			out = append(out, c)
		}
	}
	return out, true
}

func lanDecompress(in []byte, max int) ([]byte, error) {
	out := make([]byte, 0, max)
	i := 0
	for i < len(in) && len(out) < max {
		c := in[i]
		i++
		out = append(out, c)
		if c == 0 {
			if i == len(in) {
				return nil, errors.New("rle truncado")
			}
			count := int(in[i])
			i++
			for k := 0; k < count && len(out) < max; k++ {
				out = append(out, 0)
			}
		}
	}
	if i != len(in) {
		return nil, errors.New("rle demasiado largo")
	}
	return out, nil
}

// encodeLan builds a LAN packet (header + optionally compressed body).
func encodeLan(typ uint8, body []byte) []byte {
	h := make([]byte, lanHeaderSize)
	binary.LittleEndian.PutUint32(h, lanMagic)
	h[4] = typ
	payload := body
	if len(body) > 0 {
		if c, ok := lanCompress(body); ok {
			h[5] = 1
			binary.LittleEndian.PutUint16(h[8:], uint16(len(body)))
			payload = c
		}
	}
	binary.LittleEndian.PutUint16(h[6:], uint16(len(payload)))
	return append(h, payload...)
}

// decodeLan parses one LAN packet from a datagram.
func decodeLan(d []byte) (typ uint8, body []byte, n int, err error) {
	if len(d) < lanHeaderSize {
		return 0, nil, 0, io.ErrShortBuffer
	}
	if binary.LittleEndian.Uint32(d) != lanMagic {
		return 0, nil, 0, errors.New("magic incorrecto")
	}
	typ = d[4]
	compressed := d[5] != 0
	length := int(binary.LittleEndian.Uint16(d[6:]))
	dlen := int(binary.LittleEndian.Uint16(d[8:]))
	if len(d) < lanHeaderSize+length {
		return 0, nil, 0, io.ErrShortBuffer
	}
	body = d[lanHeaderSize : lanHeaderSize+length]
	if compressed {
		body, err = lanDecompress(body, 2048)
		if err != nil {
			return 0, nil, 0, err
		}
		if len(body) != dlen {
			return 0, nil, 0, errors.New("longitud descomprimida incorrecta")
		}
	} else {
		body = append([]byte(nil), body...)
	}
	return typ, body, lanHeaderSize + length, nil
}

// readLanTCP reads one LAN packet from a TCP stream.
func readLanTCP(c net.Conn) (uint8, []byte, error) {
	h := make([]byte, lanHeaderSize)
	if _, err := io.ReadFull(c, h); err != nil {
		return 0, nil, err
	}
	if binary.LittleEndian.Uint32(h) != lanMagic {
		return 0, nil, errors.New("magic incorrecto en TCP")
	}
	length := int(binary.LittleEndian.Uint16(h[6:]))
	body := make([]byte, length)
	if _, err := io.ReadFull(c, body); err != nil {
		return 0, nil, err
	}
	typ, b, _, err := decodeLan(append(h, body...))
	return typ, b, err
}

// ---- IP helpers. In LDN structures, IPv4 is a host-order u32 stored little endian.

func ipToU32(ip IPv4) uint32     { return binary.BigEndian.Uint32(ip[:]) }
func u32ToIP(v uint32) (ip IPv4) { binary.BigEndian.PutUint32(ip[:], v); return }

func netIPv4(ip net.IP) (out IPv4) {
	copy(out[:], ip.To4())
	return
}

// macForSwitchSide mimics ldn_mitm's fake mac: 02:00 + memcpy(host-order u32) on little endian.
func macForSwitchSide(ip IPv4) [6]byte {
	return [6]byte{0x02, 0x00, ip[3], ip[2], ip[1], ip[0]}
}

// macForEdenSide mimics Eden's fake mac: 02:00 + IPv4 bytes in network order.
func macForEdenSide(ip IPv4) [6]byte {
	return [6]byte{0x02, 0x00, ip[0], ip[1], ip[2], ip[3]}
}

type ipMapper func(IPv4) (IPv4, bool)

// rewriteNode rewrites ipv4Address + mac of a NodeInfo (in place).
func rewriteNode(node []byte, m ipMapper, mac func(IPv4) [6]byte) {
	old := u32ToIP(binary.LittleEndian.Uint32(node[0:]))
	if old == (IPv4{}) {
		return
	}
	if n, ok := m(old); ok && n != old {
		binary.LittleEndian.PutUint32(node[0:], ipToU32(n))
		mm := mac(n)
		copy(node[4:10], mm[:])
	}
}

func nodeIP(node []byte) IPv4 { return u32ToIP(binary.LittleEndian.Uint32(node[0:])) }

// rewriteNetworkInfo returns a rewritten copy of a NetworkInfo.
func rewriteNetworkInfo(info []byte, m ipMapper, mac func(IPv4) [6]byte) []byte {
	out := append([]byte(nil), info...)
	if len(out) != netInfoSize {
		return out
	}
	oldHost := nodeIP(out[offNodes:])
	for i := 0; i < nodeCountMax; i++ {
		rewriteNode(out[offNodes+i*nodeInfoSize:offNodes+(i+1)*nodeInfoSize], m, mac)
	}
	// bssid follows the host (node 0)
	host := nodeIP(out[offNodes:])
	if host != (IPv4{}) && host != oldHost {
		mm := mac(host)
		copy(out[offBssid:offBssid+6], mm[:])
	}
	return out
}

func netInfoHostIP(info []byte) IPv4 {
	if len(info) != netInfoSize {
		return IPv4{}
	}
	return nodeIP(info[offNodes:])
}

func nodeUserName(node []byte) string {
	b := node[0x0C : 0x0C+33]
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

func netInfoSummary(info []byte) (lcid uint64, nodes int, maxNodes int, host string) {
	if len(info) != netInfoSize {
		return
	}
	lcid = binary.LittleEndian.Uint64(info[0:])
	nodes = int(info[offNodeCount])
	maxNodes = int(info[offNodeCount-1])
	host = nodeUserName(info[offNodes:])
	return
}
