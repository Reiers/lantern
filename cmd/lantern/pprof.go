package main

import (
	"net/http"
	"net/http/pprof"
	"os"
)

// pprofEnabled reports whether the operator opted into the Go profiler
// endpoints on the metrics listener (LANTERN_PPROF=1). Off by default: the
// profiler exposes goroutine stacks and heap contents, so it is only meant
// for a loopback metrics listener during a performance investigation.
func pprofEnabled() bool { return os.Getenv("LANTERN_PPROF") == "1" }

// registerPprof mounts net/http/pprof under /debug/pprof/ on mux.
func registerPprof(mux *http.ServeMux) {
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
}
