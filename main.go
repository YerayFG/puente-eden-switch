// SPDX-License-Identifier: GPL-3.0-or-later
// Puente Eden <-> Switch (ldn_mitm)

package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const version = "1.0.0"

var (
	verbose  bool
	logMu    sync.Mutex
	onceSeen = map[string]string{}
)

func logf(format string, a ...any) {
	logMu.Lock()
	defer logMu.Unlock()
	msg := fmt.Sprintf(format, a...)
	if !guiMode {
		fmt.Printf("[%s] %s\n", time.Now().Format("15:04:05"), msg)
	}
	go pushLog(msg)
}

func logv(format string, a ...any) {
	if verbose {
		logf(format, a...)
	}
}

func logOnce(k, msg string) { logOnceKey(k, msg, "%s", msg) }

func logOnceKey(k, val, format string, a ...any) {
	logMu.Lock()
	prev, ok := onceSeen[k]
	onceSeen[k] = val
	logMu.Unlock()
	if !ok || prev != val {
		logf(format, a...)
	} else {
		logv(format, a...)
	}
}

func isClosedErr(err error) bool { return errors.Is(err, net.ErrClosed) }

func parsePorts(s string) ([]uint16, error) {
	var out []uint16
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if a, bb, ok := strings.Cut(part, "-"); ok {
			lo, err1 := strconv.Atoi(a)
			hi, err2 := strconv.Atoi(bb)
			if err1 != nil || err2 != nil || lo < 1 || hi > 65535 || lo > hi || hi-lo > 2000 {
				return nil, fmt.Errorf("rango de puertos no válido: %s", part)
			}
			for p := lo; p <= hi; p++ {
				out = append(out, uint16(p))
			}
			continue
		}
		p, err := strconv.Atoi(part)
		if err != nil || p < 1 || p > 65535 {
			return nil, fmt.Errorf("puerto no válido: %s", part)
		}
		out = append(out, uint16(p))
	}
	return out, nil
}

// detectLAN returns this PC's LAN IPv4, its broadcast address and all local IPv4s.
func detectLAN(force string) (IPv4, IPv4, map[IPv4]bool, error) {
	own := map[IPv4]bool{{127, 0, 0, 1}: true}
	type cand = lanCand
	var cands []cand
	ifs, _ := net.Interfaces()
	for _, itf := range ifs {
		addrs, _ := itf.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.To4() == nil {
				continue
			}
			ip := netIPv4(ipn.IP)
			own[ip] = true
			if itf.Flags&net.FlagUp == 0 || itf.Flags&net.FlagLoopback != 0 {
				continue
			}
			m := ipn.Mask
			if len(m) == 16 {
				m = m[12:]
			}
			var bc IPv4
			for i := 0; i < 4; i++ {
				bc[i] = ip[i] | ^m[i]
			}
			var mask IPv4
			copy(mask[:], m)
			cands = append(cands, cand{ip, bc, mask})
		}
	}
	var want IPv4
	if force != "" {
		p := net.ParseIP(force)
		if p == nil || p.To4() == nil {
			return IPv4{}, IPv4{}, nil, fmt.Errorf("IP no válida: %s", force)
		}
		want = netIPv4(p)
	} else if c, err := net.Dial("udp4", "8.8.8.8:80"); err == nil {
		want = netIPv4(c.LocalAddr().(*net.UDPAddr).IP)
		c.Close()
	}
	lanCands = cands
	for _, c := range cands {
		if c.ip == want && (force != "" || c.prefix() <= 30) {
			return c.ip, c.bc, own, nil
		}
	}
	if force != "" {
		own[want] = true
		return want, IPv4{255, 255, 255, 255}, own, nil
	}
	for _, first := range []byte{192, 10, 172} {
		for _, c := range cands {
			if c.ip[0] == first && c.prefix() <= 30 {
				return c.ip, c.bc, own, nil
			}
		}
	}
	return IPv4{}, IPv4{}, nil, errors.New("no encuentro la red local del PC; indica tu IP con -ip")
}

var stdin = bufio.NewReader(os.Stdin)

var guiMode bool

// fatal shows an error. In the graphical interface the program stays open so
// the user can read it.
func fatal(msg string) {
	if guiMode {
		logf("Error: %s", msg)
		statusUpdate(func(s *Status) { s.RoomError = msg })
		select {}
	}
	pause("Error: " + msg)
	os.Exit(1)
}

func pause(msg string) {
	if msg != "" {
		fmt.Println(msg)
	}
	fmt.Print("Pulsa Enter para continuar...")
	stdin.ReadString('\n')
}

func main() {
	roomAddr := flag.String("sala", "", "unirse a una sala de Eden ya creada (dirección:puerto) en vez de crear una")
	roomPort := flag.Int("puerto-sala", 24872, "puerto de la sala que crea este programa")
	nick := flag.String("nombre", "Switch", "nombre con el que la Switch aparece en la sala")
	pass := flag.String("password", "", "contraseña de la sala (si tiene)")
	switchIP := flag.String("switch", "", "IP de la Switch (opcional; se detecta sola)")
	lanIPFlag := flag.String("ip", "", "IP de este PC en la red local (opcional)")
	ports := flag.String("puertos", "", "puertos UDP del juego a abrir de antemano, ej: 49150-49160")
	nosniff := flag.Bool("sin-detector", false, "no usar el detector de puertos (no pide administrador)")
	flag.BoolVar(&verbose, "v", false, "mostrar mensajes detallados")
	bindLAN := flag.Bool("bind-lan", false, "(pruebas) escuchar el puerto 11452 solo en la IP indicada con -ip")
	consola := flag.Bool("consola", false, "usar la consola en vez de la ventana")
	gui := flag.Bool("gui", false, "forzar la ventana (pruebas)")
	soloFirewall := flag.Bool("solo-firewall", false, "solo configurar el Firewall de Windows y salir")
	flag.Parse()

	if *soloFirewall {
		setupFirewall()
		return
	}
	guiMode = (runtime.GOOS == "windows" && !*consola) || *gui
	if !guiMode && runtime.GOOS == "windows" {
		showConsole()
	}
	if guiMode && !isAdmin() {
		*nosniff = true // the port detector needs administrator rights
	}

	if !guiMode {
		fmt.Printf("Puente Eden <-> Switch (ldn_mitm)  v%s\n", version)
		fmt.Println("======================================================")
	}

	if !guiMode && !*nosniff && needElevation() {
		if relaunchElevated() {
			return
		}
		fmt.Println("Aviso: sin permisos de administrador el detector de puertos no funciona.")
	}

	lanIP, lanBC, own, err := detectLAN(*lanIPFlag)
	b := NewBridge(lanIP, lanBC, own)
	if guiMode {
		url, uerr := startUI(b)
		if uerr != nil {
			// Already running: just show the existing window.
			openAppWindow(fmt.Sprintf("http://127.0.0.1:%d/", uiPort))
			return
		}
		if closed := openAppWindow(url); closed != nil {
			go func() {
				<-closed
				os.Exit(0)
			}()
		}
	}
	if isAdmin() {
		setupFirewall()
	}
	go func() {
		for {
			fw := firewallStatus()
			statusUpdate(func(s *Status) { s.Firewall = fw; s.Admin = isAdmin() })
			if fw != "missing" {
				return
			}
			time.Sleep(4 * time.Second)
		}
	}()
	if err != nil {
		fatal(err.Error())
	}
	logf("PC en la red local: %s (difusión %s)", lanIP, lanBC)
	statusUpdate(func(s *Status) { s.PCIP = lanIP.String() })
	b.lanFixed = *lanIPFlag != ""
	if *switchIP != "" {
		p := net.ParseIP(*switchIP)
		if p == nil || p.To4() == nil {
			fatal("IP de la Switch no válida")
		}
		b.swIP, b.swKnown, b.swFixed = netIPv4(p), true, true
		logf("IP de la Switch fijada: %s", b.swIP)
		b.pickLANForSwitchLocked()
	}
	var bindIP *IPv4
	if *bindLAN {
		bindIP = &lanIP
		gameBindHost = net.IP(lanIP[:]).String()
	}
	for {
		err := b.StartLAN(bindIP)
		if err == nil {
			break
		}
		logf("No puedo abrir el puerto %d: %v", lanPort, err)
		logf("¿Tienes otra ventana del puente abierta? Ciérrala. Reintento en 3 s…")
		statusUpdate(func(s *Status) {
			s.RoomError = fmt.Sprintf("El puerto %d está ocupado. ¿Hay otro puente abierto?", lanPort)
		})
		time.Sleep(3 * time.Second)
	}
	if *ports != "" {
		pl, err := parsePorts(*ports)
		if err != nil {
			fatal(err.Error())
		}
		b.data.prebind(pl)
	}
	if !*nosniff {
		startDiag(b)
		if err := startSniffer(lanIP, b.data.sniffed); err != nil {
			logf("Aviso: detector de puertos no disponible (%v)", err)
		} else {
			logf("Detector de puertos: activo")
		}
		b.onLANChange = func(ip IPv4) {
			if err := startSniffer(ip, b.data.sniffed); err != nil {
				logf("Aviso: detector de puertos no disponible (%v)", err)
			}
		}
	}

	go func() {
		last := ""
		for range time.Tick(30 * time.Second) {
			s := b.data.Stats()
			if s != last {
				logf("%s", s)
				last = s
			}
		}
	}()

	statusUpdate(func(s *Status) { s.RoomPort = *roomPort })
	if *roomAddr != "" {
		statusUpdate(func(s *Status) { s.RoomMode = "client" })
	}
	if *roomAddr == "" {
		var rs *RoomServer
		for {
			var err error
			rs, err = NewRoomServer(uint16(*roomPort), "Switch + Eden", *pass, *nick, func() IPv4 {
				b.mu.Lock()
				defer b.mu.Unlock()
				return b.lanIP
			})
			if err == nil {
				break
			}
			logf("No puedo crear la sala en el puerto %d (%v).", *roomPort, err)
			statusUpdate(func(s *Status) {
				s.RoomError = "El puerto de la sala está ocupado. Si Eden tiene una sala creada, ciérrala (Multijugador > Abandonar sala)."
			})
			logf("Casi seguro que Eden tiene una sala creada: en Eden, Multijugador > Abandonar/Cerrar sala. Reintento en 3 s…")
			time.Sleep(3 * time.Second)
		}
		b.onSwitchIP = rs.SetSwitchIP
		if sw, ok := b.switchIP(); ok {
			rs.SetSwitchIP(sw)
		}
		b.setRoom(rs)
		statusUpdate(func(s *Status) { s.RoomReady, s.RoomError, s.RoomPort = true, "", *roomPort })
		logf("Sala creada en el puerto %d.", *roomPort)
		logf("En Eden: Multijugador > Conexión directa (Direct Connect) > IP 127.0.0.1, puerto %d.", *roomPort)
		logf("Luego abre el juego en la Switch y en Eden y usa el modo \"juego local / inalámbrico\".")
		select {}
	}

	for {
		logf("Conectando a la sala de Eden en %s como %q…", *roomAddr, *nick)
		room, err := JoinRoom(*roomAddr, *nick, *pass)
		if err != nil {
			logf("No se pudo entrar en la sala: %v", err)
			logf("En Eden: Multijugador → Crear sala (o entra en una), y deja este programa abierto. Reintento en 5 s…")
			time.Sleep(5 * time.Second)
			continue
		}
		logf("Dentro de la sala %q. IP virtual de la Switch: %s", room.RoomName, room.FakeIP)
		statusUpdate(func(s *Status) { s.RoomReady, s.RoomError = true, "" })
		room.OnMembers = func(ms []RoomMemberInfo) {
			var names []string
			for _, m := range ms {
				g := m.GameName
				if g == "" {
					g = "sin juego"
				}
				names = append(names, fmt.Sprintf("%s (%s, %s)", m.Nickname, m.FakeIP, g))
			}
			logOnceKey("members", strings.Join(names, ", "), "Jugadores en la sala: %s", strings.Join(names, ", "))
		}
		room.OnMembers(room.Members)
		room.OnChat = func(n, m string) { logf("chat %s: %s", n, m) }
		b.setRoom(room)
		logf("Listo. Ahora abre el juego en la Switch y en Eden y usa el modo \"juego local / inalámbrico\".")
		<-room.Done
		logf("Desconectado de la sala: %v", room.DoneErr)
		statusUpdate(func(s *Status) { s.RoomReady = false })
		b.setRoom(nil)
		b.dropSessions()
		time.Sleep(2 * time.Second)
	}
}

type lanCand struct {
	ip   IPv4
	bc   IPv4
	mask IPv4
}

func (c lanCand) prefix() int {
	n := 0
	for _, b := range c.mask {
		for i := 7; i >= 0; i-- {
			if b&(1<<i) != 0 {
				n++
			}
		}
	}
	return n
}

func (c lanCand) contains(ip IPv4) bool {
	for i := 0; i < 4; i++ {
		if ip[i]&c.mask[i] != c.ip[i]&c.mask[i] {
			return false
		}
	}
	return true
}

var lanCands []lanCand
