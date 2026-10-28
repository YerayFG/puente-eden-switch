// SPDX-License-Identifier: GPL-3.0-or-later
// Puente Eden <-> Switch (ldn_mitm)

// mkrsrc writes a Windows resource object (.syso) with the program icon and
// manifest, so `go build` embeds them in the .exe. No external tools needed.
//
//	go run ./tools/mkrsrc -o rsrc_windows_amd64.syso assets/icon-256.png assets/icon-64.png ...
package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"image/png"
	"os"
	"sort"
)

const manifest = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<assembly xmlns="urn:schemas-microsoft-com:asm.v1" manifestVersion="1.0">
  <assemblyIdentity type="win32" name="PuenteEdenSwitch" version="1.0.0.0" processorArchitecture="*"/>
  <description>Puente Eden - Switch (ldn_mitm)</description>
  <trustInfo xmlns="urn:schemas-microsoft-com:asm.v3">
    <security><requestedPrivileges><requestedExecutionLevel level="asInvoker" uiAccess="false"/></requestedPrivileges></security>
  </trustInfo>
  <compatibility xmlns="urn:schemas-microsoft-com:compatibility.v1">
    <application><supportedOS Id="{8e0f7a12-bfb3-4fe8-b9a5-48fd50a15a9a}"/></application>
  </compatibility>
  <application xmlns="urn:schemas-microsoft-com:asm.v3">
    <windowsSettings>
      <dpiAware xmlns="http://schemas.microsoft.com/SMI/2005/WindowsSettings">true/pm</dpiAware>
      <dpiAwareness xmlns="http://schemas.microsoft.com/SMI/2016/WindowsSettings">PerMonitorV2</dpiAwareness>
    </windowsSettings>
  </application>
</assembly>
`

const (
	rtIcon      = 3
	rtGroupIcon = 14
	rtManifest  = 24
	langID      = 0x0409
)

type res struct {
	typ, id uint32
	data    []byte
}

func main() {
	out := flag.String("o", "rsrc_windows_amd64.syso", "output file")
	flag.Parse()

	var resources []res
	grp := &bytes.Buffer{}
	binary.Write(grp, binary.LittleEndian, [3]uint16{0, 1, uint16(flag.NArg())})
	for i, path := range flag.Args() {
		data, err := os.ReadFile(path)
		check(err)
		img, err := png.Decode(bytes.NewReader(data))
		check(err)
		w, h := img.Bounds().Dx(), img.Bounds().Dy()
		if w > 256 || h > 256 {
			check(fmt.Errorf("%s: el icono no puede pasar de 256x256", path))
		}
		id := uint32(i + 1)
		resources = append(resources, res{rtIcon, id, data})
		grp.WriteByte(byte(w % 256))
		grp.WriteByte(byte(h % 256))
		grp.WriteByte(0)
		grp.WriteByte(0)
		binary.Write(grp, binary.LittleEndian, uint16(1))
		binary.Write(grp, binary.LittleEndian, uint16(32))
		binary.Write(grp, binary.LittleEndian, uint32(len(data)))
		binary.Write(grp, binary.LittleEndian, uint16(id))
	}
	if flag.NArg() > 0 {
		resources = append(resources, res{rtGroupIcon, 1, grp.Bytes()})
	}
	resources = append(resources, res{rtManifest, 1, []byte(manifest)})
	check(os.WriteFile(*out, buildCOFF(resources), 0o644))
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// buildCOFF lays out a .rsrc section: directory tree (type -> id -> language),
// data entries, then the data, plus relocations for the data RVAs.
func buildCOFF(rs []res) []byte {
	sort.Slice(rs, func(i, j int) bool {
		if rs[i].typ != rs[j].typ {
			return rs[i].typ < rs[j].typ
		}
		return rs[i].id < rs[j].id
	})
	var types []uint32
	byType := map[uint32][]int{}
	for i, r := range rs {
		if len(byType[r.typ]) == 0 {
			types = append(types, r.typ)
		}
		byType[r.typ] = append(byType[r.typ], i)
	}
	dirSize := func(n int) int { return 16 + 8*n }

	// offsets
	off := dirSize(len(types)) // root
	typeDirOff := map[uint32]int{}
	for _, t := range types {
		typeDirOff[t] = off
		off += dirSize(len(byType[t]))
	}
	langDirOff := make([]int, len(rs))
	for i := range rs {
		langDirOff[i] = off
		off += dirSize(1)
	}
	entryOff := make([]int, len(rs))
	for i := range rs {
		entryOff[i] = off
		off += 16
	}
	dataOff := make([]int, len(rs))
	for i, r := range rs {
		off = (off + 7) &^ 7
		dataOff[i] = off
		off += len(r.data)
	}
	sec := make([]byte, (off+7)&^7)
	le := binary.LittleEndian
	putDir := func(at, n int) { le.PutUint16(sec[at+14:], uint16(n)) }
	putEnt := func(at int, id uint32, target int, sub bool) {
		le.PutUint32(sec[at:], id)
		v := uint32(target)
		if sub {
			v |= 0x80000000
		}
		le.PutUint32(sec[at+4:], v)
	}
	putDir(0, len(types))
	for i, t := range types {
		putEnt(16+8*i, t, typeDirOff[t], true)
		putDir(typeDirOff[t], len(byType[t]))
		for j, ri := range byType[t] {
			putEnt(typeDirOff[t]+16+8*j, rs[ri].id, langDirOff[ri], true)
		}
	}
	var relocs []int
	for i, r := range rs {
		putDir(langDirOff[i], 1)
		putEnt(langDirOff[i]+16, langID, entryOff[i], false)
		le.PutUint32(sec[entryOff[i]:], uint32(dataOff[i])) // RVA, relocated
		le.PutUint32(sec[entryOff[i]+4:], uint32(len(r.data)))
		relocs = append(relocs, entryOff[i])
		copy(sec[dataOff[i]:], r.data)
	}

	const hdr, shdr = 20, 40
	rawPtr := hdr + shdr
	relPtr := rawPtr + len(sec)
	symPtr := relPtr + 10*len(relocs)

	b := &bytes.Buffer{}
	binary.Write(b, le, uint16(0x8664)) // AMD64
	binary.Write(b, le, uint16(1))
	binary.Write(b, le, uint32(0))
	binary.Write(b, le, uint32(symPtr))
	binary.Write(b, le, uint32(1))
	binary.Write(b, le, uint16(0))
	binary.Write(b, le, uint16(0))
	b.Write([]byte(".rsrc\x00\x00\x00"))
	binary.Write(b, le, uint32(0))
	binary.Write(b, le, uint32(0))
	binary.Write(b, le, uint32(len(sec)))
	binary.Write(b, le, uint32(rawPtr))
	binary.Write(b, le, uint32(relPtr))
	binary.Write(b, le, uint32(0))
	binary.Write(b, le, uint16(len(relocs)))
	binary.Write(b, le, uint16(0))
	binary.Write(b, le, uint32(0x40000040)) // initialized data, read
	b.Write(sec)
	for _, r := range relocs {
		binary.Write(b, le, uint32(r))
		binary.Write(b, le, uint32(0)) // symbol 0 (.rsrc)
		binary.Write(b, le, uint16(3)) // IMAGE_REL_AMD64_ADDR32NB
	}
	b.Write([]byte(".rsrc\x00\x00\x00"))
	binary.Write(b, le, uint32(0))
	binary.Write(b, le, uint16(1))
	binary.Write(b, le, uint16(0))
	b.WriteByte(3) // IMAGE_SYM_CLASS_STATIC
	b.WriteByte(0)
	binary.Write(b, le, uint32(4)) // empty string table
	return b.Bytes()
}
