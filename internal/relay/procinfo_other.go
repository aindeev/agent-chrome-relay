//go:build !darwin && !linux

package relay

import "net"

// Where neither macOS nor Linux: callers are found the slow way (ps), and there is no driver socket.
func procInfo(pid int) (args, env []string, ppid int, ok bool) { return nil, nil, 0, false }

func peerPID(net.Conn) int { return 0 }
