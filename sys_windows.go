// SPDX-License-Identifier: GPL-3.0-or-later
// Puente Eden <-> Switch (ldn_mitm)

//go:build windows

package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

const (
	sioUDPConnReset = 0x9800000C
	sioRcvAll       = 0x98000001
)

func listenUDPShared(port uint16) (*net.UDPConn, error) { return listenUDPSharedAddr("0.0.0.0", port) }

func listenUDPSharedAddr(host string, port uint16) (*net.UDPConn, error) {
	lc := net.ListenConfig{Control: func(network, address string, c syscall.RawConn) error {
		var serr error
		c.Control(func(fd uintptr) {
			h := syscall.Handle(fd)
			serr = syscall.SetsockoptInt(h, syscall.SOL_SOCKET, syscall.SO_BROADCAST, 1)
			syscall.SetsockoptInt(h, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
			// stop ICMP "port unreachable" from breaking reads
			var flag uint32 = 0
			var ret uint32
			syscall.WSAIoctl(h, sioUDPConnReset, (*byte)(unsafe.Pointer(&flag)), 4, nil, 0, &ret, nil, 0)
		})
		return serr
	}}
	pc, err := lc.ListenPacket(context.Background(), "udp4", fmt.Sprintf("%s:%d", host, port))
	if err != nil {
		return nil, err
	}
	return pc.(*net.UDPConn), nil
}

func isAdmin() bool {
	f, err := os.Open(`\\.\PHYSICALDRIVE0`)
	if err != nil {
		return false
	}
	f.Close()
	return true
}

func needElevation() bool { return !isAdmin() }

func relaunchElevated() bool { return runElevated(strings.Join(os.Args[1:], " ")) }

func runElevated(args string) bool {
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	shell32 := syscall.NewLazyDLL("shell32.dll")
	proc := shell32.NewProc("ShellExecuteW")
	verb, _ := syscall.UTF16PtrFromString("runas")
	file, _ := syscall.UTF16PtrFromString(exe)
	params, _ := syscall.UTF16PtrFromString(args)
	r, _, _ := proc.Call(0, uintptr(unsafe.Pointer(verb)), uintptr(unsafe.Pointer(file)), uintptr(unsafe.Pointer(params)), 0, 1)
	return r > 32
}

func setupFirewall() {
	if !isAdmin() {
		logf("Aviso: sin permisos de administrador. Si Windows pregunta por el Firewall, pulsa PERMITIR.")
		return
	}
	exe, err := os.Executable()
	if err != nil {
		return
	}
	// Remove any rule for this program (including "block" rules created when the
	// Windows Firewall prompt was dismissed) and allow it on every network profile.
	hiddenCmd("netsh", "advfirewall", "firewall", "delete", "rule", "name=all", "program="+exe).Run()
	hiddenCmd("netsh", "advfirewall", "firewall", "delete", "rule", "name=Puente Eden Switch").Run()
	err1 := hiddenCmd("netsh", "advfirewall", "firewall", "add", "rule", "name=Puente Eden Switch", "dir=in", "action=allow",
		"program="+exe, "enable=yes", "profile=any").Run()
	err2 := hiddenCmd("netsh", "advfirewall", "firewall", "add", "rule", "name=Puente Eden Switch", "dir=out", "action=allow",
		"program="+exe, "enable=yes", "profile=any").Run()
	if err1 == nil && err2 == nil {
		logf("Firewall de Windows: permitido")
	} else {
		logf("Aviso: no pude configurar el Firewall de Windows (%v %v)", err1, err2)
	}
}

// startSniffer opens a raw socket in promiscuous "receive all" mode on the LAN
// interface to see game packets sent by the Switch to ports we do not listen on yet.
func startSniffer(lanIP IPv4, cb func(src IPv4, sport, dport uint16, data []byte)) error {
	s, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_RAW, 0)
	if err != nil {
		return err
	}
	sa := &syscall.SockaddrInet4{Addr: lanIP}
	if err := syscall.Bind(s, sa); err != nil {
		syscall.Closesocket(s)
		return err
	}
	var on uint32 = 1 // RCVALL_ON
	var ret uint32
	if err := syscall.WSAIoctl(s, sioRcvAll, (*byte)(unsafe.Pointer(&on)), 4, nil, 0, &ret, nil, 0); err != nil {
		syscall.Closesocket(s)
		return err
	}
	go func() {
		buf := make([]byte, 65536)
		for {
			n, _, err := syscall.Recvfrom(s, buf, 0)
			if err != nil {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			if rawTap != nil {
				rawTap(buf[:n])
			}
			parseIPv4UDP(buf[:n], lanIP, cb)
		}
	}()
	return nil
}

func parseIPv4UDP(p []byte, lanIP IPv4, cb func(src IPv4, sport, dport uint16, data []byte)) {
	if len(p) < 28 || p[0]>>4 != 4 || p[9] != 17 {
		return
	}
	ihl := int(p[0]&0x0F) * 4
	if len(p) < ihl+8 {
		return
	}
	// skip fragments other than the first
	if binary.BigEndian.Uint16(p[6:])&0x1FFF != 0 {
		return
	}
	var src, dst IPv4
	copy(src[:], p[12:16])
	copy(dst[:], p[16:20])
	if src == lanIP {
		return
	}
	if dst != lanIP && dst[3] != 255 {
		return
	}
	u := p[ihl:]
	sport := binary.BigEndian.Uint16(u[0:])
	dport := binary.BigEndian.Uint16(u[2:])
	ulen := int(binary.BigEndian.Uint16(u[4:]))
	if ulen < 8 || ulen > len(u) {
		return
	}
	cb(src, sport, dport, append([]byte(nil), u[8:ulen]...))
}

const firewallRuleName = "Puente Eden Switch"

// firewallStatus reports "ok" when an allow rule exists for this exact program.
func firewallStatus() string {
	exe, err := os.Executable()
	if err != nil {
		return "unknown"
	}
	cmd := hiddenCmd("netsh", "advfirewall", "firewall", "show", "rule", "name="+firewallRuleName, "verbose")
	out, _ := cmd.CombinedOutput()
	if strings.Contains(strings.ToLower(string(out)), strings.ToLower(exe)) {
		return "ok"
	}
	return "missing"
}

// fixFirewallElevated asks Windows (UAC) to run this program as administrator
// only to add the firewall rules.
func fixFirewallElevated() {
	runElevated("-solo-firewall")
}

// openAppWindow opens the interface in its own window (Microsoft Edge "app"
// mode, with a separate profile so closing the window ends the process).
// Returns a channel closed when the window is closed, or nil if it can't tell.
func openAppWindow(url string) <-chan struct{} {
	var edge string
	for _, p := range []string{
		os.Getenv("ProgramFiles(x86)") + `\Microsoft\Edge\Application\msedge.exe`,
		os.Getenv("ProgramFiles") + `\Microsoft\Edge\Application\msedge.exe`,
		os.Getenv("LocalAppData") + `\Microsoft\Edge\Application\msedge.exe`,
	} {
		if _, err := os.Stat(p); err == nil {
			edge = p
			break
		}
	}
	if edge != "" {
		profile := os.Getenv("LocalAppData") + `\PuenteEdenSwitch\ventana`
		os.MkdirAll(profile, 0o755)
		cmd := exec.Command(edge, "--app="+url, "--window-size=1060,800", "--user-data-dir="+profile,
			"--no-first-run", "--no-default-browser-check", "--disable-features=Translate",
			"--hide-crash-restore-bubble", "--disable-session-crashed-bubble")
		if err := cmd.Start(); err == nil {
			appWindow = cmd
			started := time.Now()
			done := make(chan struct{})
			go func() {
				cmd.Wait()
				// If Edge handed the window to an already running instance it
				// exits at once; then we can't tell when the window closes.
				if time.Since(started) > 5*time.Second {
					close(done)
				}
			}()
			return done
		}
	}
	exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	return nil
}

func hiddenCmd(name string, args ...string) *exec.Cmd {
	c := exec.Command(name, args...)
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
	return c
}

var appWindow *exec.Cmd

func closeAppWindow() {
	if appWindow != nil && appWindow.Process != nil {
		appWindow.Process.Kill()
	}
}

// showConsole gives the GUI-built program a console window (for -consola).
func showConsole() {
	k := syscall.NewLazyDLL("kernel32.dll")
	if r, _, _ := k.NewProc("AttachConsole").Call(uintptr(0xFFFFFFFF)); r == 0 {
		k.NewProc("AllocConsole").Call()
	}
	if f, err := os.OpenFile("CONOUT$", os.O_RDWR, 0); err == nil {
		os.Stdout, os.Stderr = f, f
	}
	if f, err := os.OpenFile("CONIN$", os.O_RDWR, 0); err == nil {
		os.Stdin = f
		stdin = bufio.NewReader(f)
	}
}
