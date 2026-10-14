//go:build !darwin

package relay

import "net"

// Chrome Relay runs on macOS; elsewhere (CI) callers are found the slow way, with ps.
func procInfo(pid int) (args, env []string, ppid int, ok bool) { return nil, nil, 0, false }

func peerPID(net.Conn) int { return 0 }
