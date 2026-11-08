package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Status is the container status of the runtime-spec state.
type Status string

const (
	StatusCreating Status = "creating"
	StatusCreated  Status = "created"
	StatusRunning  Status = "running"
	StatusStopped  Status = "stopped"
)

const (
	stateFile = "state.json"
	fifoFile  = "exec.fifo"
)

// record is what the runtime keeps in <root>/<id>/state.json.
type record struct {
	ID          string            `json:"id"`
	Pid         int               `json:"pid"`
	StartTime   uint64            `json:"startTime"`
	Bundle      string            `json:"bundle"`
	Annotations map[string]string `json:"annotations,omitempty"`
	Cgroup      string            `json:"cgroup,omitempty"`
	CgroupOwned bool              `json:"cgroupOwned,omitempty"`
}

// State is the JSON `state` prints, as the runtime-spec defines it.
type State struct {
	OCIVersion  string            `json:"ociVersion"`
	ID          string            `json:"id"`
	Status      Status            `json:"status"`
	Pid         int               `json:"pid,omitempty"`
	Bundle      string            `json:"bundle"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// deriveStatus is the state machine. A record without a pid is still
// being created; a dead process is stopped whatever happened before; a
// live one is created while init still holds the FIFO open for start.
func deriveStatus(pid int, alive, fifoExists bool) Status {
	switch {
	case pid == 0:
		return StatusCreating
	case !alive:
		return StatusStopped
	case fifoExists:
		return StatusCreated
	default:
		return StatusRunning
	}
}

func canStart(s Status) bool { return s == StatusCreated }

func canKill(s Status) bool { return s == StatusCreated || s == StatusRunning }

// canDelete allows created (the process is killed first) and stopped.
// force also allows running and creating.
func canDelete(s Status, force bool) bool {
	return force || s == StatusCreated || s == StatusStopped
}

var validID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.+-]*$`)

func checkID(id string) error {
	if id == "" {
		return errors.New("container id is required")
	}
	if !validID.MatchString(id) || len(id) > 1024 {
		return fmt.Errorf("invalid container id %q", id)
	}
	return nil
}

// defaultRoot is where state lives without --root: under
// $XDG_RUNTIME_DIR, which inside a user namespace mapped with
// --map-root-user still points at the user's /run/user/<uid>, or else
// /run/user/<uid> of the current uid.
func defaultRoot() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "anden-runtime")
	}
	return filepath.Join("/run/user", strconv.Itoa(os.Getuid()), "anden-runtime")
}

type store struct{ root string }

func (s store) dir(id string) string { return filepath.Join(s.root, id) }

func (s store) load(id string) (*record, error) {
	if err := checkID(id); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(s.dir(id), stateFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("container %s does not exist", id)
	}
	if err != nil {
		return nil, err
	}
	var r record
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("container %s: corrupt state: %w", id, err)
	}
	return &r, nil
}

func (s store) save(r *record) error {
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(s.dir(r.ID), stateFile), data)
}

func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// status looks at the live system: whether the recorded process still
// runs (same pid and start time, not a zombie) and whether the FIFO that
// start consumes is still there.
func (s store) status(r *record) Status {
	alive := false
	if r.Pid > 0 {
		alive = processAlive(r.Pid, r.StartTime)
	}
	_, err := os.Lstat(filepath.Join(s.dir(r.ID), fifoFile))
	return deriveStatus(r.Pid, alive, err == nil)
}

func (s store) state(r *record) State {
	st := State{OCIVersion: OCIVersion, ID: r.ID, Status: s.status(r), Bundle: r.Bundle, Annotations: r.Annotations}
	if st.Status == StatusCreated || st.Status == StatusRunning {
		st.Pid = r.Pid
	}
	return st
}

// procStat holds the fields of /proc/<pid>/stat the runtime needs.
type procStat struct {
	State     byte
	StartTime uint64
}

// parseProcStat reads state (field 3) and starttime (field 22). The
// command name may contain spaces and parentheses, so fields are counted
// from the last ')'.
func parseProcStat(data []byte) (procStat, error) {
	s := string(data)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return procStat{}, errors.New("malformed stat")
	}
	fields := strings.Fields(s[i+1:])
	if len(fields) < 20 {
		return procStat{}, errors.New("short stat")
	}
	start, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return procStat{}, err
	}
	return procStat{State: fields[0][0], StartTime: start}, nil
}

func readProcStat(pid int) (procStat, error) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return procStat{}, err
	}
	return parseProcStat(data)
}

func processAlive(pid int, startTime uint64) bool {
	st, err := readProcStat(pid)
	if err != nil {
		return false
	}
	return st.StartTime == startTime && st.State != 'Z' && st.State != 'X' && st.State != 'x'
}
