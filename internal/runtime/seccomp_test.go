package runtime

import (
	"encoding/binary"
	"slices"
	"strings"
	"testing"
)

// runBPF interprets the subset of classic BPF compileSeccomp emits over a
// struct seccomp_data built from nr, arch and args.
func runBPF(t *testing.T, prog []sockFilter, nr, arch uint32, args [6]uint64) uint32 {
	t.Helper()
	data := make([]byte, 64)
	binary.LittleEndian.PutUint32(data[0:], nr)
	binary.LittleEndian.PutUint32(data[4:], arch)
	for i, a := range args {
		binary.LittleEndian.PutUint64(data[16+8*i:], a)
	}
	var acc uint32
	for pc := 0; pc < len(prog); pc++ {
		in := prog[pc]
		switch in.Code {
		case bpfLD | bpfW | bpfABS:
			acc = binary.LittleEndian.Uint32(data[in.K:])
		case bpfALU | bpfAND | bpfK:
			acc &= in.K
		case bpfJMP | bpfJEQ | bpfK, bpfJMP | bpfJGT | bpfK, bpfJMP | bpfJGE | bpfK:
			var cond bool
			switch in.Code &^ (bpfJMP | bpfK) {
			case bpfJEQ:
				cond = acc == in.K
			case bpfJGT:
				cond = acc > in.K
			case bpfJGE:
				cond = acc >= in.K
			}
			if cond {
				pc += int(in.Jt)
			} else {
				pc += int(in.Jf)
			}
		case bpfRET | bpfK:
			return in.K
		default:
			t.Fatalf("unexpected opcode %#x at %d", in.Code, pc)
		}
	}
	t.Fatal("program ran past its end")
	return 0
}

func TestSyscallTable(t *testing.T) {
	want := map[string]uint32{
		"read": 0, "write": 1, "clone": 56, "execve": 59, "getcwd": 79,
		"personality": 135, "mount": 165, "swapon": 167, "setns": sysSetns, "seccomp": sysSeccomp,
	}
	for name, nr := range want {
		if got, ok := syscallNumbers[name]; !ok || got != nr {
			t.Errorf("syscallNumbers[%q] = %d, %v; want %d", name, got, ok, nr)
		}
	}
	seen := map[uint32]string{}
	for name, nr := range syscallNumbers {
		if nr >= x32SyscallBit {
			t.Errorf("%s has number %d, in the x32 range", name, nr)
		}
		if other, dup := seen[nr]; dup {
			t.Errorf("%s and %s share number %d", name, other, nr)
		}
		seen[nr] = name
	}
	for _, name := range []string{"_llseek", "arm_fadvise64_64", "socketcall"} {
		if _, ok := syscallNumbers[name]; ok {
			t.Errorf("%s is not an x86-64 syscall", name)
		}
	}
}

func TestCompileBlockList(t *testing.T) {
	s := &Seccomp{
		DefaultAction: "SCMP_ACT_ALLOW",
		Architectures: []string{"SCMP_ARCH_X86_64", "SCMP_ARCH_X86", "SCMP_ARCH_X32"},
		Syscalls: []SeccompSyscall{
			{Names: []string{"mount", "swapon", "not_a_syscall"}, Action: "SCMP_ACT_ERRNO"},
			{Names: []string{"getcwd"}, Action: "SCMP_ACT_ALLOW"},
		},
	}
	prog, flags, err := compileSeccomp(s)
	if err != nil {
		t.Fatal(err)
	}
	jeq := uint16(bpfJMP | bpfJEQ | bpfK)
	want := []sockFilter{
		{Code: bpfLD | bpfW | bpfABS, K: 4},
		{Code: jeq, Jt: 1, K: auditArchX86_64},
		{Code: bpfRET | bpfK, K: seccompRetKillProcess},
		{Code: bpfLD | bpfW | bpfABS, K: 0},
		{Code: bpfJMP | bpfJGE | bpfK, Jf: 1, K: x32SyscallBit},
		{Code: bpfRET | bpfK, K: seccompRetKillProcess},
		{Code: jeq, Jf: 1, K: 165},
		{Code: bpfRET | bpfK, K: seccompRetErrno | 1},
		{Code: jeq, Jf: 1, K: 167},
		{Code: bpfRET | bpfK, K: seccompRetErrno | 1},
		{Code: bpfRET | bpfK, K: seccompRetAllow},
	}
	if flags != 0 || !slices.Equal(prog, want) {
		t.Fatalf("program:\n got %+v\nwant %+v (flags %d)", prog, want, flags)
	}
	var none [6]uint64
	for nr, ret := range map[uint32]uint32{165: seccompRetErrno | 1, 167: seccompRetErrno | 1, 79: seccompRetAllow, 0: seccompRetAllow} {
		if got := runBPF(t, prog, nr, auditArchX86_64, none); got != ret {
			t.Errorf("nr %d: got %#x, want %#x", nr, got, ret)
		}
	}
	if got := runBPF(t, prog, 165, 0x40000003, none); got != seccompRetKillProcess {
		t.Errorf("i386 call: got %#x, want kill", got)
	}
	if got := runBPF(t, prog, x32SyscallBit|165, auditArchX86_64, none); got != seccompRetKillProcess {
		t.Errorf("x32 call: got %#x, want kill", got)
	}
}

func TestDefaultAction(t *testing.T) {
	errno := uint(38)
	cases := []struct {
		s    Seccomp
		want uint32
	}{
		{Seccomp{DefaultAction: "SCMP_ACT_ALLOW"}, seccompRetAllow},
		{Seccomp{DefaultAction: "SCMP_ACT_ERRNO"}, seccompRetErrno | 1},
		{Seccomp{DefaultAction: "SCMP_ACT_ERRNO", DefaultErrnoRet: &errno}, seccompRetErrno | 38},
		{Seccomp{DefaultAction: "SCMP_ACT_KILL"}, seccompRetKillThread},
	}
	for _, c := range cases {
		prog, _, err := compileSeccomp(&c.s)
		if err != nil {
			t.Fatalf("%s: %v", c.s.DefaultAction, err)
		}
		if len(prog) != 7 || prog[6] != ret(c.want) {
			t.Errorf("%s: program %+v, want it to end in RET %#x", c.s.DefaultAction, prog, c.want)
		}
	}
}

func TestRuleWithDefaultActionIsSkipped(t *testing.T) {
	s := &Seccomp{DefaultAction: "SCMP_ACT_ERRNO", Syscalls: []SeccompSyscall{
		{Names: []string{"read"}, Action: "SCMP_ACT_ERRNO"},
	}}
	prog, _, err := compileSeccomp(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(prog) != 7 {
		t.Fatalf("a rule with the default action should add no code: %+v", prog)
	}
}

func TestCompileRejects(t *testing.T) {
	errno := uint(70000)
	cases := map[string]Seccomp{
		"SCMP_ACT_TRAP":     {DefaultAction: "SCMP_ACT_TRAP"},
		"listenerPath":      {DefaultAction: "SCMP_ACT_ALLOW", ListenerPath: "/run/x.sock"},
		"SCMP_ARCH_X86_64":  {DefaultAction: "SCMP_ACT_ALLOW", Architectures: []string{"SCMP_ARCH_AARCH64"}},
		"flag":              {DefaultAction: "SCMP_ACT_ALLOW", Flags: []string{"SECCOMP_FILTER_FLAG_NEW_LISTENER"}},
		"does not fit":      {DefaultAction: "SCMP_ACT_ERRNO", DefaultErrnoRet: &errno},
		"index 6":           {DefaultAction: "SCMP_ACT_ALLOW", Syscalls: []SeccompSyscall{{Names: []string{"read"}, Action: "SCMP_ACT_ERRNO", Args: []SeccompArg{{Index: 6, Op: "SCMP_CMP_EQ"}}}}},
		"SCMP_CMP_BOGUS":    {DefaultAction: "SCMP_ACT_ALLOW", Syscalls: []SeccompSyscall{{Names: []string{"read"}, Action: "SCMP_ACT_ERRNO", Args: []SeccompArg{{Op: "SCMP_CMP_BOGUS"}}}}},
		"SCMP_ACT_LOG":      {DefaultAction: "SCMP_ACT_ALLOW", Syscalls: []SeccompSyscall{{Names: []string{"read"}, Action: "SCMP_ACT_LOG"}}},
		"SCMP_ACT_KILL_ALL": {DefaultAction: "SCMP_ACT_KILL_ALL"},
		"returns no errno":  {DefaultAction: "SCMP_ACT_ALLOW", DefaultErrnoRet: &errno},
	}
	for want, s := range cases {
		_, _, err := compileSeccomp(&s)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got error %v", want, err)
		}
	}
}

func TestArgumentComparisons(t *testing.T) {
	const big = 0x1_0000_0005
	cases := []struct {
		arg  SeccompArg
		hits []uint64
		miss []uint64
	}{
		{SeccompArg{Index: 1, Op: "SCMP_CMP_EQ", Value: big}, []uint64{big}, []uint64{5, 0x1_0000_0000, 0}},
		{SeccompArg{Index: 1, Op: "SCMP_CMP_NE", Value: big}, []uint64{5, 0x1_0000_0000, 0}, []uint64{big}},
		{SeccompArg{Index: 1, Op: "SCMP_CMP_GT", Value: big}, []uint64{big + 1, 0x2_0000_0000}, []uint64{big, 6, 0}},
		{SeccompArg{Index: 1, Op: "SCMP_CMP_GE", Value: big}, []uint64{big, 0x2_0000_0000}, []uint64{big - 1, 6}},
		{SeccompArg{Index: 1, Op: "SCMP_CMP_LT", Value: big}, []uint64{big - 1, 6, 0}, []uint64{big, 0x2_0000_0000}},
		{SeccompArg{Index: 1, Op: "SCMP_CMP_LE", Value: big}, []uint64{big, 6}, []uint64{big + 1, 0x2_0000_0000}},
		{SeccompArg{Index: 1, Op: "SCMP_CMP_MASKED_EQ", Value: 0x7e020000, ValueTwo: 0}, []uint64{0x3d0f00, 0x1_0000_0000}, []uint64{0x20000, 0x10000000}},
	}
	for _, c := range cases {
		s := &Seccomp{DefaultAction: "SCMP_ACT_ALLOW", Syscalls: []SeccompSyscall{
			{Names: []string{"clone"}, Action: "SCMP_ACT_ERRNO", Args: []SeccompArg{c.arg}},
			{Names: []string{"clone"}, Action: "SCMP_ACT_KILL"},
		}}
		prog, _, err := compileSeccomp(s)
		if err != nil {
			t.Fatalf("%s: %v", c.arg.Op, err)
		}
		for _, v := range c.hits {
			if got := runBPF(t, prog, 56, auditArchX86_64, [6]uint64{0, v}); got != seccompRetErrno|1 {
				t.Errorf("%s %#x on %#x: got %#x, want errno", c.arg.Op, c.arg.Value, v, got)
			}
		}
		// A failed check must reload nr so the second clone rule still matches.
		for _, v := range c.miss {
			if got := runBPF(t, prog, 56, auditArchX86_64, [6]uint64{0, v}); got != seccompRetKillThread {
				t.Errorf("%s %#x on %#x: got %#x, want the second rule", c.arg.Op, c.arg.Value, v, got)
			}
		}
		if got := runBPF(t, prog, 0, auditArchX86_64, [6]uint64{0, c.miss[0]}); got != seccompRetAllow {
			t.Errorf("%s: read got %#x, want allow", c.arg.Op, got)
		}
	}
}

func TestArgumentsAreAnded(t *testing.T) {
	s := &Seccomp{DefaultAction: "SCMP_ACT_ERRNO", Syscalls: []SeccompSyscall{
		{Names: []string{"personality"}, Action: "SCMP_ACT_ALLOW", Args: []SeccompArg{
			{Index: 0, Op: "SCMP_CMP_EQ", Value: 8},
			{Index: 2, Op: "SCMP_CMP_LT", Value: 3},
		}},
	}}
	prog, _, err := compileSeccomp(s)
	if err != nil {
		t.Fatal(err)
	}
	for args, want := range map[[6]uint64]uint32{
		{8, 0, 2}: seccompRetAllow,
		{8, 0, 3}: seccompRetErrno | 1,
		{7, 0, 2}: seccompRetErrno | 1,
	} {
		if got := runBPF(t, prog, 135, auditArchX86_64, args); got != want {
			t.Errorf("personality%v: got %#x, want %#x", args[:3], got, want)
		}
	}
}

func TestValidateSeccomp(t *testing.T) {
	if err := validateSeccomp(nil); err != nil {
		t.Fatalf("nil seccomp: %v", err)
	}
	if err := validateSeccomp(&Seccomp{DefaultAction: "SCMP_ACT_ALLOW"}); err != nil {
		t.Fatalf("allow-all: %v", err)
	}
	if err := validateSeccomp(&Seccomp{DefaultAction: "SCMP_ACT_NOTIFY"}); err == nil {
		t.Fatal("SCMP_ACT_NOTIFY was accepted")
	}
}
