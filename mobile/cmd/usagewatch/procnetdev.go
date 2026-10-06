package main

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/s-rakim/my-personal-project/mobile/internal/budget"
)

// readProcNetDev reads cumulative per-interface byte counters from
// /proc/net/dev.
//
// The format is two header lines then one line per interface:
//
//	eth0: 1234567 8901 0 0 0 0 0 0 7654321 ...
//	       ^rx bytes                 ^tx bytes (9th field)
//
// Loopback is skipped: its traffic never crosses any link and would inflate the
// free-bytes total with purely local chatter.
func readProcNetDev(metered map[string]bool) ([]budget.Sample, error) {
	f, err := os.Open("/proc/net/dev")
	if err != nil {
		return nil, fmt.Errorf("read /proc/net/dev: %w (this reader is Linux-only)", err)
	}
	defer f.Close()

	now := time.Now().UTC()
	var out []budget.Sample

	sc := bufio.NewScanner(f)
	line := 0
	for sc.Scan() {
		line++
		if line <= 2 {
			continue // skip the two header rows
		}
		text := sc.Text()
		colon := strings.IndexByte(text, ':')
		if colon < 0 {
			continue
		}
		name := strings.TrimSpace(text[:colon])
		if name == "lo" {
			continue
		}
		fields := strings.Fields(text[colon+1:])
		if len(fields) < 9 {
			continue
		}
		rx, err1 := strconv.ParseUint(fields[0], 10, 64)
		tx, err2 := strconv.ParseUint(fields[8], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		// A never-used interface reads all zeros; keep it, since diff() handles
		// zero windows and its presence documents that the interface exists.
		out = append(out, budget.Sample{
			At:      now,
			Link:    name,
			Metered: metered[name],
			RxBytes: rx,
			TxBytes: tx,
		})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no usable interfaces found in /proc/net/dev")
	}
	return out, nil
}
