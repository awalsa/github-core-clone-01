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
	MaxUs   int64 `json:"max_us"`
	// hold-time histogram buckets: <100µs, 100-500µs, 500µs-2ms, 2-10ms, >10ms
	Hist [5]int64 `json:"hist"`
	// cache counters (fetch reply / pack build)
	Hit  int64 `json:"hit"`
	Miss int64 `json:"miss"`
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
	if holdUs > st.MaxUs {
		st.MaxUs = holdUs
	}
	switch {
	case holdUs < 100:
		st.Hist[0]++
	case holdUs < 500:
		st.Hist[1]++
	case holdUs < 2000:
		st.Hist[2]++
	case holdUs < 10000:
		st.Hist[3]++
	default:
		st.Hist[4]++
	}
	profMu.Unlock()
	if atomic.AddInt64(&profCount, 1)%500 == 0 {
		profDump()
	}
}

// profCache records a cache hit (hit=true) or miss for a named cache.
func profCache(name string, hit bool) {
	profMu.Lock()
	st := profStatFor("cache:" + name)
	if hit {
		st.Hit++
	} else {
		st.Miss++
	}
	profMu.Unlock()
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
