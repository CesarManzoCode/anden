// Command anden is Andén's command line. This version has the image
// subcommands of the contract (section 5).
package main

import (
	"fmt"
	"io"
	"os"
)

const usage = `usage:
  anden image import --layout DIR:TAG [--state DIR] [--json]
  anden image unpack --digest D --dest STORE [--state DIR] [--json]
  anden image mount --digest D --dest DIR [--state DIR] [--json]
  anden image verify [--state DIR] [--json]
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "image" {
		return runImage(args[1:], stdout, stderr)
	}
	fmt.Fprint(stderr, usage)
	return 2
}
