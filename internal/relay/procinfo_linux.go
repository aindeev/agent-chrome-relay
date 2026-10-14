package relay

import (
	"bytes"
	"net"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// procInfo reads a process's arguments, environment and parent from /proc (what `ps eww` prints, without starting
// ps).
func procInfo(pid int) (args, env []string, ppid int, ok bool) {
	dir := "/proc/" + strconv.Itoa(pid) + "/"
	cmdline, err := os.ReadFile(dir + "cmdline")
	if err != nil {
		return nil, nil, 0, false
	}
	split := func(b []byte) []string {
		var out []string
		for _, f := range bytes.Split(bytes.TrimRight(b, "\x00"), []byte{0}) {
			out = append(out, string(f))
		}
		return out
	}
	args = split(cmdline)
	if b, err := os.ReadFile(dir + "environ"); err == nil {
		env = split(b)
	}
	if b, err := os.ReadFile(dir + "stat"); err == nil {
		// pid (comm) state ppid ...: comm may hold spaces and parentheses, so read after the last ")"
		if i := bytes.LastIndexByte(b, ')'); i >= 0 {
			if f := strings.Fields(string(b[i+1:])); len(f) > 1 {
				ppid, _ = strconv.Atoi(f[1])
			}
		}
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
		if cred, err := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED); err == nil {
			pid = int(cred.Pid)
		}
	})
	return pid
}
