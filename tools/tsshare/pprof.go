//go:build pprof

package main

import (
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
)

// Built with -tags pprof only: TSSHARE_PPROF=127.0.0.1:6060 serves
// /debug/pprof for profiling runs (see docs/PERF.md). Release builds do not
// link net/http.
func init() {
	if addr := os.Getenv("TSSHARE_PPROF"); addr != "" {
		go func() { log.Println(http.ListenAndServe(addr, nil)) }()
	}
}
