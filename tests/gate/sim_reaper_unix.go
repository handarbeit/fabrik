//go:build !windows

package gate

import (
	"context"
	"strconv"
	"strings"
	"syscall"

	"github.com/handarbeit/fabrik/internal/sessionreap"
)

// simReaperOps are the real process-table operations. The command name comes
// from ps through the Commander (sessionreap's own is unexported).
func (g *Gate) simReaperOps() *simReaperOps {
	return &simReaperOps{
		Members:    func(sid int) ([]int, error) { return sessionreap.Members(sid, sessionreap.Options{}) },
		StartToken: sessionreap.StartToken,
		Comm: func(pid int) string {
			so, _, _ := output(context.Background(), g.Exec, Cmd{Name: "ps", Args: []string{"-o", "comm=", "-p", strconv.Itoa(pid)}})
			return strings.TrimSpace(so)
		},
		Kill: func(pid int) error { return syscall.Kill(pid, syscall.SIGKILL) },
	}
}
