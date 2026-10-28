// SPDX-License-Identifier: GPL-3.0-or-later
// Puente Eden <-> Switch (ldn_mitm)

package main

// Network diagnostics: summarizes every packet between this PC and the Switch
// seen by the port detector (raw socket).

import (
	"encoding/binary"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

var rawTap func([]byte)

type flowStat struct {
	n     int
	bytes int
	first time.Time
}

type diag struct {
	b     *Bridge
	mu    sync.Mutex
	flows map[string]*flowStat
	total int
}

func startDiag(b *Bridge) {
	d := &diag{b: b, flows: map[string]*flowStat{}}
	rawTap = d.packet
	go func() {
		last := ""
		for range time.Tick(15 * time.Second) {
			s := d.summary()
			if s != "" && s != last {
				logf("TRÁFICO con la Switch (últimos datos):%s", s)
				last = s
			}
		}
	}()
}

func protoName(p byte) string {
	switch p {
	case 6:
		return "TCP"
	case 17:
		return "UDP"
	case 1:
		return "ICMP"
	}
	return fmt.Sprintf("proto%d", p)
}

func (d *diag) packet(p []byte) {
	if len(p) < 20 || p[0]>>4 != 4 {
		return
	}
	sw, ok := d.b.switchIP()
	if !ok {
		return
	}
	var src, dst IPv4
	copy(src[:], p[12:16])
	copy(dst[:], p[16:20])
	if src != sw && dst != sw {
		return
	}
	ihl := int(p[0]&0x0F) * 4
	proto := p[9]
	key := ""
	if (proto == 6 || proto == 17) && len(p) >= ihl+4 && binary.BigEndian.Uint16(p[6:])&0x1FFF == 0 {
		sp := binary.BigEndian.Uint16(p[ihl:])
		dp := binary.BigEndian.Uint16(p[ihl+2:])
		key = fmt.Sprintf("%s %s:%d -> %s:%d", protoName(proto), src, sp, dst, dp)
	} else if proto == 1 && len(p) >= ihl+2 {
		key = fmt.Sprintf("ICMP %s -> %s tipo %d/%d", src, dst, p[ihl], p[ihl+1])
	} else {
		key = fmt.Sprintf("%s %s -> %s", protoName(proto), src, dst)
	}
	d.mu.Lock()
	f := d.flows[key]
	isNew := f == nil
	if isNew {
		if len(d.flows) > 200 {
			d.mu.Unlock()
			return
		}
		f = &flowStat{first: time.Now()}
		d.flows[key] = f
	}
	f.n++
	f.bytes += len(p)
	d.total++
	d.mu.Unlock()
	if isNew {
		logf("red: %s", key)
	}
}

func (d *diag) summary() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.flows) == 0 {
		return ""
	}
	keys := make([]string, 0, len(d.flows))
	for k := range d.flows {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for _, k := range keys {
		f := d.flows[k]
		fmt.Fprintf(&sb, "\n    %-55s %5d paquetes", k, f.n)
	}
	return sb.String()
}
