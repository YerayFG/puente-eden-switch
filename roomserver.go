// SPDX-License-Identifier: GPL-3.0-or-later
// Puente Eden <-> Switch (ldn_mitm)

package main

// Built-in Eden/yuzu-compatible multiplayer room. Eden joins it with
// "Direct Connect". The Switch is a virtual member of the room whose virtual
// IP is the Switch's real LAN IP, and Eden members get this PC's real LAN IP,
// so games that put IP addresses inside their own packets (e.g. Smash) work.

import (
	"strings"
	"sync"
)

type roomMember struct {
	peer     *ENetPeer
	nickname string
	ip       IPv4
	gameName string
	gameID   uint64
	gameVer  string
}

type RoomServer struct {
	srv      *ENetServer
	mu       sync.Mutex
	name     string
	password string
	port     uint16
	members  []*roomMember

	swNick string
	swIP   IPv4
	lanIP  func() IPv4

	onLDN   func(LDNPacket)
	onProxy func(ProxyPacket)
}

func NewRoomServer(port uint16, name, password, switchNick string, lanIP func() IPv4) (*RoomServer, error) {
	s, err := ENetListen(port)
	if err != nil {
		return nil, err
	}
	rs := &RoomServer{srv: s, name: name, password: password, port: port, swNick: switchNick, lanIP: lanIP}
	s.OnReceive = rs.onReceive
	s.OnDisconnect = rs.onDisconnect
	s.Start()
	return rs, nil
}

// ---- RoomLink implementation (used by the bridge as "the Switch")

func (rs *RoomServer) MyIP() IPv4 {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.swIP
}

func (rs *RoomServer) SetHandlers(onLDN func(LDNPacket), onProxy func(ProxyPacket)) {
	rs.mu.Lock()
	rs.onLDN, rs.onProxy = onLDN, onProxy
	rs.mu.Unlock()
}

func (rs *RoomServer) SetSwitchIP(ip IPv4) {
	rs.mu.Lock()
	changed := rs.swIP != ip
	rs.swIP = ip
	rs.mu.Unlock()
	if changed {
		rs.broadcastInfo()
	}
}

func (rs *RoomServer) SendLDN(l LDNPacket) {
	w := &wbuf{}
	w.u8(idLdnPacket)
	w.u8(l.Type)
	w.ip(l.LocalIP)
	w.ip(l.RemoteIP)
	w.boolean(l.Broadcast)
	w.vec(l.Data)
	rs.route(nil, l.RemoteIP, l.Broadcast, w.b)
}

func (rs *RoomServer) SendProxy(x ProxyPacket) {
	w := &wbuf{}
	w.u8(idProxyPacket)
	w.u8(1)
	w.ip(x.Local.IP)
	w.u16(x.Local.Port)
	w.u8(1)
	w.ip(x.Remote.IP)
	w.u16(x.Remote.Port)
	w.u8(x.Protocol)
	w.boolean(x.Broadcast)
	w.vec(x.Data)
	rs.route(nil, x.Remote.IP, x.Broadcast, w.b)
}

// route delivers a raw room packet. from == nil means it comes from the Switch.
func (rs *RoomServer) route(from *ENetPeer, dst IPv4, broadcast bool, pkt []byte) {
	rs.mu.Lock()
	var targets []*ENetPeer
	toSwitch := false
	for _, m := range rs.members {
		if m.peer == from {
			continue
		}
		if broadcast || m.ip == dst {
			targets = append(targets, m.peer)
		}
	}
	if from != nil && (broadcast || dst == rs.swIP) {
		toSwitch = true
	}
	rs.mu.Unlock()
	for _, p := range targets {
		rs.srv.Send(p, pkt)
	}
	if toSwitch {
		rs.deliverToSwitch(pkt)
	}
}

func (rs *RoomServer) deliverToSwitch(p []byte) {
	rs.mu.Lock()
	onLDN, onProxy := rs.onLDN, rs.onProxy
	rs.mu.Unlock()
	r := &rbuf{b: p, off: 1}
	switch p[0] {
	case idLdnPacket:
		var l LDNPacket
		l.Type = r.u8()
		l.LocalIP = r.ip()
		l.RemoteIP = r.ip()
		l.Broadcast = r.boolean()
		l.Data = r.vec()
		if !r.bad && onLDN != nil {
			onLDN(l)
		}
	case idProxyPacket:
		var x ProxyPacket
		r.u8()
		x.Local.IP = r.ip()
		x.Local.Port = r.u16()
		r.u8()
		x.Remote.IP = r.ip()
		x.Remote.Port = r.u16()
		x.Protocol = r.u8()
		x.Broadcast = r.boolean()
		x.Data = r.vec()
		if !r.bad && onProxy != nil {
			onProxy(x)
		}
	}
}

// ---- room protocol (server side)

func (rs *RoomServer) member(p *ENetPeer) *roomMember {
	for _, m := range rs.members {
		if m.peer == p {
			return m
		}
	}
	return nil
}

func (rs *RoomServer) onReceive(p *ENetPeer, data []byte) {
	if len(data) == 0 {
		return
	}
	switch data[0] {
	case idJoinRequest:
		rs.handleJoin(p, data)
	case idSetGameInfo:
		r := &rbuf{b: data, off: 1}
		name := r.str()
		id := r.u64()
		ver := r.str()
		rs.mu.Lock()
		m := rs.member(p)
		if m != nil && !r.bad {
			m.gameName, m.gameID, m.gameVer = name, id, ver
		}
		rs.mu.Unlock()
		if m != nil {
			statusUpdate(func(s *Status) {
				if s.EdenNick == m.nickname {
					s.EdenGame = strings.TrimSpace(name + " " + ver)
				}
			})
			if name != "" {
				logf("Eden (%s) está jugando a %s %s", m.nickname, name, ver)
			}
			rs.broadcastInfo()
		}
	case idProxyPacket:
		r := &rbuf{b: data, off: 1}
		r.u8()
		r.ip()
		r.u16()
		r.u8()
		dst := r.ip()
		r.u16()
		r.u8()
		bc := r.boolean()
		if !r.bad && rs.isMember(p) {
			rs.route(p, dst, bc, data)
		}
	case idLdnPacket:
		r := &rbuf{b: data, off: 1}
		r.u8()
		r.ip()
		dst := r.ip()
		bc := r.boolean()
		if !r.bad && rs.isMember(p) {
			rs.route(p, dst, bc, data)
		}
	case idChatMessage:
		r := &rbuf{b: data, off: 1}
		msg := r.str()
		rs.mu.Lock()
		m := rs.member(p)
		rs.mu.Unlock()
		if m == nil || r.bad || len(msg) > 500 {
			return
		}
		logf("chat %s: %s", m.nickname, msg)
		w := &wbuf{}
		w.u8(idChatMessage)
		w.str(m.nickname)
		w.str("")
		w.str(msg)
		rs.sendAll(w.b)
	case idModGetBanList:
		w := &wbuf{}
		w.u8(idModBanListResponse)
		w.u32(0)
		w.u32(0)
		rs.srv.Send(p, w.b)
	}
}

func (rs *RoomServer) isMember(p *ENetPeer) bool {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.member(p) != nil
}

func (rs *RoomServer) handleJoin(p *ENetPeer, data []byte) {
	r := &rbuf{b: data, off: 1}
	nick := r.str()
	pref := r.ip()
	ver := r.u32()
	pass := r.str()
	r.str() // token
	if r.bad {
		return
	}
	reply := func(id uint8) { rs.srv.Send(p, []byte{id}) }
	if pass != rs.password {
		reply(idWrongPassword)
		return
	}
	if ver != roomNetworkVersion {
		reply(idVersionMismatch)
		return
	}
	rs.mu.Lock()
	if rs.member(p) != nil {
		rs.mu.Unlock()
		return
	}
	if nick == "" || strings.EqualFold(nick, rs.swNick) {
		rs.mu.Unlock()
		reply(idNameCollision)
		return
	}
	for _, m := range rs.members {
		if strings.EqualFold(m.nickname, nick) {
			rs.mu.Unlock()
			reply(idNameCollision)
			return
		}
	}
	if len(rs.members) >= 15 {
		rs.mu.Unlock()
		reply(idRoomIsFull)
		return
	}
	used := func(ip IPv4) bool {
		if ip == rs.swIP {
			return true
		}
		for _, m := range rs.members {
			if m.ip == ip {
				return true
			}
		}
		return false
	}
	var ip IPv4
	lan := rs.lanIP()
	switch {
	case pref != (IPv4{0xFF, 0xFF, 0xFF, 0xFF}) && !used(pref):
		ip = pref
	case !used(lan):
		// First Eden player: use this PC's real LAN address.
		ip = lan
	default:
		for i := 200; i < 254 && ip == (IPv4{}); i++ {
			cand := IPv4{lan[0], lan[1], lan[2], byte(i)}
			if !used(cand) {
				ip = cand
			}
		}
	}
	m := &roomMember{peer: p, nickname: nick, ip: ip}
	rs.members = append(rs.members, m)
	rs.mu.Unlock()

	logf("%s (Eden) entró en la sala con IP %s", nick, ip)
	statusUpdate(func(s *Status) { s.EdenNick, s.EdenIP, s.EdenGame = nick, ip.String(), "" })
	rs.sendStatus(idMemberJoin, nick)
	rs.broadcastInfo()
	w := &wbuf{}
	w.u8(idJoinSuccess)
	w.ip(ip)
	rs.srv.Send(p, w.b)
}

func (rs *RoomServer) onDisconnect(p *ENetPeer) {
	rs.mu.Lock()
	var gone *roomMember
	for i, m := range rs.members {
		if m.peer == p {
			gone = m
			rs.members = append(rs.members[:i], rs.members[i+1:]...)
			break
		}
	}
	rs.mu.Unlock()
	if gone != nil {
		logf("%s salió de la sala", gone.nickname)
		statusUpdate(func(s *Status) {
			if s.EdenNick == gone.nickname {
				s.EdenNick, s.EdenIP, s.EdenGame = "", "", ""
			}
		})
		rs.sendStatus(idMemberLeave, gone.nickname)
		rs.broadcastInfo()
	}
}

func (rs *RoomServer) sendAll(pkt []byte) {
	rs.mu.Lock()
	var ps []*ENetPeer
	for _, m := range rs.members {
		ps = append(ps, m.peer)
	}
	rs.mu.Unlock()
	for _, p := range ps {
		rs.srv.Send(p, pkt)
	}
}

const (
	idMemberJoin  = 1
	idMemberLeave = 2
)

func (rs *RoomServer) sendStatus(typ uint8, nick string) {
	w := &wbuf{}
	w.u8(idStatusMessage)
	w.u8(typ)
	w.str(nick)
	w.str("")
	rs.sendAll(w.b)
}

func (rs *RoomServer) broadcastInfo() {
	rs.mu.Lock()
	w := &wbuf{}
	w.u8(idRoomInformation)
	w.str(rs.name)
	w.str("Sala creada por el Puente Eden <-> Switch")
	w.u32(16)
	w.u16(rs.port)
	w.str("")
	w.str("")
	w.u32(uint32(len(rs.members) + 1))
	// the Switch
	w.str(rs.swNick)
	w.ip(rs.swIP)
	w.str("Nintendo Switch (ldn_mitm)")
	w.u64(0)
	w.str("")
	w.str("")
	w.str("")
	w.str("")
	for _, m := range rs.members {
		w.str(m.nickname)
		w.ip(m.ip)
		w.str(m.gameName)
		w.u64(m.gameID)
		w.str(m.gameVer)
		w.str("")
		w.str("")
		w.str("")
	}
	rs.mu.Unlock()
	rs.sendAll(w.b)
}

func (rs *RoomServer) MemberSummary() string {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	var s []string
	for _, m := range rs.members {
		s = append(s, m.nickname+" ("+m.ip.String()+")")
	}
	return strings.Join(s, ", ")
}
