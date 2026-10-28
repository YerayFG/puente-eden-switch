// SPDX-License-Identifier: GPL-3.0-or-later
// Puente Eden <-> Switch (ldn_mitm)

package main

// Eden/yuzu/Citra multiplayer room member protocol (network_version 1).

import (
	"encoding/binary"
	"errors"
	"fmt"
	"time"
)

const (
	idJoinRequest         = 1
	idJoinSuccess         = 2
	idRoomInformation     = 3
	idSetGameInfo         = 4
	idProxyPacket         = 5
	idLdnPacket           = 6
	idChatMessage         = 7
	idNameCollision       = 8
	idIpCollision         = 9
	idVersionMismatch     = 10
	idWrongPassword       = 11
	idCloseRoom           = 12
	idRoomIsFull          = 13
	idStatusMessage       = 14
	idHostKicked          = 15
	idHostBanned          = 16
	idModGetBanList       = 20
	idModBanListResponse  = 21
	idModPermissionDenied = 22
	idModNoSuchUser       = 23
	idJoinSuccessAsMod    = 24

	roomNetworkVersion = 1
)

// LDN packet types (same values as ldn_mitm's LANPacketType for the first four).
const (
	ldnScan           = 0
	ldnScanResp       = 1
	ldnConnect        = 2
	ldnSyncNetwork    = 3
	ldnDisconnect     = 4
	ldnDestroyNetwork = 5
)

const (
	protoTCP = 3
	protoUDP = 4
)

type IPv4 [4]byte

func (ip IPv4) String() string { return fmt.Sprintf("%d.%d.%d.%d", ip[0], ip[1], ip[2], ip[3]) }

type LDNPacket struct {
	Type      uint8
	LocalIP   IPv4
	RemoteIP  IPv4
	Broadcast bool
	Data      []byte
}

type SockAddr struct {
	IP   IPv4
	Port uint16
}

type ProxyPacket struct {
	Local     SockAddr
	Remote    SockAddr
	Protocol  uint8
	Broadcast bool
	Data      []byte // compressed (zstd) as on the wire
}

type RoomMemberInfo struct {
	Nickname string
	FakeIP   IPv4
	GameName string
}

// ---- serialization helpers (Citra Packet format: big endian, u32-length strings/vectors)

type wbuf struct{ b []byte }

func (w *wbuf) u8(v uint8)   { w.b = append(w.b, v) }
func (w *wbuf) u16(v uint16) { w.b = binary.BigEndian.AppendUint16(w.b, v) }
func (w *wbuf) u32(v uint32) { w.b = binary.BigEndian.AppendUint32(w.b, v) }
func (w *wbuf) u64(v uint64) { w.b = binary.BigEndian.AppendUint64(w.b, v) }
func (w *wbuf) str(s string) { w.u32(uint32(len(s))); w.b = append(w.b, s...) }
func (w *wbuf) ip(ip IPv4)   { w.b = append(w.b, ip[:]...) }
func (w *wbuf) boolean(v bool) {
	if v {
		w.u8(1)
	} else {
		w.u8(0)
	}
}
func (w *wbuf) vec(d []byte) { w.u32(uint32(len(d))); w.b = append(w.b, d...) }

type rbuf struct {
	b   []byte
	off int
	bad bool
}

func (r *rbuf) need(n int) bool {
	if r.bad || r.off+n > len(r.b) {
		r.bad = true
		return false
	}
	return true
}
func (r *rbuf) u8() uint8 {
	if !r.need(1) {
		return 0
	}
	v := r.b[r.off]
	r.off++
	return v
}
func (r *rbuf) u16() uint16 {
	if !r.need(2) {
		return 0
	}
	v := binary.BigEndian.Uint16(r.b[r.off:])
	r.off += 2
	return v
}
func (r *rbuf) u32() uint32 {
	if !r.need(4) {
		return 0
	}
	v := binary.BigEndian.Uint32(r.b[r.off:])
	r.off += 4
	return v
}
func (r *rbuf) u64() uint64 {
	if !r.need(8) {
		return 0
	}
	v := binary.BigEndian.Uint64(r.b[r.off:])
	r.off += 8
	return v
}
func (r *rbuf) bytes(n int) []byte {
	if n < 0 || !r.need(n) {
		return nil
	}
	v := r.b[r.off : r.off+n]
	r.off += n
	return v
}
func (r *rbuf) str() string   { return string(r.bytes(int(r.u32()))) }
func (r *rbuf) vec() []byte   { return append([]byte(nil), r.bytes(int(r.u32()))...) }
func (r *rbuf) boolean() bool { return r.u8() != 0 }
func (r *rbuf) ip() (ip IPv4) {
	copy(ip[:], r.bytes(4))
	return
}

// ---- Room client

type RoomClient struct {
	enet     *ENetClient
	FakeIP   IPv4
	Members  []RoomMemberInfo
	RoomName string

	OnLDN     func(LDNPacket)
	OnProxy   func(ProxyPacket)
	OnMembers func([]RoomMemberInfo)
	OnChat    func(nick, msg string)
	Done      chan struct{}
	DoneErr   error
}

func JoinRoom(addr, nickname, password string) (*RoomClient, error) {
	e, err := ENetDial(addr, 5*time.Second)
	if err != nil {
		return nil, err
	}
	rc := &RoomClient{enet: e, Done: make(chan struct{})}

	w := &wbuf{}
	w.u8(idJoinRequest)
	w.str(nickname)
	w.ip(IPv4{0xFF, 0xFF, 0xFF, 0xFF})
	w.u32(roomNetworkVersion)
	w.str(password)
	w.str("")
	e.Send(w.b)

	g := &wbuf{}
	g.u8(idSetGameInfo)
	g.str("Nintendo Switch (ldn_mitm)")
	g.u64(0)
	g.str("")
	e.Send(g.b)

	deadline := time.After(8 * time.Second)
	for {
		select {
		case p := <-e.Recv:
			if len(p) == 0 {
				continue
			}
			switch p[0] {
			case idRoomInformation:
				rc.parseRoomInfo(p)
			case idJoinSuccess, idJoinSuccessAsMod:
				r := &rbuf{b: p, off: 1}
				rc.FakeIP = r.ip()
				go rc.loop()
				return rc, nil
			case idNameCollision:
				e.Close()
				return nil, errors.New("ese nombre ya está en uso en la sala; usa otro con -nombre")
			case idIpCollision:
				e.Close()
				return nil, errors.New("conflicto de IP en la sala")
			case idVersionMismatch:
				e.Close()
				return nil, errors.New("versión de sala incompatible")
			case idWrongPassword:
				e.Close()
				return nil, errors.New("contraseña de la sala incorrecta; usa -password")
			case idRoomIsFull:
				e.Close()
				return nil, errors.New("la sala está llena")
			case idHostBanned:
				e.Close()
				return nil, errors.New("estás baneado de esa sala")
			}
		case <-e.Closed:
			return nil, fmt.Errorf("la sala cerró la conexión: %v", e.Err())
		case <-deadline:
			e.Close()
			return nil, errors.New("la sala no aceptó la conexión a tiempo")
		}
	}
}

func (rc *RoomClient) parseRoomInfo(p []byte) {
	r := &rbuf{b: p, off: 1}
	rc.RoomName = r.str()
	r.str() // description
	r.u32() // member slots
	r.u16() // port
	r.str() // preferred game name
	r.str() // host username
	n := r.u32()
	if n > 1024 {
		return
	}
	var ms []RoomMemberInfo
	for i := uint32(0); i < n && !r.bad; i++ {
		var m RoomMemberInfo
		m.Nickname = r.str()
		m.FakeIP = r.ip()
		m.GameName = r.str()
		r.u64()
		r.str() // version
		r.str() // username
		r.str() // display name
		r.str() // avatar
		ms = append(ms, m)
	}
	if r.bad {
		return
	}
	rc.Members = ms
	if rc.OnMembers != nil {
		rc.OnMembers(ms)
	}
}

func (rc *RoomClient) loop() {
	for {
		select {
		case p := <-rc.enet.Recv:
			if len(p) == 0 {
				continue
			}
			switch p[0] {
			case idLdnPacket:
				r := &rbuf{b: p, off: 1}
				var l LDNPacket
				l.Type = r.u8()
				l.LocalIP = r.ip()
				l.RemoteIP = r.ip()
				l.Broadcast = r.boolean()
				l.Data = r.vec()
				if !r.bad && rc.OnLDN != nil {
					rc.OnLDN(l)
				}
			case idProxyPacket:
				r := &rbuf{b: p, off: 1}
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
				if !r.bad && rc.OnProxy != nil {
					rc.OnProxy(x)
				}
			case idRoomInformation:
				rc.parseRoomInfo(p)
			case idChatMessage:
				r := &rbuf{b: p, off: 1}
				nick := r.str()
				r.str()
				msg := r.str()
				if rc.OnChat != nil && !r.bad {
					rc.OnChat(nick, msg)
				}
			case idCloseRoom:
				rc.finish(errors.New("el anfitrión cerró la sala"))
				return
			case idHostKicked:
				rc.finish(errors.New("te han expulsado de la sala"))
				return
			case idHostBanned:
				rc.finish(errors.New("te han baneado de la sala"))
				return
			}
		case <-rc.enet.Closed:
			rc.finish(rc.enet.Err())
			return
		}
	}
}

func (rc *RoomClient) finish(err error) {
	select {
	case <-rc.Done:
	default:
		rc.DoneErr = err
		close(rc.Done)
		rc.enet.Close()
	}
}

func (rc *RoomClient) MyIP() IPv4 { return rc.FakeIP }

func (rc *RoomClient) SetHandlers(onLDN func(LDNPacket), onProxy func(ProxyPacket)) {
	rc.OnLDN, rc.OnProxy = onLDN, onProxy
}

func (rc *RoomClient) Leave() { rc.finish(errors.New("saliendo")) }

func (rc *RoomClient) SendLDN(l LDNPacket) {
	w := &wbuf{}
	w.u8(idLdnPacket)
	w.u8(l.Type)
	w.ip(l.LocalIP)
	w.ip(l.RemoteIP)
	w.boolean(l.Broadcast)
	w.vec(l.Data)
	rc.enet.Send(w.b)
}

func (rc *RoomClient) SendProxy(x ProxyPacket) {
	w := &wbuf{}
	w.u8(idProxyPacket)
	w.u8(1) // Domain::INET
	w.ip(x.Local.IP)
	w.u16(x.Local.Port)
	w.u8(1)
	w.ip(x.Remote.IP)
	w.u16(x.Remote.Port)
	w.u8(x.Protocol)
	w.boolean(x.Broadcast)
	w.vec(x.Data)
	rc.enet.Send(w.b)
}

func (rc *RoomClient) SendChat(msg string) {
	w := &wbuf{}
	w.u8(idChatMessage)
	w.str(msg)
	rc.enet.Send(w.b)
}
