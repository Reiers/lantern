package handlers

import (
	"strings"
	"sync"
	"time"
)

// Rate-limited INFO logging of eth_* local misses (bridge-off visibility).
// One line per (method, to, selector, reason) per ethMissLogInterval; the
// next line for the same key reports how many were suppressed in between.

const (
	ethMissLogInterval = time.Minute
	ethMissLogMaxKeys  = 2048
)

type ethMissLogger struct {
	mu         sync.Mutex
	last       map[string]time.Time
	suppressed map[string]int
	now        func() time.Time
	emit       func(method, to, selector, reason string, bridge bool, suppressed int)
}

var ethMissLog = &ethMissLogger{
	last:       map[string]time.Time{},
	suppressed: map[string]int{},
	now:        time.Now,
	emit: func(method, to, selector, reason string, bridge bool, suppressed int) {
		next := "bridge"
		if !bridge {
			next = "error (no bridge configured)"
		}
		log.Infow("eth local miss", "method", method, "to", to, "selector", selector,
			"reason", reason, "fallback", next, "suppressedSinceLast", suppressed)
	},
}

// ethLocalMiss records a local miss for method; logs at most once per key
// per interval.
func ethLocalMiss(method, to, selector, reason string, bridge bool) {
	ethMissLog.record(method, to, selector, reason, bridge)
}

func (m *ethMissLogger) record(method, to, selector, reason string, bridge bool) {
	to = strings.ToLower(to)
	r := reason
	if len(r) > 160 {
		r = r[:160]
	}
	key := method + "|" + to + "|" + selector + "|" + r
	now := m.now()
	m.mu.Lock()
	if t, ok := m.last[key]; ok && now.Sub(t) < ethMissLogInterval {
		m.suppressed[key]++
		m.mu.Unlock()
		return
	}
	if len(m.last) >= ethMissLogMaxKeys {
		m.last = map[string]time.Time{}
		m.suppressed = map[string]int{}
	}
	n := m.suppressed[key]
	m.suppressed[key] = 0
	m.last[key] = now
	m.mu.Unlock()
	m.emit(method, to, selector, reason, bridge, n)
}

// ethSelector returns the 4-byte function selector (0x + 8 hex) of call
// data, or "" when the data is shorter than 4 bytes.
func ethSelector(data string) string {
	d := strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(data), "0x"), "0X")
	if len(d) < 8 {
		return ""
	}
	return "0x" + strings.ToLower(d[:8])
}
