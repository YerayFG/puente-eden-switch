// SPDX-License-Identifier: GPL-3.0-or-later
// Puente Eden <-> Switch (ldn_mitm)

package main

// Game data plane: UDP between the Switch (real LAN) and Eden (room ProxyPackets).

import (
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
)

var gameBindHost = "0.0.0.0"

var (
	zEnc, _ = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithSingleSegment(true), zstd.WithEncoderCRC(false), zstd.WithZeroFrames(true))
	zDec, _ = zstd.NewReader(nil, zstd.WithDecoderMaxMemory(64<<20))
)

func zCompress(b []byte) []byte { return zEnc.EncodeAll(b, nil) }
func zDecompress(b []byte) ([]byte, error) {
	if len(b) == 0 {
		return nil, nil
	}
	return zDec.DecodeAll(b, nil)
}

type portSock struct {
	realPort uint16 // port bound on this PC
	edenPort uint16 // port number as seen by Eden (may differ, e.g. 0)
	conn     *net.UDPConn
	lastUse  time.Time
}

type DataPlane struct {
	b      *Bridge
	mu     sync.Mutex
	socks  map[uint16]*portSock // by real port
	byEden map[uint16]*portSock // by eden port
	seen   map[string]bool
	stats  struct {
		toSwitch, toEden uint64
	}
}

func newDataPlane(b *Bridge) *DataPlane {
	return &DataPlane{b: b, socks: map[uint16]*portSock{}, byEden: map[uint16]*portSock{}}
}

func (d *DataPlane) isBound(port uint16) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.socks[port]
	return ok
}

// ensure returns (creating if needed) the socket used for an Eden-side port.
func (d *DataPlane) ensure(edenPort uint16) (*portSock, error) {
	s, _, err := d.ensureNew(edenPort)
	return s, err
}

func (d *DataPlane) ensureNew(edenPort uint16) (*portSock, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if s, ok := d.byEden[edenPort]; ok {
		return s, false, nil
	}
	want := edenPort
	if want == lanPort {
		want = 0
	}
	conn, err := listenUDPSharedAddr(gameBindHost, want)
	if err != nil && want != 0 {
		logf("aviso: no pude usar el puerto UDP %d (%v); uso uno aleatorio", want, err)
		conn, err = listenUDPSharedAddr(gameBindHost, 0)
	}
	if err != nil {
		return nil, false, err
	}
	real := uint16(conn.LocalAddr().(*net.UDPAddr).Port)
	s := &portSock{realPort: real, edenPort: edenPort, conn: conn, lastUse: time.Now()}
	d.socks[real] = s
	d.byEden[edenPort] = s
	if real != edenPort {
		logv("puerto Eden %d -> puerto PC %d", edenPort, real)
	} else {
		logv("escuchando datos de juego en UDP %d", real)
	}
	go d.readLoop(s)
	return s, true, nil
}

func (d *DataPlane) readLoop(s *portSock) {
	buf := make([]byte, 65536)
	for {
		n, from, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			if isClosedErr(err) {
				return
			}
			continue // ICMP port unreachable etc.
		}
		src := netIPv4(from.IP)
		d.noteRx(fmt.Sprintf("socket %d <- %s:%d", s.realPort, src, from.Port), "datos recibidos en el puerto %d desde %s:%d", s.realPort, src, from.Port)
		d.fromLAN(src, uint16(from.Port), s.edenPort, buf[:n])
	}
}

// noteRx logs the first packet of each kind (diagnostics).
func (d *DataPlane) noteRx(key, format string, a ...any) {
	d.mu.Lock()
	if d.seen == nil {
		d.seen = map[string]bool{}
	}
	first := !d.seen[key] && len(d.seen) < 64
	d.seen[key] = true
	d.mu.Unlock()
	if first {
		logf(format, a...)
	}
}

// fromLAN forwards a datagram sent by the Switch to Eden.
func (d *DataPlane) fromLAN(src IPv4, srcPort, edenDstPort uint16, data []byte) {
	b := d.b
	if !b.acceptSwitchSource(src) {
		return
	}
	edenIP, ok := b.edenPeerIP()
	if !ok {
		return
	}
	room := b.currentRoom()
	if room == nil {
		return
	}
	d.mu.Lock()
	d.stats.toEden++
	n := d.stats.toEden
	d.mu.Unlock()
	if n%8 == 1 {
		statusUpdate(func(s *Status) { s.ToEden = n })
	}
	room.SendProxy(ProxyPacket{
		Local:    SockAddr{IP: room.MyIP(), Port: srcPort},
		Remote:   SockAddr{IP: edenIP, Port: edenDstPort},
		Protocol: protoUDP,
		Data:     zCompress(data),
	})
}

// fromRoom forwards a ProxyPacket from Eden to the Switch.
func (d *DataPlane) fromRoom(p ProxyPacket) {
	b := d.b
	if p.Protocol != protoUDP {
		logOnce("tcp", "aviso: el juego usa TCP, que Eden no permite en salas; puede no funcionar")
		return
	}
	room := b.currentRoom()
	if room == nil {
		return
	}
	if !p.Broadcast && p.Remote.IP != room.MyIP() {
		return
	}
	b.noteEdenPeer(p.Local.IP)
	data, err := zDecompress(p.Data)
	if err != nil {
		logv("paquete de Eden ilegible: %v", err)
		return
	}
	s, err := d.ensure(p.Local.Port)
	if err != nil {
		logf("error abriendo puerto UDP: %v", err)
		return
	}
	s.lastUse = time.Now()
	sw, ok := b.switchIP()
	var dst *net.UDPAddr
	if ok {
		dst = &net.UDPAddr{IP: net.IP(sw[:]), Port: int(p.Remote.Port)}
	} else if p.Broadcast {
		bc := b.lanBroadcast()
		dst = &net.UDPAddr{IP: net.IP(bc[:]), Port: int(p.Remote.Port)}
	} else {
		return
	}
	d.mu.Lock()
	d.stats.toSwitch++
	n := d.stats.toSwitch
	d.mu.Unlock()
	if n%8 == 1 {
		statusUpdate(func(s *Status) { s.ToSwitch = n })
	}
	if _, err := s.conn.WriteToUDP(data, dst); err != nil {
		d.noteRx("txerr", "error enviando a la Switch: %v", err)
	}
	d.noteRx(fmt.Sprintf("tx %d>%s:%d", s.realPort, dst.IP, dst.Port), "enviando datos de Eden desde el puerto %d a %s:%d", s.realPort, dst.IP, dst.Port)
}

// sniffed is called for UDP packets seen by the raw sniffer (Windows) that were
// sent by the Switch to this PC on a port we are not listening on yet.
func (d *DataPlane) sniffed(src IPv4, srcPort, dstPort uint16, data []byte) {
	if sw, ok := d.b.switchIP(); ok && src == sw && dstPort != lanPort {
		d.noteRx(fmt.Sprintf("sniff %d>%d", srcPort, dstPort), "detector: la Switch envía datos de %d al puerto %d del PC", srcPort, dstPort)
	}
	if dstPort == lanPort || d.isBound(dstPort) {
		return
	}
	if !d.b.acceptSwitchSource(src) || !d.b.sessionActive() {
		return
	}
	if _, created, err := d.ensureNew(dstPort); err != nil || !created {
		return
	}
	logv("detectado tráfico de la Switch al puerto %d", dstPort)
	d.fromLAN(src, srcPort, dstPort, data)
}

func (d *DataPlane) prebind(ports []uint16) {
	for _, p := range ports {
		if _, err := d.ensure(p); err != nil {
			logf("no pude abrir el puerto %d: %v", p, err)
		}
	}
}

func (d *DataPlane) Stats() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return fmt.Sprintf("paquetes de juego: Switch→Eden %d, Eden→Switch %d, puertos abiertos %d",
		d.stats.toEden, d.stats.toSwitch, len(d.socks))
}

func (d *DataPlane) Counts() (toEden, toSwitch uint64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.stats.toEden, d.stats.toSwitch
}
