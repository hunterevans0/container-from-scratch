//go:build linux

package runtime

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// startFifoName is the FIFO in a container's state directory that its init
// waits on before running the command; runt start writes to it.
const startFifoName = "start.fifo"

// warnOutput receives problems the supervisor works around, such as a
// cgroup it could not remove. A detached monitor has no terminal, so it
// points this at monitor.log in the state directory.
var warnOutput io.Writer = os.Stderr

func warn(err error) {
	fmt.Fprintln(warnOutput, "runt: warning:", err)
}

// Run runs a container in the foreground: its input and output are runt's,
// and Run returns when it exits.
func Run(config Config) error {
	return supervise(config, true, true, nil)
}

// Create starts a container in the background and returns its ID. With
// start set it also runs the command (runt run -d); otherwise the container
// is set up and waits for runt start (runt create). Output goes to the
// container's log.
//
// Something has to wait for the container to exit, record its exit code,
// and remove its cgroup, long after this command returns. That is a monitor
// process (like containerd-shim or conmon): runt again, with --monitor, in a
// session of its own so it is not tied to this terminal.
func Create(config Config, start bool) (string, error) {
	// The monitor runs in "/", so a relative path would change meaning.
	for _, path := range []*string{&config.Rootfs, &config.Image} {
		if *path != "" {
			absolute, err := filepath.Abs(*path)
			if err != nil {
				return "", err
			}
			*path = absolute
		}
	}
	requestR, requestW, err := os.Pipe()
	if err != nil {
		return "", err
	}
	replyR, replyW, err := os.Pipe()
	if err != nil {
		return "", err
	}
	monitor := exec.Command("/proc/self/exe", "--monitor")
	monitor.Dir = "/"
	monitor.ExtraFiles = []*os.File{requestR, replyW}
	monitor.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	err = monitor.Start()
	requestR.Close()
	replyW.Close()
	if err != nil {
		return "", fmt.Errorf("start container monitor: %w", err)
	}

	err = json.NewEncoder(requestW).Encode(monitorRequest{Config: config, Start: start})
	requestW.Close()
	if err != nil {
		return "", fmt.Errorf("send container config to monitor: %w", err)
	}
	var reply monitorReply
	err = json.NewDecoder(replyR).Decode(&reply)
	replyR.Close()
	if err != nil {
		monitor.Wait()
		return "", errors.New("the container monitor exited without reporting back")
	}
	// The monitor outlives this process; don't wait for it.
	monitor.Process.Release()
	if reply.Code != 0 {
		return reply.ID, &exitCodeError{errors.New(reply.Error), reply.Code}
	}
	if reply.Error != "" {
		return reply.ID, errors.New(reply.Error)
	}
	return reply.ID, nil
}

type monitorRequest struct {
	Config Config `json:"config"`
	Start  bool   `json:"start"`
}

type monitorReply struct {
	ID    string `json:"id,omitempty"`
	Error string `json:"error,omitempty"`
	// Code is the exit code to report the error with, if not 1.
	Code int `json:"code,omitempty"`
}

// Monitor is the background process Create starts. It reads its request
// from file descriptor 3 and reports on descriptor 4 once the container is
// created (or started), then stays until the container exits.
func Monitor() error {
	syscall.CloseOnExec(3)
	syscall.CloseOnExec(4)
	requestFile := os.NewFile(3, "request")
	replyFile := os.NewFile(4, "reply")
	var request monitorRequest
	err := json.NewDecoder(requestFile).Decode(&request)
	requestFile.Close()
	if err != nil {
		return fmt.Errorf("read container config: %w", err)
	}

	replied := false
	reply := func(id string, err error) {
		if replied {
			return
		}
		replied = true
		message := monitorReply{ID: id}
		if err != nil {
			message.Error = err.Error()
			var codeErr *exitCodeError
			if errors.As(err, &codeErr) {
				message.Code = codeErr.code
			}
		}
		json.NewEncoder(replyFile).Encode(message)
		replyFile.Close()
	}
	err = supervise(request.Config, false, request.Start, reply)
	// supervise only reports back itself once the container exists.
	reply("", err)
	return nil
}

// supervise creates a container and stays with it until it exits: it starts
// init, records the container's state as it changes, copies its output into
// the log, and cleans up afterwards.
//
// Output always goes to the log, and in the foreground to runt's own output
// too. In the foreground with config.Interactive, the command reads runt's
// input; otherwise its input is empty. With config.Tty the command runs on a
// pseudo-terminal of its own, which runt's terminal is connected to in the
// foreground.
//
// ready, if set, is called once: when the container has been created (and,
// with autoStart, when its command is running), or with the reason it
// failed.
func supervise(config Config, foreground, autoStart bool, ready func(id string, err error)) error {
	if len(config.Command) == 0 {
		return errors.New("command cannot be empty")
	}
	if foreground && config.Interactive && config.Tty && !isTerminal(os.Stdin) {
		return errors.New("the input device is not a terminal; use -t without -i, or run from a terminal")
	}
	uids, gids, err := idRanges(config)
	if err != nil {
		return err
	}
	// The layers to mount an overlay of, top first. Nil means the container
	// uses the rootfs directly.
	var layers []string
	if config.Image != "" {
		if layers, err = unpackImage(config.Image, uids, gids); err != nil {
			return err
		}
	} else {
		if config.Rootfs, err = filepath.Abs(config.Rootfs); err != nil {
			return err
		}
		if err := checkRootfsOwner(config.Rootfs, uids, gids); err != nil {
			return err
		}
		if !config.DirectRootfs {
			layers = []string{config.Rootfs}
		}
	}
	useCgroup := cgroupV2()
	if !useCgroup && !config.Resources.IsZero() {
		return errors.New("resource limits need cgroup v2 mounted at /sys/fs/cgroup")
	}
	if config.Name != "" {
		if err := checkName(StateRoot, config.Name); err != nil {
			return err
		}
	}

	id, err := newID()
	if err != nil {
		return err
	}
	if err := createStateDir(StateRoot, id); err != nil {
		return err
	}
	dir := stateDir(StateRoot, id)
	// A container that never gets as far as being set up leaves no state
	// behind.
	keepState := false
	defer func() {
		if !keepState {
			os.RemoveAll(dir)
		}
	}()
	if !foreground {
		if file, err := os.OpenFile(filepath.Join(dir, "monitor.log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600); err == nil {
			defer file.Close()
			warnOutput = file
		}
	}
	state := &State{
		ID:              id,
		Name:            config.Name,
		Status:          StatusCreated,
		Rootfs:          config.Rootfs,
		Image:           config.Image,
		Bundle:          config.Bundle,
		Command:         config.Command,
		Tty:             config.Tty,
		AppArmorProfile: config.AppArmorProfile,
		Resources:       config.Resources,
		Created:         time.Now(),
	}
	if config.Image != "" {
		state.Rootfs = ""
	}

	rootfs := config.Rootfs
	if layers != nil {
		pruneContainerFS()
		rootfs, err = mountContainerFS(id, layers, uids, gids)
		if err != nil {
			return err
		}
		// The container's own namespace has its own copy of the mount, so
		// the host's can go once init is gone. The writable layer stays
		// for runt rm, unless the container never got going.
		defer func() {
			var err error
			if keepState {
				err = unmountContainerFS(id)
			} else {
				err = removeContainerFS(id)
			}
			if err != nil {
				warn(err)
			}
		}()
	}

	cgroupFD := -1
	if useCgroup {
		cgroup, err := createCgroup(id, config.Resources)
		if err != nil {
			return err
		}
		defer func() {
			if err := removeCgroup(cgroup); err != nil {
				warn(err)
			}
		}()
		state.Cgroup = cgroup
		cgroupFD, err = syscall.Open(cgroup, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
		if err != nil {
			return fmt.Errorf("open cgroup %q: %w", cgroup, err)
		}
		defer syscall.Close(cgroupFD)
	}

	// The start FIFO. Opening a FIFO for reading and writing never blocks,
	// and it gives init a descriptor it can read from even after
	// pivot_root, when the state directory is out of its sight.
	fifoPath := filepath.Join(dir, startFifoName)
	if err := syscall.Mkfifo(fifoPath, 0o600); err != nil {
		return fmt.Errorf("create start FIFO: %w", err)
	}
	defer os.Remove(fifoPath)
	fifo, err := os.OpenFile(fifoPath, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open start FIFO: %w", err)
	}
	defer fifo.Close()
	statusR, statusW, err := os.Pipe()
	if err != nil {
		return err
	}
	defer statusR.Close()
	// Files only init should hold once it has started.
	childEnds := []*os.File{statusW}
	defer func() {
		for _, file := range childEnds {
			file.Close()
		}
	}()
	configR, configW, err := os.Pipe()
	if err != nil {
		return err
	}
	defer configW.Close()
	childEnds = append(childEnds, configR)
	extraFiles := []*os.File{fifo, statusW, configR}
	consoleSocket := -1
	if config.Tty {
		var child *os.File
		if consoleSocket, child, err = consoleSocketPair(); err != nil {
			return err
		}
		defer syscall.Close(consoleSocket)
		childEnds = append(childEnds, child)
		extraFiles = append(extraFiles, child)
	}

	logFile, err := os.OpenFile(filepath.Join(dir, logFileName), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("create container log: %w", err)
	}
	defer logFile.Close()
	log := &logWriter{file: logFile}

	command := exec.Command("/proc/self/exe", "--init")
	command.ExtraFiles = extraFiles
	command.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWUSER |
			syscall.CLONE_NEWUTS |
			syscall.CLONE_NEWPID |
			syscall.CLONE_NEWNS |
			syscall.CLONE_NEWNET |
			syscall.CLONE_NEWIPC |
			syscall.CLONE_NEWCGROUP,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: uids.Base, Size: uids.Size}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: gids.Base, Size: gids.Size}},
		// Allow setgroups inside the container so tools like apt and su work.
		GidMappingsEnableSetgroups: true,
		// Become root of the new user namespace (host uid uids.Base). Without
		// this the child stays host uid 0, which is unmapped there, so it
		// would have no capabilities inside the container.
		Credential: &syscall.Credential{Uid: 0, Gid: 0},
		// A session of its own, with no controlling terminal. Terminal
		// signals such as Ctrl+C don't reach init directly; they arrive
		// once, through runt. And no process in the container can reach
		// runt's terminal through /dev/tty, to change its settings or push
		// keystrokes into it with TIOCSTI.
		Setsid: true,
		// If runt dies without cleaning up, the container dies too, rather
		// than running on with nothing to record its exit.
		Pdeathsig: syscall.SIGKILL,
	}
	if useCgroup {
		// Clone straight into the container's cgroup (clone3's
		// CLONE_INTO_CGROUP), so the limits apply from the start and the
		// new cgroup namespace is rooted at that cgroup.
		command.SysProcAttr.UseCgroupFD = true
		command.SysProcAttr.CgroupFD = cgroupFD
	}
	if config.DebugInit {
		command.Env = append(os.Environ(), debugInitEnv+"=1")
	}

	// Connect the container's input and output. Output always goes through
	// pipes into the log, and in the foreground on to runt's own output as
	// well; with a terminal, that is the command's pseudo-terminal, below,
	// and these pipes only carry what init itself prints. Input is runt's
	// own with -i, and empty otherwise.
	var copies sync.WaitGroup
	if foreground && config.Interactive && !config.Tty {
		command.Stdin = os.Stdin
	}
	names := []string{"stdout", "stderr"}
	outputs := []*os.File{os.Stdout, os.Stderr}
	for i, ours := range outputs {
		r, w, err := os.Pipe()
		if err != nil {
			return err
		}
		outputs[i] = w
		childEnds = append(childEnds, w)
		var passthrough io.Writer
		if foreground {
			passthrough = ours
		}
		copies.Add(1)
		go func() {
			defer copies.Done()
			defer r.Close()
			if err := copyOutput(r, log, names[i], passthrough); err != nil {
				warn(fmt.Errorf("container %s log: %w", names[i], err))
			}
		}()
	}
	command.Stdout = outputs[0]
	command.Stderr = outputs[1]

	// Signals such as SIGTERM would otherwise kill runt before it records
	// that the container stopped and removes its cgroup. Pass them on to the
	// container's init instead, which passes them to the command.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	defer func() {
		signal.Stop(signals)
		close(signals)
	}()

	err = command.Start()
	// Only init holds these now. Closing ours means the log copies end when
	// the container's processes are gone, and init sees end-of-file on its
	// configuration.
	for _, file := range childEnds {
		file.Close()
	}
	childEnds = nil
	if err != nil {
		copies.Wait()
		return err
	}
	go func() {
		for sig := range signals {
			command.Process.Signal(sig)
		}
	}()
	err = json.NewEncoder(configW).Encode(initConfig{
		Rootfs:          rootfs,
		ReadonlyRootfs:  config.ReadonlyRootfs,
		Hostname:        config.Hostname,
		AppArmorProfile: config.AppArmorProfile,
		Command:         config.Command,
		Env:             config.Env,
		Cwd:             config.Cwd,
		User:            config.User,
		Tty:             config.Tty,
	})
	configW.Close()
	if err != nil {
		// Init has exited; its status says why.
		warn(fmt.Errorf("send configuration to init: %w", err))
	}

	state.Pid = command.Process.Pid
	if err := saveState(StateRoot, state); err != nil {
		warn(err)
	}
	if config.DebugInit {
		// Inside its PID namespace init sees itself as PID 1, so only the
		// parent can report the PID a host debugger needs.
		fmt.Fprintf(warnOutput, "runt: container init is host PID %d, waiting for a debugger to attach\n", command.Process.Pid)
	}

	// With a terminal, init sends the master end of the container's
	// pseudo-terminal once it has made it, or closes the socket if setting
	// up fails, which the status line below then explains.
	if config.Tty {
		if console, err := receiveFile(consoleSocket, "console"); err == nil {
			defer console.Close()
			var passthrough io.Writer
			if foreground {
				disconnect, err := connectTerminal(console, config.Interactive)
				if err != nil {
					warn(err)
				} else {
					defer disconnect()
				}
				passthrough = os.Stdout
			} else {
				setWinsize(console, defaultWinsize)
			}
			// A terminal has a single stream, which Docker logs as stdout.
			copies.Add(1)
			go func() {
				defer copies.Done()
				if err := copyOutput(consoleReader{console}, log, "stdout", passthrough); err != nil {
					warn(fmt.Errorf("container terminal log: %w", err))
				}
			}()
		} else if !errors.Is(err, io.EOF) {
			warn(err)
		}
	}

	// finish waits for init and the last of its output, then records the
	// exit.
	finish := func() error {
		waitErr := command.Wait()
		copies.Wait()
		exitCode := exitStatus(command.ProcessState)
		state.Status = StatusStopped
		state.Pid = 0
		state.ExitCode = &exitCode
		state.Finished = time.Now()
		if err := saveState(StateRoot, state); err != nil {
			warn(err)
		}
		return waitErr
	}

	status := bufio.NewScanner(statusR)
	line := nextLine(status)
	if line != "ready" {
		waitErr := command.Wait()
		copies.Wait()
		setupErr := initError(line, "container init exited while setting up the container")
		if ready != nil {
			ready("", setupErr)
		}
		// In the foreground init has printed the error itself, so pass on
		// its exit status quietly.
		if foreground && waitErr != nil {
			return waitErr
		}
		return setupErr
	}
	keepState = true
	if autoStart {
		// Init reads this, and then runs the command. Writing it only now,
		// rather than up front, means a terminal has its size by the time
		// the command starts.
		if _, err := fifo.Write([]byte{0}); err != nil {
			warn(fmt.Errorf("write start FIFO: %w", err))
		}
	}
	// Only init holds the FIFO now, so runt start fails cleanly once init
	// has gone.
	fifo.Close()
	if !autoStart && ready != nil {
		ready(id, nil)
	}

	// Now init waits for the byte in the start FIFO, and then runs the
	// command.
	line = nextLine(status)
	if line != "started" {
		// The command could not be run, so init is exiting. Report its exit
		// code too: 127 if the command was not found, as in the foreground.
		waitErr := finish()
		if ready != nil {
			ready(id, &exitCodeError{initError(line, "container init exited before running the command"), *state.ExitCode})
		}
		return waitErr
	}
	state.Status = StatusRunning
	state.Started = time.Now()
	if err := saveState(StateRoot, state); err != nil {
		warn(err)
	}
	if ready != nil {
		ready(id, nil)
	}
	return finish()
}

// idRanges returns the host ID ranges the container's user namespace maps
// onto. Container UIDs/GIDs 0..Size-1 map to host IDs Base..Base+Size-1, so
// root inside the container is an unprivileged user on the host. The ranges
// come from the bundle's config.json, or else from /etc/subuid and
// /etc/subgid; scripts/make-rootfs.sh reads the same files to shift the
// rootfs ownership to match.
func idRanges(config Config) (uids, gids IDRange, err error) {
	if config.UIDs.Size > 0 && config.GIDs.Size > 0 {
		return config.UIDs, config.GIDs, nil
	}
	user := usernsUser(config.UsernsUser)
	explicit := config.UsernsUser != ""
	if uids, err = subIDRange("/etc/subuid", user, explicit); err != nil {
		return uids, gids, fmt.Errorf("read subordinate UIDs: %w", err)
	}
	if gids, err = subIDRange("/etc/subgid", user, explicit); err != nil {
		return uids, gids, fmt.Errorf("read subordinate GIDs: %w", err)
	}
	return uids, gids, nil
}

// exitCodeError is an error that runt reports with a particular exit code.
type exitCodeError struct {
	err  error
	code int
}

func (e *exitCodeError) Error() string { return e.err.Error() }
func (e *exitCodeError) Unwrap() error { return e.err }

// nextLine returns the next line init sends on the status pipe, or "" if
// init closed it.
func nextLine(scanner *bufio.Scanner) string {
	if scanner.Scan() {
		return scanner.Text()
	}
	return ""
}

// initError turns an "error MESSAGE" status line into an error.
func initError(line, fallback string) error {
	if message, ok := strings.CutPrefix(line, "error "); ok {
		return errors.New(message)
	}
	return errors.New(fallback)
}

// initAlive reports whether pid is a container's init process that is still
// running. Checking the command line guards against the PID having been
// reused by an unrelated process.
func initAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	return err == nil && bytes.HasPrefix(cmdline, []byte("/proc/self/exe\x00--init\x00"))
}

// refresh corrects a state whose monitor died without recording the exit:
// one that says created or running while its init is gone.
func refresh(state *State) {
	if state.Status != StatusStopped && !initAlive(state.Pid) {
		state.Status = StatusStopped
		state.Pid = 0
	}
}

// ListContainers returns all containers, newest first, or with all unset
// only those that are created or running, as docker ps does.
func ListContainers(all bool) ([]*State, error) {
	states, err := listStates(StateRoot)
	if err != nil {
		return nil, err
	}
	var shown []*State
	for _, state := range states {
		refresh(state)
		if all || state.Status != StatusStopped {
			shown = append(shown, state)
		}
	}
	return shown, nil
}

// Start runs the command of a container made with Create(config, false). It
// returns once the command is running, or with an error if it could not be
// run.
func Start(ref string) error {
	state, err := findContainer(StateRoot, ref)
	if err != nil {
		return err
	}
	refresh(state)
	if state.Status != StatusCreated {
		return fmt.Errorf("container %s is %s; only a created container can be started", state.ID, state.Status)
	}
	// O_NONBLOCK makes the open fail with ENXIO, instead of waiting, if no
	// init has the FIFO open for reading.
	fifo, err := os.OpenFile(filepath.Join(stateDir(StateRoot, state.ID), startFifoName), os.O_WRONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return fmt.Errorf("container %s is not waiting to start: %w", state.ID, err)
	}
	_, err = fifo.Write([]byte{0})
	fifo.Close()
	if err != nil {
		return fmt.Errorf("start container %s: %w", state.ID, err)
	}

	// The monitor records whether the command started.
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		current, err := loadState(StateRoot, state.ID)
		if err != nil {
			return err
		}
		switch current.Status {
		case StatusRunning:
			return nil
		case StatusStopped:
			code := "unknown"
			if current.ExitCode != nil {
				code = fmt.Sprint(*current.ExitCode)
			}
			return fmt.Errorf("container %s exited with code %s; see runt logs %s", state.ID, code, state.ID)
		}
	}
	return fmt.Errorf("timed out waiting for container %s to start", state.ID)
}

// Kill sends sig to a container's init, which passes it on to the command.
// SIGKILL can't be passed on: it ends init, and with it every process in the
// container.
func Kill(ref string, sig syscall.Signal) error {
	state, err := findContainer(StateRoot, ref)
	if err != nil {
		return err
	}
	refresh(state)
	if state.Status == StatusStopped {
		return fmt.Errorf("container %s is not running", state.ID)
	}
	if err := syscall.Kill(state.Pid, sig); err != nil {
		return fmt.Errorf("signal container %s: %w", state.ID, err)
	}
	return nil
}

// Stop asks a container to exit with SIGTERM, and kills it with SIGKILL if
// it is still running after timeout, as docker stop does.
func Stop(ref string, timeout time.Duration) error {
	state, err := findContainer(StateRoot, ref)
	if err != nil {
		return err
	}
	refresh(state)
	if state.Status == StatusStopped {
		return nil
	}
	if err := syscall.Kill(state.Pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("stop container %s: %w", state.ID, err)
	}
	if !waitForExit(state.Pid, timeout) {
		syscall.Kill(state.Pid, syscall.SIGKILL)
		if !waitForExit(state.Pid, 5*time.Second) {
			return fmt.Errorf("container %s did not stop", state.ID)
		}
	}
	waitForStopped(state.ID, 5*time.Second)
	return nil
}

// Remove deletes a stopped container's state and log. With force it kills a
// running container first.
func Remove(ref string, force bool) error {
	state, err := findContainer(StateRoot, ref)
	if err != nil {
		return err
	}
	refresh(state)
	if state.Status != StatusStopped {
		if !force {
			return fmt.Errorf("container %s is %s; stop it first, or use rm -f", state.ID, state.Status)
		}
		syscall.Kill(state.Pid, syscall.SIGKILL)
		if !waitForExit(state.Pid, 5*time.Second) {
			return fmt.Errorf("container %s did not stop", state.ID)
		}
		waitForStopped(state.ID, 5*time.Second)
	}
	// The writable layer goes before the state directory; see
	// pruneContainerFS.
	if err := removeContainerFS(state.ID); err != nil {
		return fmt.Errorf("remove container %s: %w", state.ID, err)
	}
	if err := os.RemoveAll(stateDir(StateRoot, state.ID)); err != nil {
		return fmt.Errorf("remove container %s: %w", state.ID, err)
	}
	// The monitor normally removes the cgroup. This catches one whose
	// monitor died.
	if state.Cgroup != "" {
		if err := removeCgroup(state.Cgroup); err != nil {
			warn(err)
		}
	}
	return nil
}

// waitForExit waits up to timeout for init to exit, and reports whether it
// did.
func waitForExit(pid int, timeout time.Duration) bool {
	for deadline := time.Now().Add(timeout); ; time.Sleep(50 * time.Millisecond) {
		if !initAlive(pid) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
	}
}

// waitForStopped waits up to timeout for the monitor to record that the
// container stopped, so that ps and rm see the final state.
func waitForStopped(id string, timeout time.Duration) {
	for deadline := time.Now().Add(timeout); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		state, err := loadState(StateRoot, id)
		if err != nil || state.Status == StatusStopped {
			return
		}
	}
}
