package relay

import (
	"fmt"
	"time"

	"golang.org/x/sys/unix"
)

// quarantine marks a saved file as downloaded from the internet, as Chrome does: macOS checks it before opening it.
func quarantine(path string) {
	unix.Setxattr(path, "com.apple.quarantine", []byte(fmt.Sprintf("0081;%08x;Chrome Relay;", time.Now().Unix())), 0)
}
