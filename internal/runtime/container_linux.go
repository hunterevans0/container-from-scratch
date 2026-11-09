//go:build linux

package runtime

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// The container's life, from the host's side: making it (supervise, through
// Run in the foreground or Create in the background), and the commands that
// act on it afterwards: start, ps, kill, stop, and rm. What happens inside
// the container is in runtime_linux.go (Init).
//
// Three processes take part in running a container:
//
//	runt run (or runt create, whose monitor stays behind)  -- the supervisor
//	  `-- runt --init, cloned into new namespaces           -- the container's PID 1
//	        `-- the container's command
//
// The supervisor sets up everything that has to be done from the host (the
// filesystem, cgroup, network, and log), starts init, and then waits,
// copying output into the log, until init exits. They talk over pipes: the
// supervisor sends init its configuration as JSON, and init answers with a
// line saying how far it got ("ready", "started", or "error MESSAGE").
//
// For how to read the Go here, see doc.go.

// startFifoName is the FIFO in a container's state directory that its init
// waits on before running the command; runt start writes to it.
const startFifoName = "start.fifo"

// warnOutput receives problems the supervisor works around, such as a
// cgroup it could not remove. A detached monitor has no terminal, so it
// points this at monitor.log in the state directory.
var warnOutput io.Writer = os.Stderr

// warn reports a problem that isn't worth failing over.
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
//
// The two talk over two pipes: runt sends the monitor the request (the
// configuration, as JSON) on one, and the monitor answers on the other once
// the container is made, or has failed to be. A pipe is a one-way channel
// between processes: what is written to one end can be read from the other.
func Create(config Config, start bool) (string, error) {
	// The monitor runs in "/", so a relative path would change meaning.
	// paths holds pointers to the fields to make absolute, so the loop can
	// change the fields themselves through them (*path = ...).
	paths := []*string{&config.Rootfs}
	if config.Image != "" && isLayoutRef(config.Image) {
		paths = append(paths, &config.Image)
	}
	for _, path := range paths {
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
	// /proc/self/exe is the running program itself: runt starts another
	// copy of runt. --monitor makes main call Monitor. ExtraFiles become
	// the monitor's descriptors 3 and 4, and Setsid puts it in a session of
	// its own.
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

// monitorRequest is what Create sends the monitor.
type monitorRequest struct {
	Config Config `json:"config"`
	Start  bool   `json:"start"`
}

// monitorReply is what the monitor sends back.
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

	// reply answers runt create, once only. It is passed to supervise as
	// its ready function, so supervise can answer as soon as the container
	// exists and then carry on supervising it, long after runt create has
	// returned.
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
// Output always goes to the log, and to anyone attached (see attachHub), and
// in the foreground to runt's own output too. With config.Interactive, the
// command reads runt's input in the foreground, and in the background a pipe
// that runt attach writes to; otherwise its input is empty. With config.Tty
// the command runs on a pseudo-terminal of its own, which runt's terminal is
// connected to in the foreground.
//
// ready, if set, is called once: when the container has been created (and,
// with autoStart, when its command is running), or with the reason it
// failed.
//
// It is a long function, as the steps share much, and it reads top to
// bottom in the order they happen:
//
//  1. Work out the filesystem: an image's layers, or a rootfs.
//  2. Make the state directory, and from there on, the cleanups: each step
//     that makes something defers its undoing, so a failure at any later
//     step, or the container's exit, undoes everything in reverse order.
//  3. Mount the overlay, stage the bind mounts and volumes, make the cgroup,
//     join the network.
//  4. Make the pipes and FIFO init uses, and connect its input and output.
//  5. Start init, in new namespaces and the cgroup, and make its veth pair.
//  6. Send init its configuration, and wait for "ready".
//  7. Start the command (or leave that to runt start), and wait for init
//     to exit (finish).
func supervise(config Config, foreground, autoStart bool, ready func(id string, err error)) error {
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
	var image *ociImage
	// Starting from an image holds the image store's lock until the
	// container's state, which lists the layers it uses, is saved, so runt
	// rmi can't remove them in between.
	unlockImages := func() {}
	if config.Image != "" || config.image != nil {
		if unlockImages, err = defaultImageStore().lock(false); err != nil {
			return err
		}
		defer unlockImages()
		image = config.image
		if image == nil {
			// Open the image config.Image names: an OCI image layout
			// directory, or else an image in the store, whose lock is held
			// above.
			if isLayoutRef(config.Image) {
				if image, err = openImage(config.Image); err != nil {
					return err
				}
			} else {
				found, err := defaultImageStore().find(config.Image)
				if err != nil {
					return err
				}
				image = found.image
			}
		}
		if err := applyImageConfig(&config, image); err != nil {
			return err
		}
		// Make sure every layer of the image is unpacked, with its file
		// ownership shifted into the container's ID ranges, and list the
		// layer directories top first, the order overlayfs takes them in.
		//
		// Layers are stored by digest under layerRoot/<uid base>-<gid base>,
		// since their ownership depends on the ranges, and are shared by
		// every container that uses them. A layer is unpacked into a
		// temporary directory and renamed into place when complete, so a
		// half-unpacked layer is never used, and two runt processes
		// unpacking the same layer at once don't get in each other's way.
		store := filepath.Join(layerRoot, fmt.Sprintf("%d-%d", uids.Base, gids.Base))
		if err := os.MkdirAll(store, 0o700); err != nil {
			return fmt.Errorf("create layer store: %w", err)
		}
		var dirs []string
		for i, layer := range image.manifest.Layers {
			blob, err := image.blobPath(layer.Digest)
			if err != nil {
				return err
			}
			dir := filepath.Join(store, filepath.Base(blob))
			dirs = append(dirs, dir)
			if _, err := os.Stat(dir); err == nil {
				continue
			}
			temp, err := os.MkdirTemp(store, "unpacking-")
			if err != nil {
				return fmt.Errorf("create layer directory: %w", err)
			}
			err = unpackLayer(blob, layer, image.config.RootFS.DiffIDs[i], temp, uids, gids)
			if err == nil {
				err = os.Rename(temp, dir)
			}
			if err != nil {
				os.RemoveAll(temp)
				if _, statErr := os.Stat(dir); statErr == nil {
					// Another runt finished unpacking it first.
					continue
				}
				return fmt.Errorf("unpack layer %s: %w", layer.Digest, err)
			}
		}
		// The manifest lists layers bottom first; overlayfs wants them top
		// first.
		slices.Reverse(dirs)
		layers = dirs
	} else {
		if config.Rootfs, err = filepath.Abs(config.Rootfs); err != nil {
			return err
		}
		// Fail early with a helpful message if the rootfs has not been
		// shifted into the user-namespace ID ranges.
		var stat syscall.Stat_t
		if err := syscall.Stat(config.Rootfs, &stat); err != nil {
			return fmt.Errorf("stat rootfs %q: %w", config.Rootfs, err)
		}
		if int(stat.Uid) != uids.Base || int(stat.Gid) != gids.Base {
			return fmt.Errorf("rootfs %q is owned by host %d:%d, but container root is host %d:%d; run `make rootfs` to shift its ownership", config.Rootfs, stat.Uid, stat.Gid, uids.Base, gids.Base)
		}
		if !config.DirectRootfs {
			layers = []string{config.Rootfs}
		}
	}
	if len(config.Command) == 0 {
		return errors.New("command cannot be empty")
	}
	if config.SetEnv != nil {
		// -e settings on top of the bundle's environment, or runt's own.
		env := config.Env
		if env == nil {
			env = os.Environ()
		}
		config.Env = mergeEnv(env, config.SetEnv)
	}
	// Reject a working directory that isn't an absolute path, since there is
	// nothing for a relative one to be relative to.
	if config.Cwd != "" && !path.IsAbs(config.Cwd) {
		return fmt.Errorf("the working directory %q must be an absolute path", config.Cwd)
	}
	// Whether the unified cgroup v2 hierarchy is mounted. statfs describes
	// the filesystem a path is on, including a number saying what kind of
	// filesystem it is.
	var cgroupStat syscall.Statfs_t
	useCgroup := syscall.Statfs(cgroupRoot, &cgroupStat) == nil && cgroupStat.Type == cgroup2SuperMagic
	if !useCgroup && !config.Resources.IsZero() {
		return errors.New("resource limits need cgroup v2 mounted at /sys/fs/cgroup")
	}
	if config.Name != "" {
		if err := checkName(StateRoot, config.Name); err != nil {
			return err
		}
	}
	// Whether a network of another name exists is found out when the
	// container joins it (see joinNetwork in network_linux.go).
	if config.Network == "" {
		config.Network = NetworkBridge
	}
	if config.Network == NetworkNone && len(config.Ports) > 0 {
		return errors.New("publishing ports needs a network, and --network none has none")
	}
	if config.Mounts, err = sortMounts(config.Mounts); err != nil {
		return err
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
	// behind. The deferred function runs when supervise returns, and reads
	// keepState as it is then, which is set to true once init is ready.
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
		Interactive:     config.Interactive,
		AppArmorProfile: config.AppArmorProfile,
		Resources:       config.Resources,
		Network:         config.Network,
		Mounts:          config.Mounts,
		Created:         time.Now(),
	}
	if image != nil {
		state.Rootfs = ""
		state.ImageID = image.manifest.Config.Digest
		state.Layers = layers
	}

	rootfs := config.Rootfs
	if layers != nil {
		// Remove writable layers that no container owns any more. /run is a
		// tmpfs, so after a reboot the containers' state is gone but their
		// writable layers in /var/lib remain, with nothing left to remove
		// them. Every container's state directory is made before its
		// writable layer, and removed after it, so a layer without one is an
		// orphan.
		if entries, err := os.ReadDir(containerFSRoot); err == nil {
			for _, entry := range entries {
				if _, err := os.Stat(stateDir(StateRoot, entry.Name())); !errors.Is(err, os.ErrNotExist) {
					continue
				}
				if err := removeContainerFS(entry.Name()); err != nil {
					warn(err)
				}
			}
		}
		// Mount the container's root filesystem: an overlay of layers, given
		// top first, under a new writable layer. rootfs is its mount point.
		if len(layers) == 0 {
			return errors.New("no filesystem layers")
		}
		for _, layer := range layers {
			// The mount options use these to separate layers and options.
			if strings.ContainsAny(layer, ":,\\") {
				return fmt.Errorf("layer path %q contains ':', ',', or '\\', which overlayfs can't take", layer)
			}
		}
		// Init runs as container root, an unprivileged host user, and has to
		// reach the mount point. 0711 lets anyone pass through the top
		// directory, and the container's own directory lets in only its
		// group, container root's.
		if err := os.MkdirAll(containerFSRoot, 0o711); err != nil {
			return fmt.Errorf("create %s: %w", containerFSRoot, err)
		}
		fsDir := containerFSDir(id)
		if err := os.Mkdir(fsDir, 0o710); err != nil {
			return fmt.Errorf("create container filesystem directory: %w", err)
		}
		// cleanup removes what has been made so far, on any failure from
		// here on.
		cleanup := func() { os.RemoveAll(fsDir) }
		if err := os.Chown(fsDir, 0, gids.Base); err != nil {
			cleanup()
			return fmt.Errorf("set owner of %s: %w", fsDir, err)
		}

		upper := filepath.Join(fsDir, "upper")
		work := filepath.Join(fsDir, "work")
		merged := filepath.Join(fsDir, "rootfs")
		// The root directory of the overlay takes its owner and mode from
		// the writable layer: container root, with the mode of the top
		// layer's root.
		mode := os.FileMode(0o755)
		if info, err := os.Stat(layers[0]); err == nil {
			mode = info.Mode().Perm()
		}
		// The steps are a slice of anonymous functions, run in order, so
		// that one error check covers them all.
		for _, step := range []func() error{
			func() error { return os.Mkdir(upper, mode) },
			func() error { return os.Chmod(upper, mode) }, // undo the umask
			func() error { return os.Chown(upper, uids.Base, gids.Base) },
			func() error { return os.Mkdir(work, 0o700) },
			func() error { return os.Mkdir(merged, 0o755) },
		} {
			if err := step(); err != nil {
				cleanup()
				return fmt.Errorf("prepare container filesystem: %w", err)
			}
		}

		// The overlay's options name its layers: lowerdir lists the
		// read-only ones, top first and separated by colons, upperdir is the
		// writable one, and workdir the scratch space.
		//
		// redirect_dir=off and metacopy=off keep the writable layer complete
		// in itself, so runt build can turn it into an image layer (see
		// buildlayer_linux.go). Otherwise, renaming a directory from a lower
		// layer could leave only a pointer to it in the writable layer, and
		// changing a file's owner or mode only its new metadata, without its
		// content. Either feature may be on by default, depending on how the
		// kernel was built.
		options := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s,redirect_dir=off,metacopy=off", strings.Join(layers, ":"), upper, work)
		// Mount options must fit in one page.
		if len(options) >= os.Getpagesize() {
			cleanup()
			return fmt.Errorf("too many layers (%d) to fit overlayfs's mount options", len(layers))
		}
		// nodev: an image could contain device files, and a mount made by
		// host root would let the container open them.
		if err := syscall.Mount("overlay", merged, "overlay", syscall.MS_NODEV, options); err != nil {
			cleanup()
			return fmt.Errorf("mount overlay filesystem: %w", err)
		}
		rootfs = merged
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

	// Bind mounts and volumes, mounted on the host for init to mount in
	// the container (see mounts_linux.go). New volumes are filled from the
	// image, but not from a bundle's root filesystem, which other
	// containers may be changing as runt reads it. The volumes stay locked
	// until the state, which lists them, is saved, so runt volume rm can't
	// remove one in between. unstage runs once init is ready, or else when
	// supervise returns.
	var mounts []Mount
	unlockVolumes := func() {}
	unstage := func() {}
	if len(config.Mounts) > 0 {
		if slices.ContainsFunc(config.Mounts, func(m Mount) bool { return m.Type == MountVolume }) {
			if unlockVolumes, err = lockVolumes(true); err != nil {
				return err
			}
			defer unlockVolumes()
		}
		unstage = sync.OnceFunc(func() {
			if err := unstageMounts(id); err != nil {
				warn(err)
			}
		})
		defer unstage()
		// Stage the mounts, as host root, before init is cloned: mounts is
		// config.Mounts as init is to make them, each volume made if need
		// be, and each bind mount and volume turned into a bind mount from
		// its staging directory. New volumes are filled from rootfs, the
		// container's root filesystem as mounted on the host, if it has
		// layers.
		//
		// Container root has to find the mount points, and nobody else need
		// see what they are: 0711 lets anyone pass through the top
		// directory, and the container's own lets in only its group,
		// container root's.
		if err := os.MkdirAll(mountStagingRoot, 0o711); err != nil {
			return fmt.Errorf("create %s: %w", mountStagingRoot, err)
		}
		stagingDir := filepath.Join(mountStagingRoot, id)
		if err := os.Mkdir(stagingDir, 0o710); err != nil {
			return fmt.Errorf("create mount staging directory: %w", err)
		}
		if err := os.Chown(stagingDir, 0, gids.Base); err != nil {
			return fmt.Errorf("set owner of %s: %w", stagingDir, err)
		}
		staged := make([]Mount, len(config.Mounts))
		for i, mount := range config.Mounts {
			if mount.Type == MountTmpfs {
				staged[i] = mount
				continue
			}
			source := mount.Source
			if mount.Type == MountVolume {
				volume, _, err := ensureVolume(mount.Source, uids, gids)
				if err != nil {
					return err
				}
				source = volume.Mountpoint
				if layers != nil && !slices.Contains(mount.Options, "nocopy") {
					if err := populateVolume(source, rootfs, mount.Destination); err != nil {
						return fmt.Errorf("fill volume %s from the image: %w", mount.Source, err)
					}
				}
			}
			// Stat follows a symbolic link, as the bind mount will.
			info, err := os.Stat(source)
			if errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("bind mount source %s does not exist", source)
			}
			if err != nil {
				return err
			}
			// A bind mount goes onto a directory if its source is one, and
			// onto a file otherwise.
			target := filepath.Join(stagingDir, strconv.Itoa(i))
			if info.IsDir() {
				err = os.Mkdir(target, 0o700)
			} else {
				err = os.WriteFile(target, nil, 0o600)
			}
			if err != nil {
				return fmt.Errorf("create mount point %s: %w", target, err)
			}
			// MS_REC takes along whatever is mounted inside the source, as
			// Docker's bind mounts do.
			if err := syscall.Mount(source, target, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
				return fmt.Errorf("bind mount %s: %w", source, err)
			}
			// A bind mount ignores MS_RDONLY and the like the first time, so
			// read-only takes a second step. Init's copy keeps the flags,
			// and, as it came from the host, can't be made writable again.
			//
			// The mount options that are flags for the mount system call:
			// ro, nosuid, nodev, and noexec.
			var flags uintptr
			for _, option := range mount.Options {
				switch option {
				case "ro":
					flags |= syscall.MS_RDONLY
				case "nosuid":
					flags |= syscall.MS_NOSUID
				case "nodev":
					flags |= syscall.MS_NODEV
				case "noexec":
					flags |= syscall.MS_NOEXEC
				}
			}
			if flags != 0 {
				// A remount sets every flag at once, so it has to repeat the
				// nosuid, nodev, and noexec flags the mount already has, or
				// it would take them away. In a user namespace, the kernel
				// also locks them on mounts that came from a more privileged
				// one, and refuses a remount that would clear them.
				var stat syscall.Statfs_t
				if err := syscall.Statfs(target, &stat); err != nil {
					return fmt.Errorf("make the mount of %s %s: %w", source, strings.Join(mount.Options, ","), fmt.Errorf("read mount flags of %s: %w", target, err))
				}
				// ST_NOSUID, ST_NODEV, and ST_NOEXEC have the same values as
				// the MS_ flags.
				kept := uintptr(stat.Flags) & (syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC)
				if err := syscall.Mount("", target, "", syscall.MS_BIND|syscall.MS_REMOUNT|flags|kept, ""); err != nil {
					return fmt.Errorf("make the mount of %s %s: %w", source, strings.Join(mount.Options, ","), err)
				}
			}
			staged[i] = Mount{Type: MountBind, Source: target, Destination: mount.Destination}
		}
		mounts = staged
	}

	cgroupFD := -1
	if useCgroup {
		// Make the container's cgroup and write its limits. The container
		// is placed in it as it is cloned (below), so it is limited from its
		// first instruction.
		//
		// A controller can only be used in a cgroup if every ancestor
		// enables it for its children in cgroup.subtree_control.
		if err := os.MkdirAll(cgroupParent, 0o755); err != nil {
			return fmt.Errorf("create cgroup %q: %w", cgroupParent, err)
		}
		available, err := os.ReadFile(filepath.Join(cgroupRoot, "cgroup.controllers"))
		if err != nil {
			return fmt.Errorf("read cgroup controllers: %w", err)
		}
		for _, controller := range cgroupControllers {
			if !slices.Contains(strings.Fields(string(available)), controller) {
				continue
			}
			for _, ancestor := range []string{cgroupRoot, cgroupParent} {
				if err := writeCgroupFile(ancestor, "cgroup.subtree_control", "+"+controller); err != nil {
					return err
				}
			}
		}

		cgroup := filepath.Join(cgroupParent, id)
		if err := os.Mkdir(cgroup, 0o755); err != nil {
			return fmt.Errorf("create cgroup %q: %w", cgroup, err)
		}
		if err := setCgroupLimits(cgroup, config.Resources); err != nil {
			removeCgroup(cgroup)
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

	// The container's address and published ports are held until it exits.
	// Its veth pair is made once init, and so its network namespace,
	// exists. network stays nil with --network none. leaveNetwork starts as
	// a function that does nothing, so it can be called either way;
	// sync.OnceFunc below makes one that does its work only the first time
	// it is called, since it is called both when the container exits and,
	// deferred, when supervise returns.
	var network *containerNetwork
	leaveNetwork := func() {}
	if config.Network != NetworkNone {
		if network, err = joinNetwork(id, config.Network, config.Ports); err != nil {
			return err
		}
		leaveNetwork = sync.OnceFunc(func() {
			// Unpublish the container's ports, unplug it from the bridge,
			// and give its address back. Keep going past errors, so that
			// one thing failing doesn't leave the rest behind, and report
			// them all together.
			var errs []error
			if network.dns != nil {
				// The DNS server's goroutines see their sockets closed and
				// return.
				network.dns.udp.Close()
				network.dns.tcp.Close()
			}
			for _, port := range network.ports {
				errs = append(errs, port.close())
			}
			if conn, err := openNetlink(); err != nil {
				errs = append(errs, err)
			} else {
				// The pair also goes when the container's network namespace
				// does, after its last process has exited, so it may be gone
				// already: ENODEV, "no such device", is fine. Deleting
				// either end of a veth pair deletes both.
				deleteRequest := newRequest(syscall.RTM_DELLINK, 0, ifInfo(0, 0, 0))
				deleteRequest.attr(syscall.IFLA_IFNAME, cString(network.hostVeth))
				if err := conn.do(deleteRequest); err != nil && !errors.Is(err, syscall.ENODEV) {
					errs = append(errs, fmt.Errorf("delete veth %s: %w", network.hostVeth, err))
				}
				conn.Close()
			}
			// Only now, so that the next container to get the address can't
			// find this one's veth still plugged in, or its port rules still
			// in place.
			errs = append(errs, network.reservation.release())
			// errors.Join makes one error of several, leaving out the nils.
			// If all of them are nil, so is the result.
			if err := errors.Join(errs...); err != nil {
				warn(err)
			}
		})
		defer leaveNetwork()
		state.IPAddress = network.reservation.ip.String()
		// The container's published ports, with the host ports the system
		// picked, for state.json and runt ps.
		var mappings []PortMapping
		for _, port := range network.ports {
			mappings = append(mappings, port.mapping)
		}
		state.Ports = mappings
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
		// The Unix socket the pseudo-terminal's master is sent over. Our end
		// stays a raw descriptor for recvmsg; the child end is passed to the
		// process that makes the pseudo-terminal. socketpair makes two sockets
		// already connected to each other, like the two ends of a pipe that works
		// both ways.
		var fds [2]int
		if fds, err = syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0); err != nil {
			return fmt.Errorf("create console socket: %w", err)
		}
		consoleSocket = fds[0]
		child := os.NewFile(uintptr(fds[1]), "console socket")
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

	// Init is runt again, with --init. SysProcAttr says how the kernel
	// makes the new process: Cloneflags lists the new namespaces it gets,
	// one flag each, combined with |; UidMappings and GidMappings set up
	// its user namespace's ID mapping (container 0.. onto host Base..).
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

	// runt attach connects here. Listening before init starts means it
	// can connect as soon as runt create returns, before runt start.
	hub, err := listenAttach(filepath.Join(dir, attachSocketName))
	if err != nil {
		return err
	}
	defer hub.close(-1)

	// Connect the container's input and output. Output always goes through
	// pipes into the log and to anyone attached, and in the foreground on to
	// runt's own output as well; with a terminal, that is the command's
	// pseudo-terminal, below, and these pipes only carry what init itself
	// prints. With -i, input is runt's own in the foreground, and in the
	// background a pipe held open for runt attach. Otherwise it is empty.
	// copies counts the goroutines copying output, so the supervisor can
	// wait for the last of it before recording the exit.
	//
	// Each pipe for the container's input or output is owned by container
	// root, as containerd does. Programs open /dev/stdout and /dev/stderr,
	// which lead through /proc/self/fd to the pipe itself; nginx's logs are
	// links to them, for example. Opening a pipe that way checks its owner
	// and mode (0600), so a pipe owned by host root, as runt makes it, would
	// be closed to the container. Both ends share one inode.
	var copies sync.WaitGroup
	if config.Interactive && !config.Tty {
		if foreground {
			command.Stdin = os.Stdin
		} else {
			r, w, err := os.Pipe()
			if err != nil {
				return err
			}
			if err := r.Chown(uids.Base, gids.Base); err != nil {
				r.Close()
				w.Close()
				return fmt.Errorf("set owner of container pipe: %w", err)
			}
			defer w.Close()
			childEnds = append(childEnds, r)
			command.Stdin = r
			hub.connect(w, nil)
		}
	}
	names := []string{"stdout", "stderr"}
	kinds := []byte{frameStdout, frameStderr}
	outputs := []*os.File{os.Stdout, os.Stderr}
	for i, ours := range outputs {
		r, w, err := os.Pipe()
		if err != nil {
			return err
		}
		if err := r.Chown(uids.Base, gids.Base); err != nil {
			r.Close()
			w.Close()
			return fmt.Errorf("set owner of container pipe: %w", err)
		}
		outputs[i] = w
		childEnds = append(childEnds, w)
		// io.MultiWriter makes one writer that writes to several: here,
		// the attached clients and runt's own output.
		passthrough := hub.output(kinds[i])
		if foreground {
			passthrough = io.MultiWriter(passthrough, ours)
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
	// signal.Notify delivers these signals on the channel instead of
	// letting them end runt; a goroutine started below passes each on.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	defer func() {
		signal.Stop(signals)
		close(signals)
	}()

	// Start makes the new process and returns at once, without waiting for
	// it; Wait, in finish below, waits for it to exit.
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
	// Init waits for its configuration, so eth0 is in place before it
	// looks for it, and so is the DNS server, on networks that have one.
	var networkConfig *initNetwork
	if network != nil {
		if err := network.connect(command.Process.Pid); err != nil {
			command.Process.Kill()
			command.Wait()
			copies.Wait()
			return err
		}
		// What init needs to configure eth0: the container's address, as
		// eth0 gets it, which is the reserved address with the network's
		// mask, telling the container which addresses are on its own
		// network and reachable directly, through the bridge; and the
		// gateway, the bridge's own address: the first in the subnet after
		// the subnet's own (10.88.0.1). An IPv4 address is 4 bytes, which
		// read as one 32-bit number (big-endian, most significant byte
		// first) can simply have 1 added.
		gateway := net.IP(binary.BigEndian.AppendUint32(nil, binary.BigEndian.Uint32(network.network.subnet.IP.To4())+1))
		networkConfig = &initNetwork{Address: (&net.IPNet{IP: network.reservation.ip, Mask: network.network.subnet.Mask}).String(), Gateway: gateway.String()}
	}
	go func() {
		for sig := range signals {
			command.Process.Signal(sig)
		}
	}()
	// /etc/hosts and /etc/resolv.conf, which init mounts into the container
	// (see etc_linux.go), unless the container mounts its own there, as
	// with -v ./hosts:/etc/hosts. Docker leaves those alone too.
	//
	// etc holds the files by path: the container's hostname and address in
	// /etc/hosts, if it has one, and in /etc/resolv.conf, the DNS servers it
	// should use. network is nil for a container with --network none.
	hostname := config.Hostname
	if hostname == "" {
		hostname = defaultHostname
	}
	var ip net.IP
	embeddedDNS := false
	if network != nil {
		ip = network.reservation.ip
		embeddedDNS = network.network.dns
	}
	// The host's DNS configuration. On a host whose resolver is
	// systemd-resolved, /etc/resolv.conf names only its stub server at
	// 127.0.0.53, and the servers it asks are in
	// /run/systemd/resolve/resolv.conf, as Docker knows to look. The error
	// is ignored (_): a host with no resolv.conf has no servers, which
	// containerResolvConf makes up for.
	resolvData, _ := os.ReadFile("/etc/resolv.conf")
	hostConf := parseResolvConf(string(resolvData))
	if slices.Equal(hostConf.nameservers, []string{"127.0.0.53"}) {
		if data, err := os.ReadFile("/run/systemd/resolve/resolv.conf"); err == nil {
			hostConf = parseResolvConf(string(data))
		}
	}
	// A map literal: each key, the file's path, with its value, the file's
	// contents.
	etc := map[string]string{
		"/etc/hosts":       hostsFile(hostname, ip),
		"/etc/resolv.conf": containerResolvConf(hostConf, embeddedDNS).String(),
	}
	for path := range etc {
		if mountCovers(config.Mounts, path) {
			delete(etc, path)
		}
	}
	// The configuration goes to init as JSON, written to the pipe init
	// reads as its descriptor 5. Closing the pipe after tells init it has
	// all of it.
	err = json.NewEncoder(configW).Encode(initConfig{
		Rootfs:          rootfs,
		ReadonlyRootfs:  config.ReadonlyRootfs,
		DirectRootfs:    layers == nil,
		Hostname:        config.Hostname,
		AppArmorProfile: config.AppArmorProfile,
		Command:         config.Command,
		Env:             config.Env,
		Cwd:             config.Cwd,
		User:            config.User,
		UserSpec:        config.UserSpec,
		Tty:             config.Tty,
		Network:         networkConfig,
		EtcFiles:        etc,
		Mounts:          mounts,
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
	unlockImages()
	unlockVolumes()
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
			passthrough := hub.output(frameStdout)
			if foreground {
				// Connect runt's own terminal to the container's pseudo-terminal:
				// give the pseudo-terminal the same size and keep it in step when the
				// window is resized, and with config.Interactive put the terminal in raw mode and
				// send what is typed to the container. Undoing this is deferred.
				//
				// The terminal to copy the size from: runt's input, or else its output,
				// whichever is a terminal.
				var source *os.File
				for _, file := range []*os.File{os.Stdin, os.Stdout} {
					if isTerminal(file) {
						source = file
						break
					}
				}
				resize := func() {
					size := defaultWinsize
					if source != nil {
						// The terminal's size, in rows and columns.
						var current winsize
						if err := ioctl(source, syscall.TIOCGWINSZ, unsafe.Pointer(&current)); err == nil && current.rows > 0 && current.cols > 0 {
							size = current
						}
					}
					setWinsize(console, size)
				}
				resize()
				// The kernel sends SIGWINCH when runt's terminal window is resized.
				// signal.Notify delivers it on the channel winch instead of applying
				// its default action, and the goroutine resizes the container's
				// pseudo-terminal each time one comes. "for range winch" loops over
				// values received from the channel, until it is closed.
				winch := make(chan os.Signal, 1)
				signal.Notify(winch, syscall.SIGWINCH)
				go func() {
					for range winch {
						resize()
					}
				}()
				stopResizing := func() {
					signal.Stop(winch)
					close(winch)
				}
				if !config.Interactive {
					defer stopResizing()
				} else if restore, err := makeRaw(os.Stdin); err != nil {
					stopResizing()
					warn(err)
				} else {
					// This copy ends at the first read after the container is gone, or
					// when runt exits.
					go io.Copy(console, os.Stdin)
					defer func() {
						stopResizing()
						restore()
					}()
				}
				passthrough = io.MultiWriter(passthrough, os.Stdout)
			} else {
				setWinsize(console, defaultWinsize)
			}
			// Attached clients type into the terminal with -i, and
			// resize it either way.
			var input io.Writer
			if config.Interactive {
				input = console
			}
			hub.connect(input, console)
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
		// Before the container is recorded as stopped, so its address is
		// free again by the time runt stop returns.
		leaveNetwork()
		// init's exit code, or 128 plus the signal number if a signal
		// killed it, as shells report it. Sys returns the operating
		// system's own form of the exit status, which on Linux is a
		// syscall.WaitStatus; the ", ok" type assertion checks that.
		var exitCode int
		if waitStatus, ok := command.ProcessState.Sys().(syscall.WaitStatus); ok {
			if waitStatus.Signaled() {
				exitCode = 128 + int(waitStatus.Signal())
			} else {
				exitCode = waitStatus.ExitStatus()
			}
		} else {
			exitCode = command.ProcessState.ExitCode()
		}
		state.Status = StatusStopped
		state.Pid = 0
		state.IPAddress = ""
		state.Ports = nil
		state.ExitCode = &exitCode
		state.Finished = time.Now()
		if err := saveState(StateRoot, state); err != nil {
			warn(err)
		}
		hub.close(exitCode)
		return waitErr
	}

	// Read init's first status line. It comes once init has set the
	// container up, or failed to. line is "" if init closed the status
	// pipe, and an "error MESSAGE" line is turned into an error.
	status := bufio.NewScanner(statusR)
	line := ""
	if status.Scan() {
		line = status.Text()
	}
	if line != "ready" {
		waitErr := command.Wait()
		copies.Wait()
		var setupErr error
		if message, ok := strings.CutPrefix(line, "error "); ok {
			setupErr = errors.New(message)
		} else {
			setupErr = errors.New("container init exited while setting up the container")
		}
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
	// Init has made its mounts, in a mount namespace of its own, so the
	// host's staging mounts can go.
	unstage()
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
	line = ""
	if status.Scan() {
		line = status.Text()
	}
	if line != "started" {
		// The command could not be run, so init is exiting. Report its exit
		// code too: 127 if the command was not found, as in the foreground.
		waitErr := finish()
		if ready != nil {
			var startErr error
			if message, ok := strings.CutPrefix(line, "error "); ok {
				startErr = errors.New(message)
			} else {
				startErr = errors.New("container init exited before running the command")
			}
			ready(id, &exitCodeError{startErr, *state.ExitCode})
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
// Any type with an Error method is an error. Unwrap lets errors.Is and
// errors.As look through it to the error it wraps.
type exitCodeError struct {
	err  error
	code int
}

func (e *exitCodeError) Error() string { return e.err.Error() }
func (e *exitCodeError) Unwrap() error { return e.err }

// initAlive reports whether pid is a container's init process that is still
// running. Checking the command line guards against the PID having been
// reused by an unrelated process.
func initAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	// /proc/<pid>/cmdline holds a process's command line, its arguments
	// separated by zero bytes (written \x00).
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
	// Tell the container's init to run the command. O_NONBLOCK makes the
	// open fail with ENXIO, instead of waiting, if no init has the FIFO open
	// for reading.
	fifo, err := os.OpenFile(filepath.Join(stateDir(StateRoot, state.ID), startFifoName), os.O_WRONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return fmt.Errorf("container %s is not waiting to start: %w", state.ID, err)
	}
	_, err = fifo.Write([]byte{0})
	fifo.Close()
	if err != nil {
		return fmt.Errorf("start container %s: %w", state.ID, err)
	}

	// The monitor records whether the command started. A Go for loop has
	// three parts, as in C: one that runs first (set the deadline), the
	// test before each round (not past it yet), and one that runs after
	// each round (wait 50ms).
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
	// ESRCH, "no such process": it has exited already.
	if err := syscall.Kill(state.Pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("stop container %s: %w", state.ID, err)
	}
	if !waitForExit(state.Pid, timeout) {
		syscall.Kill(state.Pid, syscall.SIGKILL)
		if !waitForExit(state.Pid, 5*time.Second) {
			return fmt.Errorf("container %s did not stop", state.ID)
		}
	}
	// Wait up to 5 seconds for the monitor to record that the container
	// stopped, so that ps and rm see the final state.
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		current, err := loadState(StateRoot, state.ID)
		if err != nil || current.Status == StatusStopped {
			break
		}
	}
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
		// Wait up to 5 seconds for the monitor to record that the
		// container stopped, so that ps and rm see the final state.
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			current, err := loadState(StateRoot, state.ID)
			if err != nil || current.Status == StatusStopped {
				break
			}
		}
	}
	// The writable layer goes before the state directory; see the pruning
	// of orphaned writable layers in supervise.
	if err := removeContainerFS(state.ID); err != nil {
		return fmt.Errorf("remove container %s: %w", state.ID, err)
	}
	// The monitor normally unmounts these once init is ready. This catches
	// one that died before then.
	if err := unstageMounts(state.ID); err != nil {
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
