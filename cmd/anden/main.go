// Command anden is Andén's command line: the datacenter simulator, image
// and service subcommands of the contract (section 5).
package main

import (
	"fmt"
	"io"
	"os"
)

const usage = `usage:
  anden up [--state DIR] [--timeout D] [--json]
  anden down [--state DIR] [--json]
  anden status [--state DIR] [--json]
  anden exec --ns client|root|nodeN [--state DIR] -- CMD...
  anden chaos kill|partition|heal nodeN [--state DIR] [--json]
  anden chaos slow nodeN --delay-ms N [--state DIR] [--json]
  anden image import --layout DIR:TAG [--state DIR] [--json]
  anden image unpack --digest D --dest STORE [--state DIR] [--json]
  anden image mount --digest D --dest DIR [--state DIR] [--json]
  anden image verify [--state DIR] [--json]
  anden svc create|deploy --name N --image D --port P --cpu-millis C --memory-mib M [spec flags] [--state DIR] [--json]
  anden svc get|sleep|wake|delete --name N [--state DIR] [--json]
  anden svc move --name N --node NODE [--state DIR] [--json]
  anden svc list [--state DIR] [--json]
  anden placement solve --input FILE [--json]
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "image" {
		return runImage(args[1:], stdout, stderr)
	}
	if len(args) > 0 && args[0] == "svc" {
		return runSvc(args[1:], stdout, stderr)
	}
	if len(args) > 0 && args[0] == "placement" {
		return runPlacement(args[1:], stdout, stderr)
	}
	if len(args) > 0 {
		switch args[0] {
		case "up", "down", "status", "exec", "chaos", "_dc", "_dc-init":
			return runDC(args[0], args[1:], stdout, stderr)
		}
	}
	fmt.Fprint(stderr, usage)
	return 2
}
