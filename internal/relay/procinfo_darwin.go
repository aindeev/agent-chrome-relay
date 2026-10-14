package relay

import (
	"bytes"
	"encoding/binary"
	"net"

	"golang.org/x/sys/unix"
)

// procInfo reads a process's arguments, environment and parent from the kernel (what `ps eww` prints, without
// starting ps).
func procInfo(pid int) (args, env []string, ppid int, ok bool) {
	raw, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil || len(raw) < 4 {
		return nil, nil, 0, false
	}
	argc := int(binary.LittleEndian.Uint32(raw[:4]))
	rest := raw[4:]
	// the executable's path, then NUL padding, then argc arguments, then the environment
	if i := bytes.IndexByte(rest, 0); i >= 0 {
		rest = rest[i:]
	}
	rest = bytes.TrimLeft(rest, "\x00")
	for _, f := range bytes.Split(rest, []byte{0}) {
		if len(args) < argc {
			args = append(args, string(f))
		} else if len(f) == 0 {
			break // the end of the environment (what follows is the kernel's own)
		} else {
			env = append(env, string(f))
		}
	}
	if kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid); err == nil {
		ppid = int(kp.Eproc.Ppid)
	}
	return args, env, ppid, true
}

// peerPID: the process at the other end of a Unix socket, as the kernel knows it.
func peerPID(c net.Conn) int {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return 0
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0
	}
	pid := 0
	raw.Control(func(fd uintptr) {
		pid, _ = unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
	})
	return pid
}
