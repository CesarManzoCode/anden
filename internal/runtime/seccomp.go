package runtime

import (
	"errors"
	"fmt"
	"slices"
	"syscall"
	"unsafe"
)

// Classic BPF and seccomp constants from linux/bpf_common.h, linux/filter.h,
// linux/seccomp.h and linux/audit.h.
const (
	bpfLD  = 0x00
	bpfALU = 0x04
	bpfJMP = 0x05
	bpfRET = 0x06
	bpfW   = 0x00
	bpfABS = 0x20
	bpfK   = 0x00
	bpfAND = 0x50
	bpfJEQ = 0x10
	bpfJGT = 0x20
	bpfJGE = 0x30

	bpfMaxInsns = 4096

	seccompSetModeFilter  = 1
	seccompRetKillProcess = 0x80000000
	seccompRetKillThread  = 0x00000000
	seccompRetErrno       = 0x00050000
	seccompRetAllow       = 0x7fff0000
	seccompRetData        = 0x0000ffff

	auditArchX86_64 = 0xc000003e
	x32SyscallBit   = 0x40000000

	// Offsets in struct seccomp_data: int nr, u32 arch, u64 ip, u64 args[6].
	seccompDataNr   = 0
	seccompDataArch = 4
	seccompDataArgs = 16
)

var seccompFlagValues = map[string]uintptr{
	"SECCOMP_FILTER_FLAG_TSYNC":      1 << 0,
	"SECCOMP_FILTER_FLAG_LOG":        1 << 1,
	"SECCOMP_FILTER_FLAG_SPEC_ALLOW": 1 << 2,
}

// sockFilter is struct sock_filter.
type sockFilter struct {
	Code uint16
	Jt   uint8
	Jf   uint8
	K    uint32
}

// sockFprog is struct sock_fprog; Go pads it like the C struct on x86-64.
type sockFprog struct {
	Len    uint16
	Filter *sockFilter
}

func stmt(code uint16, k uint32) sockFilter {
	return sockFilter{Code: code, K: k}
}

func loadWord(off uint32) sockFilter {
	return stmt(bpfLD|bpfW|bpfABS, off)
}

func ret(k uint32) sockFilter {
	return stmt(bpfRET|bpfK, k)
}

// validateSeccomp checks linux.seccomp at create time: a filter the
// runtime cannot compile is refused there, never skipped in init.
func validateSeccomp(s *Seccomp) error {
	if s == nil {
		return nil
	}
	_, _, err := compileSeccomp(s)
	return err
}

// applySeccomp installs linux.seccomp in init. The kernel accepts it only
// with no_new_privs set or with CAP_SYS_ADMIN, so execProcess calls it
// after PR_SET_NO_NEW_PRIVS, or before dropping capabilities when
// process.noNewPrivileges is false.
func applySeccomp(s *Seccomp) error {
	if s == nil {
		return nil
	}
	prog, flags, err := compileSeccomp(s)
	if err != nil {
		return err
	}
	fprog := sockFprog{Len: uint16(len(prog)), Filter: &prog[0]}
	if _, _, e := syscall.RawSyscall(sysSeccomp, seccompSetModeFilter, flags, uintptr(unsafe.Pointer(&fprog))); e != 0 {
		return fmt.Errorf("seccomp(SECCOMP_SET_MODE_FILTER): %w", e)
	}
	return nil
}

// seccompAction turns an SCMP_ACT_* name into a seccomp return value.
// SCMP_ACT_ERRNO without errnoRet returns EPERM, and errnoRet on an
// action without errno is an error, as runtime-spec says.
func seccompAction(action string, errnoRet *uint) (uint32, error) {
	if errnoRet != nil && action != "SCMP_ACT_ERRNO" {
		return 0, fmt.Errorf("errnoRet is set but %s returns no errno", action)
	}
	switch action {
	case "SCMP_ACT_ALLOW":
		return seccompRetAllow, nil
	case "SCMP_ACT_KILL":
		return seccompRetKillThread, nil
	case "SCMP_ACT_ERRNO":
		errno := uint(syscall.EPERM)
		if errnoRet != nil {
			errno = *errnoRet
		}
		if errno > seccompRetData {
			return 0, fmt.Errorf("errnoRet %d does not fit in a seccomp return value", errno)
		}
		return seccompRetErrno | uint32(errno), nil
	}
	return 0, fmt.Errorf("seccomp action %q is not supported (only SCMP_ACT_ALLOW, SCMP_ACT_ERRNO, SCMP_ACT_KILL)", action)
}

// compileSeccomp builds the BPF program for linux.seccomp. Only native
// x86-64 calls are filtered: any other audit arch and every x32 call kill
// the process. Rules match in the order of the profile and the first
// match wins; names the x86-64 table does not know are skipped, as
// libseccomp-based runtimes do for names of other architectures.
func compileSeccomp(s *Seccomp) ([]sockFilter, uintptr, error) {
	if s.ListenerPath != "" || s.ListenerMetadata != "" {
		return nil, 0, errors.New("linux.seccomp listenerPath is not supported")
	}
	if len(s.Architectures) > 0 && !slices.Contains(s.Architectures, "SCMP_ARCH_X86_64") {
		return nil, 0, fmt.Errorf("linux.seccomp architectures %v lack SCMP_ARCH_X86_64, the only one filtered", s.Architectures)
	}
	var flags uintptr
	for _, f := range s.Flags {
		v, ok := seccompFlagValues[f]
		if !ok {
			return nil, 0, fmt.Errorf("linux.seccomp flag %q is not supported", f)
		}
		flags |= v
	}
	def, err := seccompAction(s.DefaultAction, s.DefaultErrnoRet)
	if err != nil {
		return nil, 0, fmt.Errorf("linux.seccomp defaultAction: %w", err)
	}

	prog := []sockFilter{
		loadWord(seccompDataArch),
		{Code: bpfJMP | bpfJEQ | bpfK, Jt: 1, Jf: 0, K: auditArchX86_64},
		ret(seccompRetKillProcess),
		loadWord(seccompDataNr),
		{Code: bpfJMP | bpfJGE | bpfK, Jt: 0, Jf: 1, K: x32SyscallBit},
		ret(seccompRetKillProcess),
	}
	for i, rule := range s.Syscalls {
		action, err := seccompAction(rule.Action, rule.ErrnoRet)
		if err != nil {
			return nil, 0, fmt.Errorf("linux.seccomp.syscalls[%d]: %w", i, err)
		}
		if action == def {
			continue
		}
		for _, name := range rule.Names {
			nr, ok := syscallNumbers[name]
			if !ok {
				continue
			}
			block, err := ruleBlock(nr, rule.Args, action)
			if err != nil {
				return nil, 0, fmt.Errorf("linux.seccomp.syscalls[%d] %s: %w", i, name, err)
			}
			prog = append(prog, block...)
		}
	}
	prog = append(prog, ret(def))
	if len(prog) > bpfMaxInsns {
		return nil, 0, fmt.Errorf("linux.seccomp compiles to %d BPF instructions, more than the kernel's %d", len(prog), bpfMaxInsns)
	}
	return prog, flags, nil
}

// Jump targets inside a rule block, resolved by ruleBlock.
const (
	toNext = iota // the following instruction
	toPass        // the first instruction after the current argument check
	toFail        // the reload of nr that ends the block
)

// insn is a BPF instruction whose jumps are still targets.
type insn struct {
	code   uint16
	k      uint32
	jt, jf int
}

// ruleBlock is the code for one syscall of a rule. It expects nr in the
// accumulator and leaves it there when the rule does not match.
func ruleBlock(nr uint32, args []SeccompArg, action uint32) ([]sockFilter, error) {
	if len(args) == 0 {
		return []sockFilter{
			{Code: bpfJMP | bpfJEQ | bpfK, Jt: 0, Jf: 1, K: nr},
			ret(action),
		}, nil
	}
	// Code after the JEQ on nr: every argument check (all must pass),
	// the return, and the reload of nr the failed checks jump to.
	type pending struct {
		in   insn
		pass int
	}
	var body []pending
	for _, a := range args {
		check, err := argCheck(a)
		if err != nil {
			return nil, err
		}
		pass := len(body) + len(check)
		for _, in := range check {
			body = append(body, pending{in: in, pass: pass})
		}
	}
	reload := len(body) + 1
	n := reload + 1
	if n > 255 {
		return nil, errors.New("too many argument checks for a BPF jump")
	}
	resolve := func(pos, target, pass int) uint8 {
		switch target {
		case toPass:
			return uint8(pass - (pos + 1))
		case toFail:
			return uint8(reload - (pos + 1))
		}
		return 0
	}
	out := make([]sockFilter, 0, n+1)
	out = append(out, sockFilter{Code: bpfJMP | bpfJEQ | bpfK, Jt: 0, Jf: uint8(n), K: nr})
	for pos, p := range body {
		out = append(out, sockFilter{
			Code: p.in.code, K: p.in.k,
			Jt: resolve(pos, p.in.jt, p.pass), Jf: resolve(pos, p.in.jf, p.pass),
		})
	}
	return append(out, ret(action), loadWord(seccompDataNr)), nil
}

// argCheck compares the 64-bit argument a.Index as two 32-bit words, high
// word first, with unsigned order as libseccomp does. SCMP_CMP_MASKED_EQ
// takes value as the mask and valueTwo as the datum.
func argCheck(a SeccompArg) ([]insn, error) {
	if a.Index > 5 {
		return nil, fmt.Errorf("argument index %d is out of range", a.Index)
	}
	hiOff := uint32(seccompDataArgs + 8*a.Index + 4)
	loOff := uint32(seccompDataArgs + 8*a.Index)
	ld := func(off uint32) insn { return insn{code: bpfLD | bpfW | bpfABS, k: off} }
	jeq := func(k uint32, jt, jf int) insn { return insn{code: bpfJMP | bpfJEQ | bpfK, k: k, jt: jt, jf: jf} }
	jgt := func(k uint32, jt, jf int) insn { return insn{code: bpfJMP | bpfJGT | bpfK, k: k, jt: jt, jf: jf} }
	jge := func(k uint32, jt, jf int) insn { return insn{code: bpfJMP | bpfJGE | bpfK, k: k, jt: jt, jf: jf} }
	and := func(k uint32) insn { return insn{code: bpfALU | bpfAND | bpfK, k: k} }
	vhi, vlo := uint32(a.Value>>32), uint32(a.Value)

	switch a.Op {
	case "SCMP_CMP_EQ":
		return []insn{ld(hiOff), jeq(vhi, toNext, toFail), ld(loOff), jeq(vlo, toNext, toFail)}, nil
	case "SCMP_CMP_NE":
		return []insn{ld(hiOff), jeq(vhi, toNext, toPass), ld(loOff), jeq(vlo, toFail, toNext)}, nil
	case "SCMP_CMP_MASKED_EQ":
		dhi, dlo := uint32(a.ValueTwo>>32), uint32(a.ValueTwo)
		return []insn{
			ld(hiOff), and(vhi), jeq(dhi, toNext, toFail),
			ld(loOff), and(vlo), jeq(dlo, toNext, toFail),
		}, nil
	case "SCMP_CMP_GT":
		return []insn{ld(hiOff), jgt(vhi, toPass, toNext), jeq(vhi, toNext, toFail), ld(loOff), jgt(vlo, toNext, toFail)}, nil
	case "SCMP_CMP_GE":
		return []insn{ld(hiOff), jgt(vhi, toPass, toNext), jeq(vhi, toNext, toFail), ld(loOff), jge(vlo, toNext, toFail)}, nil
	case "SCMP_CMP_LT":
		return []insn{ld(hiOff), jge(vhi, toNext, toPass), jeq(vhi, toNext, toFail), ld(loOff), jge(vlo, toFail, toNext)}, nil
	case "SCMP_CMP_LE":
		return []insn{ld(hiOff), jge(vhi, toNext, toPass), jeq(vhi, toNext, toFail), ld(loOff), jgt(vlo, toFail, toNext)}, nil
	}
	return nil, fmt.Errorf("seccomp operator %q is not supported", a.Op)
}
