package cli

import (
	"context"
	"errors"
	"fmt"
	stdlog "log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/thyarles/lhc/internal/fleet"
)

func newCollectCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "collect",
		Short: "Receive the fleet's reports (on a collector, as the lhc-collect service)",
		Long: `Listen on collector.listen for the encrypted reports that hosts send after
every run, and keep the latest one from each host in <state_dir>/fleet/.

Only POST /v1/report is answered, with 204 or an empty 400; nothing is ever
sent back to a host. Each accepted or refused report is one line on stderr
(the journal, under systemd), with the reason for a refusal. SIGHUP re-reads
the config and the fleet key; SIGINT or SIGTERM stop it.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			hup := make(chan os.Signal, 1)
			signal.Notify(hup, syscall.SIGHUP)
			defer signal.Stop(hup)
			return a.collect(ctx, hup)
		},
	}
}

func (a *app) collect(ctx context.Context, hup <-chan os.Signal) error {
	if err := a.load(); err != nil {
		return err
	}
	if !a.cfg.Collector.Enabled {
		return errors.New("collector.enabled is false: this host is not a collector (lhc install --collector)")
	}
	key, err := fleet.LoadKey(a.cfg.Fleet.KeyFile)
	if err != nil {
		return fmt.Errorf("fleet.key_file: %w", err)
	}
	logf := func(format string, args ...any) {
		_, _ = fmt.Fprintf(a.stderr, "[%s] %s\n", a.now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, args...))
	}
	store := fleet.Store{Dir: filepath.Join(a.paths.State, "fleet")}
	recv := fleet.NewReceiver(key, int64(a.cfg.Collector.MaxReportSize), store, a.now, logf)

	listen := a.cfg.Collector.Listen
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", listen)
	if err != nil {
		return fmt.Errorf("collector.listen: %w", err)
	}
	srv := &http.Server{
		Handler:           recv,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       time.Minute,
		MaxHeaderBytes:    8 << 10,
		ErrorLog:          stdlog.New(a.stderr, "http: ", 0),
	}
	logf("collecting on %s into %s", ln.Addr(), store.Dir)
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()

	for {
		select {
		case <-ctx.Done():
			shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := srv.Shutdown(shut)
			cancel()
			logf("stopped")
			return err
		case err := <-served:
			return err
		case <-hup:
			// A broken edit keeps the running settings: a collector that
			// stops on a typo loses every report until someone notices.
			old := a.cfg
			if err := a.load(); err != nil {
				a.cfg = old
				logf("SIGHUP: config not reloaded, keeping the running one: %v", err)
				continue
			}
			key, err := fleet.LoadKey(a.cfg.Fleet.KeyFile)
			if err != nil {
				a.cfg = old
				logf("SIGHUP: fleet key not reloaded, keeping the running one: %v", err)
				continue
			}
			recv.Configure(key, int64(a.cfg.Collector.MaxReportSize))
			logf("SIGHUP: config and fleet key reloaded from %s", a.configPath())
			if a.cfg.Collector.Listen != listen {
				logf("collector.listen changed to %s: restart the service to listen there", a.cfg.Collector.Listen)
			}
		}
	}
}
