package runtime

import "errors"

// errSeccomp is returned while the BPF generator does not exist: a filter
// the runtime cannot install is refused at create, never skipped.
var errSeccomp = errors.New("linux.seccomp is not supported by anden-runtime yet")

// validateSeccomp checks linux.seccomp at create time.
func validateSeccomp(s *Seccomp) error {
	if s == nil {
		return nil
	}
	return errSeccomp
}

// applySeccomp installs linux.seccomp in init, after PR_SET_NO_NEW_PRIVS
// and right before exec.
func applySeccomp(s *Seccomp) error {
	if s == nil {
		return nil
	}
	return errSeccomp
}
