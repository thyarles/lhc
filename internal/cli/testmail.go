package cli

import (
	"context"
	"fmt"
	"html"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/thyarles/lhc/internal/check"
	"github.com/thyarles/lhc/internal/notify"
	"github.com/thyarles/lhc/internal/notify/smtp"
	"github.com/thyarles/lhc/internal/version"
)

// newTestMailCmd checks the mail settings without running the checks. On a
// RHEL 7 host with unreachable repositories a full `lhc run` took almost
// four minutes, and fixing a wrong relay name cost one full run per try.
func newTestMailCmd(a *app) *cobra.Command {
	var to []string
	cmd := &cobra.Command{
		Use:   "test-mail",
		Short: "Send a short test message through the configured SMTP relay",
		Long: `Send a short test message through the smtp: settings, to
email.daily_recipients or to --to, and print what the relay answered.
Runs no checks and changes no state: use it right after setting
smtp.host, before the first real run.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.load(); err != nil {
				return err
			}
			if len(to) == 0 {
				to = a.cfg.Email.DailyRecipients
			}
			if len(to) == 0 {
				return fmt.Errorf("no recipients: set email.daily_recipients, or pass --to")
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return a.testMail(ctx, to)
		},
	}
	cmd.Flags().StringSliceVar(&to, "to", nil, "send to this address instead of email.daily_recipients (repeatable)")
	return cmd
}

func (a *app) testMail(ctx context.Context, to []string) error {
	h := a.hostInfo()
	relay := fmt.Sprintf("%s:%d", a.cfg.SMTP.Host, a.cfg.SMTP.Port)
	from := a.fromAddress(h)
	body := []string{
		"This is a test message from lhc. If you can read it, the mail settings",
		"on this host work.",
		"",
		"Host:    " + h.Label,
		"Version: " + version.String(),
		"Relay:   " + relay + " (tls: " + a.cfg.SMTP.TLS + ")",
		"From:    " + from,
	}
	msg := notify.Message{
		From: from, To: to, Subject: "lhc test message from " + h.Label,
		Text:     strings.Join(body, "\n") + "\n",
		HTML:     "<pre>" + html.EscapeString(strings.Join(body, "\n")) + "</pre>",
		HTMLMode: "inline",
	}
	a.printf("  Sending a test message through %s as %s", relay, from)
	if err := a.sender(h).Send(ctx, msg); err != nil {
		a.printf("  ✗ Test message failed (%s) → %v", relay, err)
		if smtp.Temporary(err) {
			// No retry here: someone is waiting at the terminal.
			a.printf("  The relay says this is temporary; try again in a minute.")
		}
		return &exitError{code: 2, err: fmt.Errorf("delivery failed: %w", err)}
	}
	a.printf("  ✓ Test message accepted by %s → %s", relay, strings.Join(to, ", "))
	a.printf("  Not in the inbox? Look in the junk folder; smtp.from sets the sender.")
	return nil
}

// sender is the SMTP transport both `lhc run` and `lhc test-mail` use, so
// a passing test-mail means the same settings deliver the report.
func (a *app) sender(h check.Host) *smtp.Sender {
	return &smtp.Sender{Config: smtp.Config{
		Host: a.cfg.SMTP.Host, Port: a.cfg.SMTP.Port, TLS: a.cfg.SMTP.TLS, TLSSkipVerify: a.cfg.SMTP.TLSSkipVerify,
		Username: a.cfg.SMTP.Username, Password: a.cfg.SMTP.Password, HelloName: h.Label,
	}, Now: a.now}
}
