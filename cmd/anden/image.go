package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/CesarManzoCode/anden/internal/image"
)

const (
	defaultState = ".run/dc"
	// idmapEnv marks the copy of anden that already runs under the id mapping.
	idmapEnv = "ANDEN_IDMAPPED"
)

func runImage(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	cmd := args[0]
	flags := flag.NewFlagSet("anden image "+cmd, flag.ContinueOnError)
	flags.SetOutput(stderr)
	state := flags.String("state", defaultState, "state directory; the registry is STATE/registry")
	jsonOut := flags.Bool("json", false, "print a JSON object on stdout")
	var layout, digest, dest *string
	switch cmd {
	case "import":
		layout = flags.String("layout", "", "OCI layout as DIR:TAG")
	case "unpack", "mount":
		digest = flags.String("digest", "", "manifest digest")
		dest = flags.String("dest", "", "destination store (unpack) or mount point (mount)")
	case "verify":
	default:
		fmt.Fprint(stderr, usage)
		return 2
	}
	if err := flags.Parse(args[1:]); err != nil {
		return 2
	}
	if flags.NArg() != 0 || (layout != nil && *layout == "") || (digest != nil && (*digest == "" || *dest == "")) {
		fmt.Fprint(stderr, usage)
		return 2
	}
	// Whiteouts need mknod and the overlay needs a mount namespace: both
	// commands run as root of a user namespace with the id mapping.
	if (cmd == "unpack" || cmd == "mount") && os.Geteuid() != 0 {
		return reexecIDMapped(args, stderr)
	}

	result, err := imageCommand(cmd, filepath.Join(*state, "registry"), layout, digest, dest)
	if err != nil {
		fmt.Fprintln(stderr, "anden:", err)
		if result == nil {
			return 1
		}
	}
	if *jsonOut {
		out, merr := json.Marshal(result)
		if merr != nil {
			fmt.Fprintln(stderr, "anden:", merr)
			return 1
		}
		fmt.Fprintln(stdout, string(out))
	} else if d, ok := result["digest"]; ok {
		fmt.Fprintln(stdout, d)
	}
	if err != nil {
		return 1
	}
	return 0
}

// imageCommand returns the JSON result. verify returns it along with an
// error when a blob does not match.
func imageCommand(cmd, registry string, layout, digest, dest *string) (map[string]any, error) {
	store, err := image.Open(registry)
	if err != nil {
		return nil, err
	}
	switch cmd {
	case "import":
		i := strings.LastIndex(*layout, ":")
		if i <= 0 || i == len(*layout)-1 {
			return nil, fmt.Errorf("--layout %q is not DIR:TAG", *layout)
		}
		d, err := store.Import((*layout)[:i], (*layout)[i+1:])
		if err != nil {
			return nil, err
		}
		return map[string]any{"digest": d}, nil
	case "unpack":
		d, err := image.ParseDigest(*digest)
		if err != nil {
			return nil, err
		}
		target, err := image.Open(*dest)
		if err != nil {
			return nil, err
		}
		img, err := target.Unpack(store, d)
		if err != nil {
			return nil, err
		}
		layers := make([]image.Digest, 0, len(img.Layers))
		for _, l := range img.Layers {
			layers = append(layers, l.DiffID)
		}
		return map[string]any{"digest": d, "layers": layers}, nil
	case "mount":
		d, err := image.ParseDigest(*digest)
		if err != nil {
			return nil, err
		}
		if err := store.Mount(d, *dest, "", ""); err != nil {
			return nil, err
		}
		return map[string]any{"digest": d, "dest": *dest}, nil
	default:
		checked, mismatches, err := store.Verify()
		if err != nil {
			return nil, err
		}
		result := map[string]any{"blobs_checked": checked, "mismatches": mismatches}
		if len(mismatches) > 0 {
			return result, fmt.Errorf("%d of %d blobs do not match their digest: %s", len(mismatches), checked, strings.Join(mismatches, " "))
		}
		return result, nil
	}
}

// reexecIDMapped runs this same command again as root of a new user and
// mount namespace with the subordinate ids of the user. A mount made there
// lasts only as long as that namespace: inside the datacenter namespace no
// reexec happens and the mount stays.
func reexecIDMapped(args []string, stderr io.Writer) int {
	if os.Getenv(idmapEnv) != "" {
		fmt.Fprintln(stderr, "anden: still not root after entering the id mapping")
		return 1
	}
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintln(stderr, "anden:", err)
		return 1
	}
	unshare, err := exec.LookPath("unshare")
	if err != nil {
		fmt.Fprintln(stderr, "anden:", err)
		return 1
	}
	argv := append([]string{"-U", "--map-root-user", "--map-auto", "--mount", "--", self, "image"}, args...)
	c := exec.Command(unshare, argv...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	c.Env = append(os.Environ(), idmapEnv+"=1")
	if err := c.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exit.ExitCode()
		}
		fmt.Fprintln(stderr, "anden:", err)
		return 1
	}
	return 0
}
