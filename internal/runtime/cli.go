package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// flagDef describes one spelling of an option: the name it is stored
// under and whether it takes a value.
type flagDef struct {
	name  string
	value bool
}

var (
	globalFlags = map[string]flagDef{"root": {"root", true}}
	createFlags = map[string]flagDef{
		"bundle": {"bundle", true}, "b": {"bundle", true},
		"pid-file": {"pid-file", true}, "console-socket": {"console-socket", true},
	}
	killFlags   = map[string]flagDef{"signal": {"signal", true}, "s": {"signal", true}}
	deleteFlags = map[string]flagDef{"force": {"force", false}, "f": {"force", false}}
	noFlags     = map[string]flagDef{}
)

// parseArgs accepts options before or after positional arguments, as
// "--name value" or "--name=value"; runtime-tools puts the ID last for
// create and first for kill.
func parseArgs(args []string, defs map[string]flagDef) (map[string]string, []string, error) {
	flags := map[string]string{}
	var pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			pos = append(pos, a)
			continue
		}
		name, value, hasValue := strings.Cut(strings.TrimLeft(a, "-"), "=")
		def, ok := defs[name]
		if !ok {
			return nil, nil, fmt.Errorf("unknown option %s", a)
		}
		switch {
		case def.value && !hasValue:
			if i+1 >= len(args) {
				return nil, nil, fmt.Errorf("option %s needs a value", a)
			}
			i++
			value = args[i]
		case !def.value && hasValue:
			return nil, nil, fmt.Errorf("option %s takes no value", a)
		case !def.value:
			value = "true"
		}
		flags[def.name] = value
	}
	return flags, pos, nil
}

// splitGlobal separates the global options from the command and its
// arguments.
func splitGlobal(args []string) (map[string]string, string, []string, error) {
	i := 0
	for i < len(args) && strings.HasPrefix(args[i], "-") {
		if !strings.Contains(args[i], "=") {
			if def, ok := globalFlags[strings.TrimLeft(args[i], "-")]; ok && def.value {
				i++
			}
		}
		i++
	}
	if i > len(args) {
		return nil, "", nil, errors.New("global option without a value")
	}
	flags, _, err := parseArgs(args[:i], globalFlags)
	if err != nil {
		return nil, "", nil, err
	}
	if i == len(args) {
		return nil, "", nil, errors.New("usage: anden-runtime [--root DIR] create|start|state|kill|delete ...")
	}
	return flags, args[i], args[i+1:], nil
}

func oneID(pos []string) (string, error) {
	switch len(pos) {
	case 0:
		return "", errors.New("container id is required")
	case 1:
		return pos[0], checkID(pos[0])
	}
	return "", fmt.Errorf("unexpected arguments %q", pos[1:])
}

var signalNames = map[string]syscall.Signal{
	"HUP": syscall.SIGHUP, "INT": syscall.SIGINT, "QUIT": syscall.SIGQUIT, "ILL": syscall.SIGILL,
	"TRAP": syscall.SIGTRAP, "ABRT": syscall.SIGABRT, "IOT": syscall.SIGIOT, "BUS": syscall.SIGBUS,
	"FPE": syscall.SIGFPE, "KILL": syscall.SIGKILL, "USR1": syscall.SIGUSR1, "SEGV": syscall.SIGSEGV,
	"USR2": syscall.SIGUSR2, "PIPE": syscall.SIGPIPE, "ALRM": syscall.SIGALRM, "TERM": syscall.SIGTERM,
	"STKFLT": syscall.SIGSTKFLT, "CHLD": syscall.SIGCHLD, "CONT": syscall.SIGCONT, "STOP": syscall.SIGSTOP,
	"TSTP": syscall.SIGTSTP, "TTIN": syscall.SIGTTIN, "TTOU": syscall.SIGTTOU, "URG": syscall.SIGURG,
	"XCPU": syscall.SIGXCPU, "XFSZ": syscall.SIGXFSZ, "VTALRM": syscall.SIGVTALRM, "PROF": syscall.SIGPROF,
	"WINCH": syscall.SIGWINCH, "IO": syscall.SIGIO, "POLL": syscall.SIGPOLL, "PWR": syscall.SIGPWR,
	"SYS": syscall.SIGSYS,
}

// parseSignal accepts a name with or without SIG, or a number.
func parseSignal(s string) (syscall.Signal, error) {
	if n, err := strconv.Atoi(s); err == nil {
		if n <= 0 || n > 64 {
			return 0, fmt.Errorf("signal %d is out of range", n)
		}
		return syscall.Signal(n), nil
	}
	name := strings.TrimPrefix(strings.ToUpper(s), "SIG")
	if sig, ok := signalNames[name]; ok {
		return sig, nil
	}
	return 0, fmt.Errorf("unknown signal %q", s)
}

// Main runs one anden-runtime command and returns its exit code.
func Main(args []string, stdout, stderr io.Writer) int {
	global, cmd, rest, err := splitGlobal(args)
	if err != nil {
		fmt.Fprintln(stderr, "anden-runtime:", err)
		return 2
	}
	if cmd == "init" {
		// Only reached when init fails; on success it execs the process.
		runInit()
		return 1
	}
	root := global["root"]
	if root == "" {
		root = defaultRoot()
	}
	if root, err = filepath.Abs(root); err == nil {
		err = dispatch(store{root: root}, cmd, rest, stdout)
	}
	if err != nil {
		fmt.Fprintf(stderr, "anden-runtime %s: %v\n", cmd, err)
		return 1
	}
	return 0
}

func dispatch(s store, cmd string, args []string, stdout io.Writer) error {
	defs := map[string]map[string]flagDef{
		"create": createFlags, "start": noFlags, "state": noFlags,
		"kill": killFlags, "delete": deleteFlags,
	}[cmd]
	if defs == nil {
		return fmt.Errorf("unknown command %q", cmd)
	}
	flags, pos, err := parseArgs(args, defs)
	if err != nil {
		return err
	}
	if cmd == "kill" && len(pos) == 2 {
		if flags["signal"] != "" {
			return errors.New("signal given both as --signal and as an argument")
		}
		flags["signal"] = pos[1]
		pos = pos[:1]
	}
	id, err := oneID(pos)
	if err != nil {
		return err
	}
	switch cmd {
	case "create":
		bundle := flags["bundle"]
		if bundle == "" {
			bundle = "."
		}
		return create(s, id, bundle, flags["pid-file"])
	case "start":
		return start(s, id)
	case "state":
		r, err := s.load(id)
		if err != nil {
			return err
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(s.state(r))
	case "kill":
		sig := syscall.SIGTERM
		if flags["signal"] != "" {
			if sig, err = parseSignal(flags["signal"]); err != nil {
				return err
			}
		}
		return kill(s, id, sig)
	default:
		return remove(s, id, flags["force"] == "true")
	}
}
