// SPDX-License-Identifier: GPL-3.0-or-later
// Puente Eden <-> Switch (ldn_mitm)

package main

// Control plane: translates ldn_mitm LAN discovery <-> Eden room LDN packets.

import (
	"bytes"
	"net"
	"sync"
	"time"
)

type Bridge struct {
	mu sync.Mutex

	room   RoomLink
	lanIP  IPv4 // this PC on the LAN
	lanBC  IPv4 // LAN broadcast address
	ownIPs map[IPv4]bool

	swIP      IPv4 // the Switch (learned or configured)
	swKnown   bool
	swFixed   bool
	edenIP    IPv4 // the Eden member we are playing with
	edenKnown bool

	udp *net.UDPConn
	tcp *net.TCPListener

	// Eden hosts, Switch is a station
	lastScanFrom  *net.UDPAddr // Switch scan socket
	edenHostIP    IPv4         // Eden host fake ip (from scan results)
	stationConn   net.Conn     // TCP from the Switch
	stationNode   []byte       // NodeInfo (Eden-side) sent in Connect
	stationHostIP IPv4

	// Switch hosts, Eden is a station
	edenScanner    IPv4              // Eden member that asked to scan
	hostConns      map[IPv4]net.Conn // Eden station -> TCP to Switch
	lastSwitchInfo []byte

	data         *DataPlane
	lanFixed     bool
	onSwitchIP   func(IPv4)
	onLANChange  func(IPv4)
	lastActivity time.Time
}

func NewBridge(lanIP, lanBC IPv4, own map[IPv4]bool) *Bridge {
	b := &Bridge{lanIP: lanIP, lanBC: lanBC, ownIPs: own, hostConns: map[IPv4]net.Conn{}}
	b.data = newDataPlane(b)
	return b
}

func (b *Bridge) currentRoom() RoomLink {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.room
}

// RoomLink is how the bridge talks to the Eden room (as the Switch).
type RoomLink interface {
	MyIP() IPv4
	SendLDN(LDNPacket)
	SendProxy(ProxyPacket)
	SetHandlers(onLDN func(LDNPacket), onProxy func(ProxyPacket))
}

func (b *Bridge) setRoom(r RoomLink) {
	b.mu.Lock()
	b.room = r
	b.mu.Unlock()
	if r != nil {
		r.SetHandlers(b.onRoomLDN, b.data.fromRoom)
	}
}

func (b *Bridge) switchIP() (IPv4, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.swIP, b.swKnown
}

func (b *Bridge) lanBroadcast() IPv4 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lanBC
}

func (b *Bridge) setSwitchIP(ip IPv4) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.swFixed || b.ownIPs[ip] {
		return
	}
	if !b.swKnown || b.swIP != ip {
		b.swIP = ip
		b.swKnown = true
		logf("Switch detectada en %s", ip)
		touchSwitch(ip)
		b.pickLANForSwitchLocked()
		if b.onSwitchIP != nil {
			b.onSwitchIP(ip)
		}
	}
}

// pickLANForSwitchLocked makes sure we use the PC address that is on the same
// network as the Switch (PCs with VPNs or virtual adapters have several).
func (b *Bridge) pickLANForSwitchLocked() {
	if b.lanFixed {
		return
	}
	for _, c := range lanCands {
		if c.ip == b.lanIP && c.contains(b.swIP) {
			return
		}
	}
	for _, c := range lanCands {
		if c.contains(b.swIP) && c.prefix() <= 30 {
			logf("Uso la red del PC %s (la misma que la Switch) en lugar de %s", c.ip, b.lanIP)
			b.lanIP, b.lanBC = c.ip, c.bc
			statusUpdate(func(s *Status) { s.PCIP = c.ip.String() })
			if b.onLANChange != nil {
				go b.onLANChange(c.ip)
			}
			return
		}
	}
	logf("Aviso: la Switch (%s) no está en la misma red que el PC (%s). Comprueba que están en el mismo router.", b.swIP, b.lanIP)
	statusUpdate(func(s *Status) {
		s.Warning = "La Switch y el PC no parecen estar en la misma red. Conéctalos al mismo router."
	})
}

func (b *Bridge) acceptSwitchSource(ip IPv4) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ownIPs[ip] {
		return false
	}
	return b.swKnown && ip == b.swIP
}

func (b *Bridge) noteEdenPeer(ip IPv4) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.room != nil && ip == b.room.MyIP() {
		return
	}
	if !b.edenKnown || b.edenIP != ip {
		b.edenIP = ip
		b.edenKnown = true
		logv("jugador de Eden: %s", ip)
	}
}

func (b *Bridge) edenPeerIP() (IPv4, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.edenIP, b.edenKnown
}

func (b *Bridge) sessionActive() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.stationConn != nil || len(b.hostConns) > 0 || time.Since(b.lastActivity) < 2*time.Minute
}

func (b *Bridge) touch() {
	b.mu.Lock()
	b.lastActivity = time.Now()
	b.mu.Unlock()
}

// ---- IP mappings

// toSwitchSide maps addresses inside data going to the Switch.
func (b *Bridge) toSwitchSide(ip IPv4) (IPv4, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.room != nil && ip == b.room.MyIP() {
		if b.swKnown {
			return b.swIP, true
		}
		return ip, false
	}
	// any other Eden member is represented by this PC
	return b.lanIP, true
}

// toEdenSide maps addresses inside data going to Eden.
func (b *Bridge) toEdenSide(ip IPv4) (IPv4, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.room == nil {
		return ip, false
	}
	if b.swKnown && ip == b.swIP {
		return b.room.MyIP(), true
	}
	if ip == b.lanIP && b.edenKnown {
		return b.edenIP, true
	}
	return ip, false
}

// ---- LAN side (ldn_mitm)

func (b *Bridge) StartLAN(bindIP *IPv4) error {
	var u *net.UDPConn
	var err error
	var tcpIP net.IP
	if bindIP != nil {
		u, err = listenUDPSharedAddr(net.IP(bindIP[:]).String(), lanPort)
		tcpIP = net.IP(bindIP[:])
	} else {
		u, err = listenUDPShared(lanPort)
	}
	if err != nil {
		return err
	}
	b.udp = u
	l, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: tcpIP, Port: lanPort})
	if err != nil {
		return err
	}
	b.tcp = l
	go b.udpLoop()
	go b.tcpAcceptLoop()
	return nil
}

func (b *Bridge) udpLoop() {
	buf := make([]byte, 4096)
	for {
		n, from, err := b.udp.ReadFromUDP(buf)
		if err != nil {
			if isClosedErr(err) {
				return
			}
			continue
		}
		src := netIPv4(from.IP)
		b.mu.Lock()
		own := b.ownIPs[src]
		fixed := b.swFixed
		sw := b.swIP
		b.mu.Unlock()
		if own || (fixed && src != sw) {
			continue
		}
		typ, body, _, err := decodeLan(buf[:n])
		if err != nil {
			continue
		}
		switch typ {
		case ldnScan:
			b.setSwitchIP(src)
			touchSwitch(src)
			b.touch()
			b.onSwitchScan(from)
		case ldnScanResp:
			b.setSwitchIP(src)
			b.touch()
			b.onSwitchScanResp(body)
		}
	}
}

// The Switch is searching for games: ask everyone in the Eden room.
func (b *Bridge) onSwitchScan(from *net.UDPAddr) {
	room := b.currentRoom()
	if room == nil {
		return
	}
	b.mu.Lock()
	b.lastScanFrom = from
	b.mu.Unlock()
	logv("la Switch busca partidas…")
	room.SendLDN(LDNPacket{Type: ldnScan, LocalIP: room.MyIP(), Broadcast: true})
}

// The Switch answered a scan we relayed for an Eden member.
func (b *Bridge) onSwitchScanResp(info []byte) {
	room := b.currentRoom()
	if room == nil || len(info) != netInfoSize {
		return
	}
	b.mu.Lock()
	target := b.edenScanner
	b.lastSwitchInfo = info
	b.mu.Unlock()
	if target == (IPv4{}) {
		return
	}
	out := rewriteNetworkInfo(info, b.toEdenSide, macForEdenSide)
	lcid, nodes, max, host := netInfoSummary(info)
	if snapshot(1<<30).SessionKind != "playing" {
		setSession("searching", "Partida encontrada en la Switch (anfitrión: "+host+")", "Switch")
	}
	logOnceKey("swhost", host, "partida encontrada en la Switch: anfitrión %q, %d/%d jugadores (juego %016x)", host, nodes, max, lcid)
	room.SendLDN(LDNPacket{Type: ldnScanResp, LocalIP: room.MyIP(), RemoteIP: target, Data: out})
}

func (b *Bridge) tcpAcceptLoop() {
	for {
		c, err := b.tcp.Accept()
		if err != nil {
			if isClosedErr(err) {
				return
			}
			continue
		}
		src := netIPv4(c.RemoteAddr().(*net.TCPAddr).IP)
		b.mu.Lock()
		ok := !b.swFixed || src == b.swIP
		b.mu.Unlock()
		if !ok {
			c.Close()
			continue
		}
		b.setSwitchIP(src)
		go b.handleStation(c)
	}
}

// The Switch joins a game hosted in Eden.
func (b *Bridge) handleStation(c net.Conn) {
	defer c.Close()
	room := b.currentRoom()
	if room == nil {
		return
	}
	b.mu.Lock()
	host := b.edenHostIP
	if b.stationConn != nil {
		b.stationConn.Close()
	}
	b.stationConn = c
	b.mu.Unlock()
	if host == (IPv4{}) {
		logf("la Switch intenta unirse pero no hay partida de Eden detectada")
		return
	}
	for {
		typ, body, err := readLanTCP(c)
		if err != nil {
			break
		}
		if typ == ldnConnect && len(body) == nodeInfoSize {
			node := append([]byte(nil), body...)
			rewriteNode(node, b.toEdenSide, macForEdenSide)
			b.mu.Lock()
			b.stationNode = node
			b.stationHostIP = host
			b.mu.Unlock()
			b.noteEdenPeer(host)
			b.touch()
			logf("la Switch (%s) se une a la partida de Eden", nodeUserName(body))
			setSession("playing", "La Switch ("+nodeUserName(body)+") está en la partida de Eden", "Eden")
			room.SendLDN(LDNPacket{Type: ldnConnect, LocalIP: room.MyIP(), RemoteIP: host, Data: node})
		}
	}
	// TCP closed -> Switch left
	b.mu.Lock()
	node := b.stationNode
	same := b.stationConn == c
	if same {
		b.stationConn = nil
		b.stationNode = nil
	}
	b.mu.Unlock()
	if same && node != nil {
		logf("la Switch salió de la partida de Eden")
		setSession("", "La Switch salió de la partida", "")
		room.SendLDN(LDNPacket{Type: ldnDisconnect, LocalIP: room.MyIP(), RemoteIP: host, Data: node})
	}
}

// ---- Room side (Eden)

func (b *Bridge) onRoomLDN(p LDNPacket) {
	room := b.currentRoom()
	if room == nil || p.LocalIP == room.MyIP() {
		return
	}
	if !p.Broadcast && p.RemoteIP != room.MyIP() {
		return
	}
	switch p.Type {
	case ldnScan:
		// An Eden player searches for games: look for one on the Switch.
		b.mu.Lock()
		b.edenScanner = p.LocalIP
		sw, known := b.swIP, b.swKnown
		b.mu.Unlock()
		b.noteEdenPeer(p.LocalIP)
		b.touch()
		pkt := encodeLan(ldnScan, nil)
		bc := b.lanBroadcast()
		b.udp.WriteToUDP(pkt, &net.UDPAddr{IP: net.IP(bc[:]), Port: lanPort})
		if known {
			b.udp.WriteToUDP(pkt, &net.UDPAddr{IP: net.IP(sw[:]), Port: lanPort})
		}
	case ldnScanResp:
		if len(p.Data) != netInfoSize {
			return
		}
		b.mu.Lock()
		to := b.lastScanFrom
		b.edenHostIP = netInfoHostIP(p.Data)
		b.mu.Unlock()
		b.noteEdenPeer(p.LocalIP)
		b.touch()
		lcid, nodes, max, host := netInfoSummary(p.Data)
		if snapshot(1<<30).SessionKind != "playing" {
			setSession("searching", "Partida encontrada en Eden (anfitrión: "+host+")", "Eden")
		}
		logOnceKey("edenhost", host, "partida encontrada en Eden: anfitrión %q, %d/%d jugadores (juego %016x)", host, nodes, max, lcid)
		if to != nil {
			out := rewriteNetworkInfo(p.Data, b.toSwitchSide, macForSwitchSide)
			b.udp.WriteToUDP(encodeLan(ldnScanResp, out), to)
		}
	case ldnSyncNetwork:
		if len(p.Data) != netInfoSize {
			return
		}
		b.mu.Lock()
		c := b.stationConn
		b.mu.Unlock()
		b.touch()
		if c != nil {
			out := rewriteNetworkInfo(p.Data, b.toSwitchSide, macForSwitchSide)
			_, err := c.Write(encodeLan(ldnSyncNetwork, out))
			_, nodes, max, _ := netInfoSummary(out)
			if err != nil {
				logf("error enviando el estado de la partida a la Switch: %v", err)
			} else {
				logv("estado de la partida enviado a la Switch: %d/%d jugadores", nodes, max)
			}
		} else {
			logf("aviso: Eden mandó el estado de la partida pero la Switch no está conectada")
		}
	case ldnDestroyNetwork:
		b.mu.Lock()
		c := b.stationConn
		b.stationConn = nil
		b.stationNode = nil
		b.mu.Unlock()
		if c != nil {
			logf("el anfitrión de Eden cerró la partida")
			setSession("", "Eden cerró la partida", "")
			c.Close()
		}
	case ldnConnect:
		// An Eden player joins the game hosted on the Switch.
		if len(p.Data) != nodeInfoSize {
			return
		}
		b.noteEdenPeer(p.LocalIP)
		b.touch()
		go b.connectToSwitch(p.LocalIP, p.Data)
	case ldnDisconnect:
		b.mu.Lock()
		c := b.hostConns[p.LocalIP]
		delete(b.hostConns, p.LocalIP)
		b.mu.Unlock()
		if c != nil {
			logf("el jugador de Eden salió de la partida de la Switch")
			setSession("", "Eden salió de la partida", "")
			c.Close()
		}
	}
}

func (b *Bridge) connectToSwitch(edenIP IPv4, node []byte) {
	room := b.currentRoom()
	sw, ok := b.switchIP()
	if room == nil || !ok {
		logf("un jugador de Eden quiere unirse, pero aún no sé la IP de la Switch")
		return
	}
	b.mu.Lock()
	if old := b.hostConns[edenIP]; old != nil {
		old.Close()
	}
	if len(b.hostConns) > 0 {
		b.mu.Unlock()
		logf("aviso: por ahora solo se admite 1 jugador de Eden en la partida de la Switch")
		return
	}
	b.mu.Unlock()
	c, err := net.DialTimeout("tcp4", (&net.TCPAddr{IP: net.IP(sw[:]), Port: lanPort}).String(), 3*time.Second)
	if err != nil {
		logf("no pude conectar con la Switch: %v", err)
		return
	}
	b.mu.Lock()
	b.hostConns[edenIP] = c
	b.mu.Unlock()
	n := append([]byte(nil), node...)
	rewriteNode(n, b.toSwitchSide, macForSwitchSide)
	logf("el jugador de Eden %q se une a la partida de la Switch", nodeUserName(node))
	setSession("playing", "Eden ("+nodeUserName(node)+") está en la partida de la Switch", "Switch")
	c.Write(encodeLan(ldnConnect, n))
	var last []byte
	for {
		typ, body, err := readLanTCP(c)
		if err != nil {
			break
		}
		if typ == ldnSyncNetwork && len(body) == netInfoSize {
			out := rewriteNetworkInfo(body, b.toEdenSide, macForEdenSide)
			if bytes.Equal(out, last) {
				continue
			}
			last = out
			b.touch()
			room.SendLDN(LDNPacket{Type: ldnSyncNetwork, LocalIP: room.MyIP(), RemoteIP: edenIP, Data: out})
		}
	}
	b.mu.Lock()
	still := b.hostConns[edenIP] == c
	if still {
		delete(b.hostConns, edenIP)
	}
	b.mu.Unlock()
	c.Close()
	if still {
		logf("la Switch cerró su partida")
		setSession("", "La Switch cerró su partida", "")
		room.SendLDN(LDNPacket{Type: ldnDestroyNetwork, LocalIP: room.MyIP(), RemoteIP: edenIP})
	}
}

func (b *Bridge) dropSessions() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.stationConn != nil {
		b.stationConn.Close()
		b.stationConn = nil
	}
	for k, c := range b.hostConns {
		c.Close()
		delete(b.hostConns, k)
	}
}
