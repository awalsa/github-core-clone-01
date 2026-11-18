// profile.go — temporary diagnostic for the hotpath round: per-route
// lock-wait vs hold time, plus transport parse/write time. Aggregates are
// dumped to $HOTPATH_PROFILE (default /tmp/hotpath-profile.json) every 500
// records and on SIGTERM (see main.go). NOT part of the shipped design;
// remove before any submit.

package main

import (
	"encoding/json"
	"os"
	"sync"
	"sync/atomic"
)

type profStat struct {
	N       int64 `json:"n"`
	WaitUs  int64 `json:"wait_us"`
	HoldUs  int64 `json:"hold_us"`
	ParseUs int64 `json:"parse_us"`
	WriteUs int64 `json:"write_us"`
}

var profMu sync.Mutex
var profStats = map[string]*profStat{}
var profCount int64

func profStatFor(route string) *profStat {
	st, ok := profStats[route]
	if !ok {
		st = &profStat{}
		profStats[route] = st
	}
	return st
}

// profRecord logs one handled request: lock-wait and hold time in µs.
func profRecord(route string, waitUs, holdUs int64) {
	profMu.Lock()
	st := profStatFor(route)
	st.N++
	st.WaitUs += waitUs
	st.HoldUs += holdUs
	profMu.Unlock()
	if atomic.AddInt64(&profCount, 1)%500 == 0 {
		profDump()
	}
}

// profRecordIO logs transport-side time (kind "parse" or "write").
func profRecordIO(kind string, us int64) {
	profMu.Lock()
	st := profStatFor("_io")
	if kind == "parse" {
		st.ParseUs += us
	} else {
		st.WriteUs += us
	}
	st.N++
	profMu.Unlock()
}

func profDump() {
	profMu.Lock()
	snap := make(map[string]profStat, len(profStats))
	for k, v := range profStats {
		snap[k] = *v
	}
	profMu.Unlock()
	b, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return
	}
	path := os.Getenv("HOTPATH_PROFILE")
	if path == "" {
		path = "/tmp/hotpath-profile.json"
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0644); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}
