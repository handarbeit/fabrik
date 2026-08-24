package pruefer

import (
	"context"
	"os"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-isatty"

	ptui "github.com/handarbeit/fabrik/pruefer/tui"
)

// useTUI reports whether Execute should launch the interactive TUI: enabled
// when cfg.TUI is true (the default; -notui/PRUEFER_TUI/config.yaml's
// `tui: false` can disable it) AND a real terminal is detected on both
// stdin and stdout — mirrors cmd/root.go's identical useTUI computation.
func useTUI(cfg Config) bool {
	return cfg.TUI &&
		(isatty.IsTerminal(os.Stdin.Fd()) || isatty.IsCygwinTerminal(os.Stdin.Fd())) &&
		(isatty.IsTerminal(os.Stdout.Fd()) || isatty.IsCygwinTerminal(os.Stdout.Fd()))
}

// runTUI wires the event channel, starts the bubbletea program, and runs the
// daemon — mirrors cmd/root.go's runTUI. The daemon handles SIGINT/SIGTERM
// itself via the context Execute constructed; bubbletea uses
// WithoutSignalHandler so it doesn't interfere. When the daemon exits, the
// TUI is quit; when the user quits the TUI first (q/ctrl+c), a SIGTERM is
// sent to the process so the daemon's own signal handling shuts it down
// gracefully — identical coordination to cmd/root.go's runTUI.
func runTUI(ctx context.Context, d *Daemon) error {
	events := make(chan ptui.Event, 256)
	d.Emit = func(ev ptui.Event) {
		// Drop rather than block the daemon if the TUI falls behind —
		// observability must never apply backpressure to review work.
		select {
		case events <- ev:
		default:
		}
	}

	tuiModel := ptui.New(d.config().WatchedRepos, time.Now())
	p := tea.NewProgram(tuiModel, tea.WithAltScreen(), tea.WithoutSignalHandler())

	go func() {
		for ev := range events {
			p.Send(ev)
		}
	}()

	errCh := make(chan error, 1)
	go func() {
		err := d.Run(ctx)
		close(events)
		errCh <- err
		p.Quit()
	}()

	if _, err := p.Run(); err != nil {
		p.ReleaseTerminal()
		_ = syscall.Kill(syscall.Getpid(), syscall.SIGTERM)
		<-errCh
		return err
	}
	p.ReleaseTerminal()

	select {
	case err := <-errCh:
		return err
	default:
		_ = syscall.Kill(syscall.Getpid(), syscall.SIGTERM)
		return <-errCh
	}
}
