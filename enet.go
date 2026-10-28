// SPDX-License-Identifier: GPL-3.0-or-later
// Puente Eden <-> Switch (ldn_mitm)

package main

// Minimal ENet 1.3 implementation (client and server) — enough to talk to the
// Eden/yuzu multiplayer room protocol: connect, reliable send/receive on
// channel 0 (with fragmentation), acknowledgements, retransmission, ping and
// disconnect.

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	mrand "math/rand"
	"net"
	"os"
	"strconv"
	"sync"
	"time"
)

const (
	enetCmdAck            = 1
	enetCmdConnect        = 2
	enetCmdVerifyConnect  = 3
	enetCmdDisconnect     = 4
	enetCmdPing           = 5
	enetCmdSendReliable   = 6
	enetCmdSendUnreliable = 7
	enetCmdSendFragment   = 8
	enetCmdSendUnseq      = 9
	enetCmdBandwidthLimit = 10
	enetCmdThrottleConfig = 11
	enetCmdSendUnrelFrag  = 12

	enetFlagAck      = 0x80
	enetHdrSentTime  = 0x8000
	enetHdrCompress  = 0x4000
	enetHdrSessMask  = 0x3000
	enetHdrSessShift = 12
	enetMaxPeerID    = 0xFFF

	enetDefaultMTU = 1392
)

var enetCmdSizes = [13]int{0, 8, 48, 44, 8, 4, 6, 8, 24, 8, 12, 16, 24}

type enetOut struct {
	ch       uint8
	seq      uint16
	raw      []byte
	firstTx  time.Time
	lastTx   time.Time
	rto      time.Duration
	attempts int
}

type enetFrag struct {
	count uint32
	total uint32
	got   map[uint32]bool
	data  []byte
}

type enetIn struct {
	isFrag   bool
	data     []byte
	startSeq uint16
}

// enetLink is the per-connection reliable state shared by client and server.
type enetLink struct {
	write func([]byte)
	start time.Time

	outgoingPeerID uint16
	outSession     uint8
	inSession      uint8
	mtu            uint32

	sysSeq   uint16
	outSeq   uint16
	inSeq    uint16
	pending  map[uint32]*enetOut
	received map[uint16]*enetIn
	frags    map[uint16]*enetFrag

	lastSend time.Time
	lastRecv time.Time
	lastDbg  time.Time
}

func newLink(write func([]byte)) *enetLink {
	return &enetLink{
		write:          write,
		start:          time.Now(),
		outgoingPeerID: enetMaxPeerID,
		mtu:            enetDefaultMTU,
		pending:        map[uint32]*enetOut{},
		received:       map[uint16]*enetIn{},
		frags:          map[uint16]*enetFrag{},
		lastRecv:       time.Now(),
	}
}

func key(ch uint8, seq uint16) uint32 { return uint32(ch)<<16 | uint32(seq) }

func (l *enetLink) now16() uint16 { return uint16(time.Since(l.start).Milliseconds()) }

func (l *enetLink) writeCmds(cmds ...[]byte) {
	pid := l.outgoingPeerID
	if l.outgoingPeerID < enetMaxPeerID {
		pid |= uint16(l.outSession) << enetHdrSessShift
	}
	pid |= enetHdrSentTime
	pkt := make([]byte, 4, 64)
	binary.BigEndian.PutUint16(pkt, pid)
	binary.BigEndian.PutUint16(pkt[2:], l.now16())
	for _, c := range cmds {
		pkt = append(pkt, c...)
	}
	l.lastSend = time.Now()
	if testDrop() {
		return
	}
	l.write(pkt)
}

func (l *enetLink) queueReliable(ch uint8, seq uint16, raw []byte) {
	now := time.Now()
	l.pending[key(ch, seq)] = &enetOut{ch: ch, seq: seq, raw: raw, firstTx: now, lastTx: now, rto: 250 * time.Millisecond, attempts: 1}
	l.writeCmds(raw)
}

func (l *enetLink) sysCmd(cmdNum uint8, size int) []byte {
	l.sysSeq++
	cmd := make([]byte, size)
	cmd[0] = cmdNum | enetFlagAck
	cmd[1] = 0xFF
	binary.BigEndian.PutUint16(cmd[2:], l.sysSeq)
	return cmd
}

func (l *enetLink) send(data []byte) {
	fragLen := int(l.mtu) - 4 - 24
	if len(data) <= fragLen {
		l.outSeq++
		cmd := make([]byte, 6+len(data))
		cmd[0] = enetCmdSendReliable | enetFlagAck
		binary.BigEndian.PutUint16(cmd[2:], l.outSeq)
		binary.BigEndian.PutUint16(cmd[4:], uint16(len(data)))
		copy(cmd[6:], data)
		l.queueReliable(0, l.outSeq, cmd)
		return
	}
	count := (len(data) + fragLen - 1) / fragLen
	startSeq := l.outSeq + 1
	for i := 0; i < count; i++ {
		off := i * fragLen
		end := min(off+fragLen, len(data))
		l.outSeq++
		cmd := make([]byte, 24+end-off)
		cmd[0] = enetCmdSendFragment | enetFlagAck
		binary.BigEndian.PutUint16(cmd[2:], l.outSeq)
		binary.BigEndian.PutUint16(cmd[4:], startSeq)
		binary.BigEndian.PutUint16(cmd[6:], uint16(end-off))
		binary.BigEndian.PutUint32(cmd[8:], uint32(count))
		binary.BigEndian.PutUint32(cmd[12:], uint32(i))
		binary.BigEndian.PutUint32(cmd[16:], uint32(len(data)))
		binary.BigEndian.PutUint32(cmd[20:], uint32(off))
		copy(cmd[24:], data[off:end])
		l.queueReliable(0, l.outSeq, cmd)
	}
}

// tick retransmits and pings. Returns an error if the connection timed out.
func (l *enetLink) tick(now time.Time, connected bool) error {
	for _, o := range l.pending {
		if now.Sub(o.firstTx) > 30*time.Second {
			return errors.New("tiempo de espera agotado")
		}
		if now.Sub(o.lastTx) >= o.rto {
			o.lastTx = now
			o.attempts++
			if o.rto < time.Second {
				o.rto *= 2
			}
			l.writeCmds(o.raw)
		}
	}
	if connected && now.Sub(l.lastSend) > time.Second {
		c := l.sysCmd(enetCmdPing, 4)
		l.queueReliable(0xFF, l.sysSeq, c)
	}
	if now.Sub(l.lastRecv) > 30*time.Second {
		return errors.New("sin respuesta")
	}
	if debugENet && now.Sub(l.lastDbg) > time.Second {
		l.lastDbg = now
		logf("[enet] pending=%d outSeq=%d inSeq=%d buffered=%d", len(l.pending), l.outSeq, l.inSeq, len(l.received))
	}
	return nil
}

type enetEvent struct {
	connect    []byte // CONNECT command (server side)
	verify     []byte // VERIFY_CONNECT command (client side)
	disconnect bool
	acked      []uint32
}

// process handles one datagram's commands (header already stripped).
func (l *enetLink) process(d []byte, hasSent bool, sentTime uint16) (deliver [][]byte, ev enetEvent) {
	l.lastRecv = time.Now()
	var acks [][]byte
	off := 0
	for off+4 <= len(d) {
		cb := d[off]
		num := int(cb & 0x0F)
		if num == 0 || num >= len(enetCmdSizes) {
			break
		}
		sz := enetCmdSizes[num]
		if off+sz > len(d) {
			break
		}
		cmd := d[off : off+sz]
		ch := cmd[1]
		seq := binary.BigEndian.Uint16(cmd[2:])
		off += sz
		var payload []byte
		switch num {
		case enetCmdSendReliable:
			n := int(binary.BigEndian.Uint16(cmd[4:]))
			if off+n > len(d) {
				return
			}
			payload, off = d[off:off+n], off+n
		case enetCmdSendUnreliable, enetCmdSendUnseq, enetCmdSendFragment, enetCmdSendUnrelFrag:
			n := int(binary.BigEndian.Uint16(cmd[6:]))
			if off+n > len(d) {
				return
			}
			payload, off = d[off:off+n], off+n
		}
		switch num {
		case enetCmdAck:
			k := key(ch, binary.BigEndian.Uint16(cmd[4:]))
			delete(l.pending, k)
			ev.acked = append(ev.acked, k)
		case enetCmdConnect:
			ev.connect = append([]byte(nil), cmd...)
			continue // never acknowledged directly
		case enetCmdVerifyConnect:
			ev.verify = append([]byte(nil), cmd...)
		case enetCmdDisconnect:
			ev.disconnect = true
		case enetCmdSendReliable:
			if ch == 0 {
				deliver = append(deliver, l.onReliable(seq, &enetIn{data: append([]byte(nil), payload...)})...)
			}
		case enetCmdSendFragment:
			if ch == 0 {
				deliver = append(deliver, l.onFragment(cmd, seq, payload)...)
			}
		case enetCmdSendUnreliable, enetCmdSendUnseq:
			if ch == 0 {
				deliver = append(deliver, append([]byte(nil), payload...))
			}
		}
		if cb&enetFlagAck != 0 && hasSent {
			a := make([]byte, 8)
			a[0] = enetCmdAck
			a[1] = ch
			binary.BigEndian.PutUint16(a[2:], seq)
			binary.BigEndian.PutUint16(a[4:], seq)
			binary.BigEndian.PutUint16(a[6:], sentTime)
			acks = append(acks, a)
		}
	}
	if len(acks) > 0 {
		l.writeCmds(acks...)
	}
	return
}

func (l *enetLink) isOld(seq uint16) bool { return int16(seq-l.inSeq) <= 0 }

func (l *enetLink) onReliable(seq uint16, in *enetIn) [][]byte {
	if l.isOld(seq) {
		return nil
	}
	if _, ok := l.received[seq]; !ok {
		l.received[seq] = in
	}
	return l.drain()
}

func (l *enetLink) onFragment(cmd []byte, seq uint16, payload []byte) [][]byte {
	if l.isOld(seq) {
		return nil
	}
	startSeq := binary.BigEndian.Uint16(cmd[4:])
	count := binary.BigEndian.Uint32(cmd[8:])
	num := binary.BigEndian.Uint32(cmd[12:])
	total := binary.BigEndian.Uint32(cmd[16:])
	foff := binary.BigEndian.Uint32(cmd[20:])
	if count == 0 || count > 4096 || num >= count || total > 32*1024*1024 || foff >= total || uint32(len(payload)) > total-foff {
		return nil
	}
	f := l.frags[startSeq]
	if f == nil {
		f = &enetFrag{count: count, total: total, got: map[uint32]bool{}, data: make([]byte, total)}
		l.frags[startSeq] = f
	}
	if f.count != count || f.total != total {
		return nil
	}
	if !f.got[num] {
		f.got[num] = true
		copy(f.data[foff:], payload)
	}
	if _, ok := l.received[seq]; !ok {
		l.received[seq] = &enetIn{isFrag: true, startSeq: startSeq}
	}
	return l.drain()
}

func (l *enetLink) drain() [][]byte {
	var out [][]byte
	for {
		next := l.inSeq + 1
		in, ok := l.received[next]
		if !ok {
			return out
		}
		if !in.isFrag {
			delete(l.received, next)
			l.inSeq = next
			out = append(out, in.data)
			continue
		}
		f := l.frags[in.startSeq]
		if f == nil || in.startSeq != next || uint32(len(f.got)) < f.count {
			return out
		}
		for i := uint32(0); i < f.count; i++ {
			delete(l.received, next+uint16(i))
		}
		l.inSeq = next + uint16(f.count) - 1
		delete(l.frags, in.startSeq)
		out = append(out, f.data)
	}
}

// parseHeader strips the ENet protocol header.
func parseHeader(d []byte) (peerID uint16, session uint8, hasSent bool, sentTime uint16, body []byte, ok bool) {
	if len(d) < 2 {
		return
	}
	pid := binary.BigEndian.Uint16(d)
	if pid&enetHdrCompress != 0 {
		return
	}
	session = uint8((pid & enetHdrSessMask) >> enetHdrSessShift)
	peerID = pid &^ (enetHdrSentTime | enetHdrCompress | enetHdrSessMask)
	off := 2
	if pid&enetHdrSentTime != 0 {
		if len(d) < 4 {
			return
		}
		hasSent, sentTime, off = true, binary.BigEndian.Uint16(d[2:]), 4
	}
	return peerID, session, hasSent, sentTime, d[off:], true
}

// ======================= client =======================

type ENetClient struct {
	conn      *net.UDPConn
	mu        sync.Mutex
	link      *enetLink
	connected bool
	closed    bool
	connectID uint32

	verifyCh chan struct{}
	Recv     chan []byte
	Closed   chan struct{}
	closeErr error
}

func ENetDial(addr string, timeout time.Duration) (*ENetClient, error) {
	raddr, err := net.ResolveUDPAddr("udp4", addr)
	if err != nil {
		return nil, err
	}
	conn, err := net.DialUDP("udp4", nil, raddr)
	if err != nil {
		return nil, err
	}
	c := &ENetClient{
		conn:     conn,
		verifyCh: make(chan struct{}),
		Recv:     make(chan []byte, 1024),
		Closed:   make(chan struct{}),
	}
	c.link = newLink(func(p []byte) { conn.Write(p) })
	var b [4]byte
	rand.Read(b[:])
	c.connectID = binary.LittleEndian.Uint32(b[:])

	go c.readLoop()
	go c.timerLoop()

	c.mu.Lock()
	cmd := c.link.sysCmd(enetCmdConnect, 48)
	binary.BigEndian.PutUint16(cmd[4:], 0)
	cmd[6], cmd[7] = 0xFF, 0xFF
	binary.BigEndian.PutUint32(cmd[8:], enetDefaultMTU)
	binary.BigEndian.PutUint32(cmd[12:], 65536)
	binary.BigEndian.PutUint32(cmd[16:], 1)
	binary.BigEndian.PutUint32(cmd[28:], 5000)
	binary.BigEndian.PutUint32(cmd[32:], 2)
	binary.BigEndian.PutUint32(cmd[36:], 2)
	binary.LittleEndian.PutUint32(cmd[40:], c.connectID)
	c.link.queueReliable(0xFF, c.link.sysSeq, cmd)
	c.mu.Unlock()

	select {
	case <-c.verifyCh:
		return c, nil
	case <-c.Closed:
		return nil, errors.New("conexión cerrada por el servidor")
	case <-time.After(timeout):
		c.Close()
		return nil, errors.New("la sala no responde (¿está creada? ¿IP/puerto correctos?)")
	}
}

func (c *ENetClient) Send(data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.connected || c.closed {
		return errors.New("no conectado")
	}
	c.link.send(data)
	return nil
}

func (c *ENetClient) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	if c.connected {
		cmd := c.link.sysCmd(enetCmdDisconnect, 8)
		c.link.writeCmds(cmd)
		c.link.writeCmds(cmd)
	}
	c.shutdownLocked(errors.New("cerrado"))
}

func (c *ENetClient) shutdownLocked(err error) {
	if c.closed {
		return
	}
	c.closed = true
	c.closeErr = err
	close(c.Closed)
	c.conn.Close()
}

func (c *ENetClient) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closeErr
}

func (c *ENetClient) timerLoop() {
	t := time.NewTicker(50 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-c.Closed:
			return
		case <-t.C:
		}
		c.mu.Lock()
		if !c.closed {
			if err := c.link.tick(time.Now(), c.connected); err != nil {
				c.shutdownLocked(fmt.Errorf("se perdió la conexión con la sala (%v)", err))
			}
		}
		c.mu.Unlock()
	}
}

func (c *ENetClient) readLoop() {
	buf := make([]byte, 65536)
	for {
		n, err := c.conn.Read(buf)
		if err != nil {
			c.mu.Lock()
			closed := c.closed
			c.mu.Unlock()
			if closed || isClosedErr(err) {
				return
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if testDrop() {
			continue
		}
		_, _, hasSent, sentTime, body, ok := parseHeader(buf[:n])
		if !ok {
			continue
		}
		body = append([]byte(nil), body...)
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return
		}
		deliver, ev := c.link.process(body, hasSent, sentTime)
		if ev.verify != nil && !c.connected {
			v := ev.verify
			delete(c.link.pending, key(0xFF, 1))
			c.link.outgoingPeerID = binary.BigEndian.Uint16(v[4:])
			c.link.inSession = v[6]
			c.link.outSession = v[7]
			if m := binary.BigEndian.Uint32(v[8:]); m >= 576 && m < c.link.mtu {
				c.link.mtu = m
			}
			c.connected = true
			close(c.verifyCh)
		}
		if ev.disconnect {
			c.shutdownLocked(errors.New("la sala cerró la conexión"))
			c.mu.Unlock()
			return
		}
		c.mu.Unlock()
		for _, p := range deliver {
			select {
			case c.Recv <- p:
			default:
			}
		}
	}
}

// ======================= server =======================

type ENetPeer struct {
	ID        uint16
	Addr      *net.UDPAddr
	link      *enetLink
	connectID uint32
	state     int // 0 = acknowledging connect, 1 = connected
	Data      any
}

type ENetServer struct {
	conn  *net.UDPConn
	mu    sync.Mutex
	peers map[uint16]*ENetPeer
	next  uint16

	OnConnect    func(*ENetPeer)
	OnReceive    func(*ENetPeer, []byte)
	OnDisconnect func(*ENetPeer)
}

func ENetListen(port uint16) (*ENetServer, error) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: int(port)})
	if err != nil {
		return nil, err
	}
	s := &ENetServer{conn: conn, peers: map[uint16]*ENetPeer{}}
	return s, nil
}

func (s *ENetServer) Start() {
	go s.readLoop()
	go s.timerLoop()
}

func (s *ENetServer) Send(p *ENetPeer, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.peers[p.ID] != p || p.state != 1 {
		return
	}
	p.link.send(data)
}

func (s *ENetServer) Kick(p *ENetPeer) {
	s.mu.Lock()
	if s.peers[p.ID] != p {
		s.mu.Unlock()
		return
	}
	cmd := p.link.sysCmd(enetCmdDisconnect, 8)
	p.link.writeCmds(cmd)
	delete(s.peers, p.ID)
	s.mu.Unlock()
	if s.OnDisconnect != nil {
		s.OnDisconnect(p)
	}
}

func (s *ENetServer) timerLoop() {
	t := time.NewTicker(50 * time.Millisecond)
	defer t.Stop()
	for range t.C {
		var gone []*ENetPeer
		s.mu.Lock()
		now := time.Now()
		for id, p := range s.peers {
			if err := p.link.tick(now, p.state == 1); err != nil {
				delete(s.peers, id)
				if p.state == 1 {
					gone = append(gone, p)
				}
			}
		}
		s.mu.Unlock()
		for _, p := range gone {
			if s.OnDisconnect != nil {
				s.OnDisconnect(p)
			}
		}
	}
}

func (s *ENetServer) readLoop() {
	buf := make([]byte, 65536)
	for {
		n, from, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			if isClosedErr(err) {
				return
			}
			time.Sleep(10 * time.Millisecond)
			continue
		}
		if testDrop() {
			continue
		}
		peerID, session, hasSent, sentTime, body, ok := parseHeader(buf[:n])
		if !ok {
			continue
		}
		body = append([]byte(nil), body...)
		s.handle(from, peerID, session, hasSent, sentTime, body)
	}
}

func (s *ENetServer) handle(from *net.UDPAddr, peerID uint16, session uint8, hasSent bool, sentTime uint16, body []byte) {
	s.mu.Lock()
	var p *ENetPeer
	if peerID == enetMaxPeerID {
		// Only CONNECT is valid here.
		if len(body) < 48 || body[0]&0x0F != enetCmdConnect {
			s.mu.Unlock()
			return
		}
		cmd := body[:48]
		connectID := binary.LittleEndian.Uint32(cmd[40:])
		for _, q := range s.peers {
			if q.Addr.String() == from.String() && q.connectID == connectID {
				s.mu.Unlock()
				return // duplicate connect; our VERIFY is being retransmitted
			}
		}
		if len(s.peers) >= 64 {
			s.mu.Unlock()
			return
		}
		for {
			s.next = (s.next + 1) % enetMaxPeerID
			if _, used := s.peers[s.next]; !used {
				break
			}
		}
		addr := *from
		p = &ENetPeer{ID: s.next, Addr: &addr, connectID: connectID}
		conn := s.conn
		p.link = newLink(func(b []byte) { conn.WriteToUDP(b, &addr) })
		p.link.outgoingPeerID = binary.BigEndian.Uint16(cmd[4:])
		mtu := binary.BigEndian.Uint32(cmd[8:])
		if mtu < 576 {
			mtu = 576
		}
		if mtu > enetDefaultMTU {
			mtu = enetDefaultMTU
		}
		p.link.mtu = mtu
		in := cmd[6]
		if in == 0xFF {
			in = 0xFF
		}
		inc := (in + 1) & 3
		out := cmd[7]
		outc := (out + 1) & 3
		p.link.outSession = inc
		p.link.inSession = outc
		win := binary.BigEndian.Uint32(cmd[12:])
		if win > 65536 {
			win = 65536
		}
		if win < 4096 {
			win = 4096
		}
		v := p.link.sysCmd(enetCmdVerifyConnect, 44)
		binary.BigEndian.PutUint16(v[4:], p.ID)
		v[6] = inc
		v[7] = outc
		binary.BigEndian.PutUint32(v[8:], mtu)
		binary.BigEndian.PutUint32(v[12:], win)
		binary.BigEndian.PutUint32(v[16:], 1)
		copy(v[28:40], cmd[28:40]) // throttle interval/accel/decel must match
		copy(v[40:44], cmd[40:44]) // connectID
		s.peers[p.ID] = p
		p.link.queueReliable(0xFF, p.link.sysSeq, v)
		s.mu.Unlock()
		return
	}
	p = s.peers[peerID]
	if p == nil || p.Addr.String() != from.String() || session != p.link.inSession {
		s.mu.Unlock()
		return
	}
	deliver, ev := p.link.process(body, hasSent, sentTime)
	justConnected := false
	if p.state == 0 {
		for _, k := range ev.acked {
			if k == key(0xFF, 1) {
				p.state = 1
				justConnected = true
			}
		}
	}
	if ev.disconnect {
		delete(s.peers, p.ID)
	}
	connected := p.state == 1
	s.mu.Unlock()

	if justConnected && s.OnConnect != nil {
		s.OnConnect(p)
	}
	if connected && s.OnReceive != nil {
		for _, d := range deliver {
			s.OnReceive(p, d)
		}
	}
	if ev.disconnect && connected && s.OnDisconnect != nil {
		s.OnDisconnect(p)
	}
}

var debugENet = os.Getenv("BRIDGE_DEBUG") != ""

var dropRate, _ = strconv.ParseFloat(os.Getenv("BRIDGE_TEST_DROP"), 64)

// testDrop simulates packet loss (only when BRIDGE_TEST_DROP is set, for testing).
func testDrop() bool { return dropRate > 0 && mrand.Float64() < dropRate }
