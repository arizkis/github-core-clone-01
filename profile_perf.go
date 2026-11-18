//go:build perfprofile

package main

import (
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"runtime"
)

// Diagnostic builds only; never included in the shipped binary.
func init() {
	if address := os.Getenv("FORGE_PPROF_ADDR"); address != "" {
		runtime.SetMutexProfileFraction(1)
		runtime.SetBlockProfileRate(1)
		listener, err := net.Listen("tcp", address)
		if err != nil {
			panic(err)
		}
		go http.Serve(listener, nil)
	}
}
