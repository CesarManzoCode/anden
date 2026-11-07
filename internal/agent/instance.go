package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	andenv1 "github.com/CesarManzoCode/anden/gen/anden/v1"
	"github.com/CesarManzoCode/anden/internal/image"
	"github.com/CesarManzoCode/anden/internal/wake"
)

const (
	readyPoll     = 10 * time.Millisecond
	probeLimit    = time.Second
	stopGrace     = 2 * time.Second
	killTimeout   = 5 * time.Second
	routeRelaunch = "relaunch"
	// instanceIface is the veth of an instance, as it is called inside.
	instanceIface = "eth0"
)

// paths are the places an instance occupies on the node (contract, section 3).
type paths struct {
	dir, upper, work, merged, bundle string
	netNS, utsNS, ipcNS              string
	cgroup                           string
	// snapshot holds the images of the restore route.
	snapshot string
}

func instancePaths(state, cgroup, service string, generation uint64, id string) paths {
	dir := filepath.Join(state, "instances", id)
	return paths{
		dir:      dir,
		upper:    filepath.Join(dir, "upper"),
		work:     filepath.Join(dir, "work"),
		merged:   filepath.Join(dir, "merged"),
		bundle:   filepath.Join(dir, "bundle"),
		netNS:    filepath.Join(dir, "net.ns"),
		utsNS:    filepath.Join(dir, "uts.ns"),
		ipcNS:    filepath.Join(dir, "ipc.ns"),
		cgroup:   filepath.Join(cgroup, "svc-"+service+"-"+strconv.FormatUint(generation, 10)),
		snapshot: filepath.Join(state, "snapshots", id),
	}
}

// instance is one instance of the node. It lives on while its service sleeps,
// as the machine that wakes it. a.mu guards state, route, stopping, idle,
// nextSample and sleepAfter.
type instance struct {
	id, service         string
	version, generation uint64
	addr, route         string
	// routeRequested is the wake_route of the spec; route is the one the last
	// wake took.
	routeRequested string
	seccomp        bool
	// pid is the root of a tree criu restored, which the runtime does not know,
	// and 0 otherwise. hasSnapshot says the last sleep left one. Only the
	// machine's Halt and Start, and stop after the machine closed, touch them.
	pid         int
	hasSnapshot bool

	paths      paths
	assignment *andenv1.Assignment
	machine    *wake.Machine

	state    andenv1.InstanceState
	stopping bool
	// drain is the time between SIGTERM and SIGKILL when the instance stops:
	// the draining_seconds of a deploy that replaces it, or 0 for stopGrace.
	drain time.Duration
	// idle is nil when the service never sleeps on its own (D05).
	idle       *wake.Idle
	nextSample time.Time
	sleepAfter time.Time

	// cancel ends a start in progress; started closes when it is over.
	cancel  context.CancelFunc
	ctx     context.Context
	started chan struct{}
}

// newInstance prepares an instance, without starting anything. An asleep one
// begins as what is left of a sleep: nothing to wait for and nothing to report.
func (a *Agent) newInstance(ctx context.Context, as *andenv1.Assignment, asleep bool) (*instance, error) {
	spec := as.GetSpec()
	addr, err := netip.ParseAddr(as.GetAddr())
	if err != nil {
		return nil, err
	}
	target := netip.AddrPortFrom(addr, uint16(spec.GetPort()))
	ictx, cancel := context.WithCancel(ctx)
	in := &instance{
		id:             as.GetInstanceId(),
		service:        spec.GetName(),
		version:        as.GetVersion(),
		generation:     as.GetGeneration(),
		addr:           as.GetAddr(),
		route:          routeRelaunch,
		seccomp:        seccompEnabled(spec),
		routeRequested: requestedRoute(spec),
		paths:          instancePaths(a.cfg.State, a.cfg.Cgroup, spec.GetName(), as.GetGeneration(), as.GetInstanceId()),
		assignment:     as,
		state:          andenv1.InstanceState_INSTANCE_STATE_STARTING,
		cancel:         cancel,
		ctx:            ictx,
		started:        make(chan struct{}),
	}
	if seconds := spec.GetIdleSeconds(); seconds > 0 {
		in.idle = wake.NewIdle(time.Duration(seconds)*time.Second, time.Now())
	}
	control := &instanceControl{a: a, in: in}
	if asleep {
		in.state = andenv1.InstanceState_INSTANCE_STATE_STOPPED
		in.machine = wake.NewDormant(target, control)
		close(in.started)
	} else {
		in.machine = wake.NewMachine(target, control)
	}
	return in, nil
}

// start brings an instance to RUNNING, or to FAILED with no process left.
// What it created stays for stop to remove.
func (a *Agent) start(in *instance, as *andenv1.Assignment) {
	defer close(in.started)
	err := a.launch(in, as, nil)
	if err == nil {
		err = a.awaitReady(in, as.GetSpec())
	}
	if err == nil {
		a.running(in)
		log.Printf("instance %s: running at %s", in.id, in.addr)
		return
	}
	if in.ctx.Err() != nil {
		return
	}
	log.Printf("instance %s: %v", in.id, err)
	if err := killCgroup(in.paths.cgroup); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("instance %s: %v", in.id, err)
	}
	a.setState(in, andenv1.InstanceState_INSTANCE_STATE_FAILED)
}

// follow takes from the latest assignment of an instance what can change while
// it lives: the grace of its stop and whether it sleeps on its own. A deploy
// gives the old instance its draining_seconds and no idle window until it is
// no longer assigned (D24); a deploy that fails gives back the usual ones.
// a.mu must be held.
func follow(in *instance, spec *andenv1.ServiceSpec, now time.Time) {
	in.drain = time.Duration(spec.GetDrainingSeconds()) * time.Second
	if spec.IdleSeconds == nil {
		return
	}
	switch seconds := spec.GetIdleSeconds(); {
	case seconds == 0:
		in.idle = nil
	case in.idle == nil:
		in.idle = wake.NewIdle(time.Duration(seconds)*time.Second, now)
	}
}

// running marks an instance as up. Its idle window starts now.
func (a *Agent) running(in *instance) {
	a.setState(in, andenv1.InstanceState_INSTANCE_STATE_RUNNING)
	a.mu.Lock()
	defer a.mu.Unlock()
	if in.idle != nil {
		in.idle.Touch(time.Now())
	}
}

// instanceControl is what the machine of an instance does on the node.
type instanceControl struct {
	a  *Agent
	in *instance
}

func (c *instanceControl) Hold(target netip.AddrPort) error    { return c.a.nft.Add(target) }
func (c *instanceControl) Release(target netip.AddrPort) error { return c.a.nft.Delete(target) }

// LiveSockets reads the namespace through the first process of the instance.
func (c *instanceControl) LiveSockets() (int, error) {
	pid, err := firstProcess(c.in.paths.cgroup)
	if err != nil {
		return 0, err
	}
	sockets, err := wake.ReadSockets(pid)
	return sockets.Live, err
}

// Halt ends the processes and frees the network and the rootfs, keeping the
// cgroup and the upper layer for the wake. The restore route dumps them first
// and, with a snapshot, keeps the network, UTS and IPC namespaces too. The memory
// goes back last, when nothing holds it anymore. Until it is done the instance is
// still reported as running.
func (c *instanceControl) Halt() error {
	in := c.in
	c.a.declare(in, false)
	in.hasSnapshot = in.routeRequested == routeRestore && c.a.sleepByDump(in)
	c.a.terminate(in, stopGrace)
	in.pid = 0
	if in.hasSnapshot {
		unmount(in, in.paths.merged)
	} else {
		c.a.release(in)
	}
	if err := reclaim(in.paths.cgroup); err != nil {
		log.Printf("instance %s: %v", in.id, err)
	}
	c.a.setState(in, andenv1.InstanceState_INSTANCE_STATE_STOPPED)
	log.Printf("instance %s: asleep", in.id)
	return nil
}

// Start wakes the instance by restore from its snapshot or by relaunch, where its
// rootfs and network are made anew. It first asks the control plane for the
// wake, waiting for its answer at most placementTimeout (see place).
func (c *instanceControl) Start(phases *wake.Phases) error {
	in := c.in
	c.a.setState(in, andenv1.InstanceState_INSTANCE_STATE_STARTING)
	c.a.declare(in, true)
	c.a.place(in)
	phases.Mark(wake.Placement)
	snapshot := in.hasSnapshot
	in.hasSnapshot = false
	route, err := wakeBy(in.routeRequested, snapshot, func() error {
		err := c.a.restore(in, phases)
		if err != nil {
			log.Printf("instance %s: restore failed, relaunching: %v", in.id, err)
		}
		return err
	}, func() error {
		if in.routeRequested == routeRestore {
			c.a.discardRestore(in)
		}
		return c.a.launch(in, in.assignment, phases)
	})
	c.a.mu.Lock()
	in.route = route
	c.a.mu.Unlock()
	log.Printf("instance %s: waking by %s, route_requested %s", in.id, route, in.routeRequested)
	return err
}

// discardRestore removes what the restore route keeps or a failed restore left,
// so that a relaunch finds nothing in its way. The images stay, for their logs,
// until the next dump or the end of the instance.
func (a *Agent) discardRestore(in *instance) {
	if populated, _ := cgroupPopulated(in.paths.cgroup); populated {
		if err := killCgroup(in.paths.cgroup); err != nil {
			log.Printf("instance %s: %v", in.id, err)
		}
		waitEmpty(in.paths.cgroup, killTimeout)
	}
	in.pid = 0
	a.release(in)
}

// Probe is a TCP connect from the node, as D04 asks, whatever the healthcheck.
func (c *instanceControl) Probe() error {
	populated, err := cgroupPopulated(c.in.paths.cgroup)
	if err != nil {
		return err
	}
	if !populated {
		return wake.ErrDead
	}
	ctx, cancel := context.WithTimeout(c.in.ctx, probeLimit)
	defer cancel()
	conn, err := wake.Dial(ctx, c.in.machine.Target())
	if err != nil {
		return err
	}
	return conn.Close()
}

// Record appends the breakdown of a wake to wake_events.jsonl.
func (c *instanceControl) Record(phases *wake.Phases) {
	c.a.mu.Lock()
	route := c.in.route
	c.a.mu.Unlock()
	if err := c.a.recordWake(phases.Event(c.in.service, route, c.in.routeRequested)); err != nil {
		log.Printf("instance %s: %s: %v", c.in.id, wakeEventsFile, err)
	}
}

// Observe keeps the counters of the machine in transitions.json.
func (c *instanceControl) Observe(st wake.Stats) {
	if err := c.a.observe(c.in.service, st); err != nil {
		log.Printf("instance %s: %s: %v", c.in.id, transitionsFile, err)
	}
}

func (c *instanceControl) Ready() {
	c.a.running(c.in)
	log.Printf("instance %s: woke at %s", c.in.id, c.in.addr)
}

// Fail leaves the instance FAILED with no process left, as a failed start does.
func (c *instanceControl) Fail(err error) {
	if c.in.ctx.Err() != nil {
		return
	}
	log.Printf("instance %s: %v", c.in.id, err)
	if err := killCgroup(c.in.paths.cgroup); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("instance %s: %v", c.in.id, err)
	}
	c.a.setState(c.in, andenv1.InstanceState_INSTANCE_STATE_FAILED)
}

// firstProcess returns a process of the cgroup. All of them share the network
// namespace of the instance.
func firstProcess(cgroup string) (int, error) {
	raw, err := os.ReadFile(filepath.Join(cgroup, "cgroup.procs"))
	if err != nil {
		return 0, err
	}
	first, _, _ := strings.Cut(string(raw), "\n")
	if first == "" {
		return 0, fmt.Errorf("no process in %s", cgroup)
	}
	return strconv.Atoi(first)
}

// readActivity is the sample of D05 for an instance.
func readActivity(cgroup string) (wake.Sockets, uint64, error) {
	pid, err := firstProcess(cgroup)
	if err != nil {
		return wake.Sockets{}, 0, err
	}
	sockets, err := wake.ReadSockets(pid)
	if err != nil {
		return wake.Sockets{}, 0, err
	}
	tx, err := wake.ReadTxPackets(pid, instanceIface)
	return sockets, tx, err
}

// reclaim gives back the memory still charged to an empty cgroup. The kernel
// answers EAGAIN if it could not reclaim all it was asked for, which is not a
// failure: what it could not free is what memory.current shows afterwards.
func reclaim(cgroup string) error {
	raw, err := os.ReadFile(filepath.Join(cgroup, "memory.current"))
	if err != nil {
		return err
	}
	err = writeUnpolled(filepath.Join(cgroup, "memory.reclaim"), bytes.TrimSpace(raw))
	if errors.Is(err, syscall.EAGAIN) {
		return nil
	}
	return err
}

// writeUnpolled makes one write without the runtime poller. An os.File on a
// cgroup file joins the poller, so it takes EAGAIN for a full buffer and waits
// for a readiness that kernfs never signals: the write would not return.
func writeUnpolled(path string, data []byte) error {
	fd, err := syscall.Open(path, syscall.O_WRONLY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return &os.PathError{Op: "open", Path: path, Err: err}
	}
	defer syscall.Close(fd)
	for {
		_, err = syscall.Write(fd, data)
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}

// launch prepares the rootfs, cgroup and network and has the runtime start the
// process, marking each part on phases when it is a wake.
func (a *Agent) launch(in *instance, as *andenv1.Assignment, phases *wake.Phases) error {
	p, spec := in.paths, as.GetSpec()
	for _, d := range []string{p.upper, p.work, p.merged, p.bundle} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	img, err := a.mountImage(in.ctx, spec.GetImageDigest(), p)
	if err != nil {
		return fmt.Errorf("image %s: %w", spec.GetImageDigest(), err)
	}
	stdio, err := prepareRootfs(p.merged, a.dns)
	if err != nil {
		return err
	}
	defer stdio.Close()
	phases.Mark(wake.Rootfs)
	if err := setupCgroup(p.cgroup); err != nil {
		return err
	}
	if err := a.setupNetwork(in); err != nil {
		return fmt.Errorf("network: %w", err)
	}
	phases.Mark(wake.Network)
	volumes := filepath.Join(a.cfg.State, "volumes")
	for _, v := range spec.GetVolumes() {
		if err := os.MkdirAll(filepath.Join(volumes, v.GetName()), 0o755); err != nil {
			return err
		}
	}
	config, err := buildConfig(configInput{spec: spec, run: img.Run, rootfs: p.merged, netNS: p.netNS, utsNS: p.utsNS, ipcNS: p.ipcNS, cgroup: p.cgroup, volumes: volumes})
	if err != nil {
		return err
	}
	raw, err := json.Marshal(config)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(p.bundle, "config.json"), raw, 0o644); err != nil {
		return err
	}
	if err := in.ctx.Err(); err != nil {
		return err
	}
	// The container inherits these, and so does the runtime's own message if
	// create fails.
	create := exec.Command(a.cfg.Runtime, a.runtimeArgs("create", "--bundle", p.bundle, in.id)...)
	create.Stdout, create.Stderr = stdio, stdio
	if err := create.Run(); err != nil {
		return fmt.Errorf("anden-runtime create: %w; see /.anden/stdio.log in %s", err, p.merged)
	}
	if out, err := exec.Command(a.cfg.Runtime, a.runtimeArgs("start", in.id)...).CombinedOutput(); err != nil {
		return fmt.Errorf("anden-runtime start: %w: %s", err, out)
	}
	phases.Mark(wake.Start)
	return nil
}

func (a *Agent) runtimeArgs(args ...string) []string {
	return append([]string{"--root", filepath.Join(a.cfg.State, "runtime")}, args...)
}

// mountImage brings whatever the node lacks of an image from the blob server
// and mounts it as the rootfs of an instance. The store verifies each blob
// against its digest.
func (a *Agent) mountImage(ctx context.Context, digest string, p paths) (image.Image, error) {
	d, err := image.ParseDigest(digest)
	if err != nil {
		return image.Image{}, err
	}
	a.mu.Lock()
	base := a.blobsURL
	a.mu.Unlock()
	a.imageMu.Lock()
	defer a.imageMu.Unlock()
	img, err := a.store.Fetch(d, func(blob image.Digest) (io.ReadCloser, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/blobs/sha256/"+blob.Hex(), nil)
		if err != nil {
			return nil, err
		}
		resp, err := a.client.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("blob %s: %s", blob, resp.Status)
		}
		return resp.Body, nil
	})
	if err != nil {
		return image.Image{}, err
	}
	return img, a.store.Mount(d, p.merged, p.upper, p.work)
}

// makeDir makes dir inside the rootfs. A symlink from the image is refused:
// the agent runs outside the container and must not follow it.
func makeDir(dir string) error {
	fi, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return os.Mkdir(dir, 0o755)
	}
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s in the image is not a directory", dir)
	}
	return nil
}

// createFile replaces path with a new regular file, never through a symlink.
func createFile(path string, flags int) (*os.File, error) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return os.OpenFile(path, os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW|flags, 0o644)
}

// prepareRootfs writes /etc/resolv.conf and returns the open /.anden/stdio.log.
func prepareRootfs(merged, dns string) (*os.File, error) {
	etc := filepath.Join(merged, "etc")
	if err := makeDir(etc); err != nil {
		return nil, err
	}
	resolv, err := createFile(filepath.Join(etc, "resolv.conf"), os.O_WRONLY)
	if err != nil {
		return nil, err
	}
	_, err = resolv.WriteString("nameserver " + dns + "\n")
	if cerr := resolv.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(merged, ".anden")
	if err := makeDir(dir); err != nil {
		return nil, err
	}
	return createFile(filepath.Join(dir, "stdio.log"), os.O_WRONLY|os.O_APPEND)
}

// setupCgroup creates the cgroup of an instance. The runtime writes its limits;
// swap is written here because the runtime does not manage it, and with swap a
// process over memory.max is slowed down instead of killed.
func setupCgroup(dir string) error {
	if err := os.Mkdir(dir, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	err := os.WriteFile(filepath.Join(dir, "memory.swap.max"), []byte("0"), 0o644)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("memory.swap.max: %w", err)
	}
	return nil
}

func (a *Agent) setupNetwork(in *instance) error {
	p := in.paths
	for _, f := range []string{p.netNS, p.utsNS, p.ipcNS} {
		if err := os.WriteFile(f, nil, 0o600); err != nil {
			return err
		}
	}
	if out, err := exec.Command("unshare", namespaceArgs(p.netNS, p.utsNS, p.ipcNS)...).CombinedOutput(); err != nil {
		return fmt.Errorf("unshare: %w: %s", err, out)
	}
	mac, err := macOf(in.addr)
	if err != nil {
		return err
	}
	host, peer := vethNames(in.id)
	if err := ipBatch(nil, hostBatch(host, peer, p.netNS)); err != nil {
		return err
	}
	return ipBatch([]string{"--net=" + p.netNS}, instanceBatch(peer, mac, in.addr, a.bits, a.gateway))
}

// ipBatch runs `ip -batch -`, under nsenter with these arguments if any.
func ipBatch(nsenter []string, lines []string) error {
	name, args := "ip", []string{"-batch", "-"}
	if nsenter != nil {
		name, args = "nsenter", append(append(append([]string{}, nsenter...), "--", "ip"), args...)
	}
	cmd := exec.Command(name, args...)
	cmd.Stdin = strings.NewReader(strings.Join(lines, "\n") + "\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("ip -batch: %w: %s", err, out)
	}
	return nil
}

// awaitReady waits for the healthcheck to pass, within its timeout, while the
// instance still has processes.
func (a *Agent) awaitReady(in *instance, spec *andenv1.ServiceSpec) error {
	deadline := time.Now().Add(time.Duration(spec.GetHealthcheckTimeoutSeconds()) * time.Second)
	target := net.JoinHostPort(in.addr, strconv.FormatUint(uint64(spec.GetPort()), 10))
	for {
		if err := in.ctx.Err(); err != nil {
			return err
		}
		populated, err := cgroupPopulated(in.paths.cgroup)
		if err != nil {
			return err
		}
		if !populated {
			return errors.New("the process exited before it was ready")
		}
		err = a.probe(in.ctx, target, spec.GetHealthcheck())
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("healthcheck did not pass in %d s: %w", spec.GetHealthcheckTimeoutSeconds(), err)
		}
		time.Sleep(readyPoll)
	}
}

// probe makes one healthcheck: an HTTP GET of its path or a TCP connect.
func (a *Agent) probe(ctx context.Context, target string, hc *andenv1.Healthcheck) error {
	ctx, cancel := context.WithTimeout(ctx, probeLimit)
	defer cancel()
	path, isHTTP := hc.GetCheck().(*andenv1.Healthcheck_Path)
	if !isHTTP {
		var d net.Dialer
		conn, err := d.DialContext(ctx, "tcp", target)
		if err != nil {
			return err
		}
		return conn.Close()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+target+path.Path, nil)
	if err != nil {
		return err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= http.StatusBadRequest {
		return fmt.Errorf("healthcheck: %s", resp.Status)
	}
	return nil
}

// stop ends an instance, asleep or not, and removes everything it occupied. Until
// that is done it keeps being reported as it was: its address and processes are
// still in use.
func (a *Agent) stop(in *instance) {
	in.cancel()
	<-in.started
	in.machine.Close()
	a.mu.Lock()
	grace := in.drain
	a.mu.Unlock()
	if grace == 0 {
		grace = stopGrace
	}
	a.terminate(in, grace)
	a.teardown(in)
	a.proxy.Unregister(in.machine)
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.instances, in.id)
	delete(a.intents, in.id)
	a.changes++
}

// terminate ends the processes of an instance with SIGTERM and, after grace,
// cgroup.kill, which is SIGKILL for all of them, and has the runtime forget the
// container.
func (a *Agent) terminate(in *instance, grace time.Duration) {
	p := in.paths
	if populated, _ := cgroupPopulated(p.cgroup); populated {
		if in.pid > 0 {
			// Restored: the runtime does not know it. It is the init of the
			// pid namespace, and the cgroup would be empty without it.
			_ = syscall.Kill(in.pid, syscall.SIGTERM)
		} else {
			// It fails when create never finished; cgroup.kill covers that.
			_ = exec.Command(a.cfg.Runtime, a.runtimeArgs("kill", in.id, "TERM")...).Run()
		}
		if !waitEmpty(p.cgroup, grace) {
			if err := killCgroup(p.cgroup); err != nil && !errors.Is(err, os.ErrNotExist) {
				log.Printf("instance %s: %v", in.id, err)
			}
			if !waitEmpty(p.cgroup, killTimeout) {
				log.Printf("instance %s: processes left in %s after cgroup.kill", in.id, p.cgroup)
			}
		}
	}
	_ = exec.Command(a.cfg.Runtime, a.runtimeArgs("delete", "--force", in.id)...).Run()
}

// teardown removes what launch created. Each step may find nothing to do,
// because a start can fail or be cancelled at any point.
func (a *Agent) teardown(in *instance) {
	p := in.paths
	a.release(in)
	for _, dir := range []string{p.dir, p.snapshot} {
		if err := os.RemoveAll(dir); err != nil {
			log.Printf("instance %s: %v", in.id, err)
		}
	}
	if err := removeCgroup(p.cgroup); err != nil {
		log.Printf("instance %s: %v", in.id, err)
	}
}

// release frees the veth, the namespaces and the overlay of an instance.
func (a *Agent) release(in *instance) {
	p := in.paths
	host, _ := vethNames(in.id)
	// Explicitly, and not through the namespace: the kernel destroys a netns
	// asynchronously, and the same instance id takes this name when it wakes.
	_ = exec.Command("ip", "link", "del", host).Run()
	unmount(in, p.netNS, p.utsNS, p.ipcNS, p.merged)
}

// unmount detaches each target that is mounted.
func unmount(in *instance, targets ...string) {
	for _, target := range targets {
		err := syscall.Unmount(target, syscall.MNT_DETACH)
		if err != nil && !errors.Is(err, syscall.EINVAL) && !errors.Is(err, syscall.ENOENT) {
			log.Printf("instance %s: unmount %s: %v", in.id, target, err)
		}
	}
}

func parsePopulated(events []byte) bool {
	return bytes.Contains(events, []byte("populated 1"))
}

// cgroupPopulated reads cgroup.events. Children of the cgroup do not exist, so
// this means the instance has processes.
func cgroupPopulated(dir string) (bool, error) {
	events, err := os.ReadFile(filepath.Join(dir, "cgroup.events"))
	if err != nil {
		return false, err
	}
	return parsePopulated(events), nil
}

func killCgroup(dir string) error {
	return os.WriteFile(filepath.Join(dir, "cgroup.kill"), []byte("1"), 0o644)
}

func waitEmpty(dir string, timeout time.Duration) bool {
	for end := time.Now().Add(timeout); time.Now().Before(end); time.Sleep(readyPoll) {
		if populated, err := cgroupPopulated(dir); err != nil || !populated {
			return true
		}
	}
	return false
}

// removeCgroup retries while the kernel finishes with the last processes.
func removeCgroup(dir string) error {
	end := time.Now().Add(killTimeout)
	for {
		err := os.Remove(dir)
		if err == nil || errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if !errors.Is(err, syscall.EBUSY) || time.Now().After(end) {
			return err
		}
		time.Sleep(readyPoll)
	}
}
