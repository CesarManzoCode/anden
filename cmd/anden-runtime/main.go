// Command anden-runtime is Andén's OCI runtime: create, start, state, kill
// and delete as the runtime-tools command line interface defines them.
package main

import (
	"os"
	"runtime"

	ociruntime "github.com/CesarManzoCode/anden/internal/runtime"
)

// init pins the main goroutine to the main thread before main runs. The
// container init re-executes this binary and changes namespaces,
// credentials and capabilities per thread, so all of it must happen on the
// thread that finally calls exec.
func init() {
	runtime.LockOSThread()
}

func main() {
	os.Exit(ociruntime.Main(os.Args[1:], os.Stdout, os.Stderr))
}
