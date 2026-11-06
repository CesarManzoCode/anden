// Command mkimage writes OCI image layouts for the acceptance criteria and
// prints the manifest digest on stdout.
//
// Usage:
//
//	mkimage synthetic --out DIR [--tag TAG]
//	mkimage pack --bin PATH --out DIR [--tag TAG] [--cmd ARG]... [--env KEY=VALUE]...
//
// synthetic writes two layers with files, directories, a symlink, a hardlink,
// special modes, whiteouts and an opaque directory. pack writes one layer with
// a binary at /usr/bin/<name> and the libraries ldd reports; without --cmd, Cmd
// is that binary.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

const usage = "usage: mkimage synthetic|pack --out DIR [--tag TAG] [--bin PATH] [--cmd ARG]... [--env KEY=VALUE]..."

type stringList []string

func (l *stringList) String() string {
	return strings.Join(*l, " ")
}

func (l *stringList) Set(value string) error {
	*l = append(*l, value)
	return nil
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "mkimage:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	flags := flag.NewFlagSet("mkimage "+args[0], flag.ContinueOnError)
	out := flags.String("out", "", "layout directory to create")
	tag := flags.String("tag", "latest", "reference name in index.json")
	var img image
	switch args[0] {
	case "synthetic":
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		img = syntheticImage()
	case "pack":
		bin := flags.String("bin", "", "binary to pack")
		var cmd, env stringList
		flags.Var(&cmd, "cmd", "one element of Cmd, repeatable")
		flags.Var(&env, "env", "one element of Env, repeatable")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if *bin == "" {
			return errors.New("pack needs --bin")
		}
		libs, err := lddLibraries(*bin)
		if err != nil {
			return err
		}
		if img, err = packImage(*bin, libs, cmd, env); err != nil {
			return err
		}
	default:
		return errors.New(usage)
	}
	if *out == "" || flags.NArg() != 0 {
		return errors.New(usage)
	}
	digest, err := writeLayout(*out, *tag, img)
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, digest)
	return nil
}
