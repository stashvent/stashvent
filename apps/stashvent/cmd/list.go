package cmd

import (
	"fmt"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/stashvent/stashvent/internal/userfiles"
)

func (a *app) listCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the shares sent from this machine",
		Long: `List the shares sent from this machine that haven't expired, newest first.

The list comes from a history file on this machine. It doesn't include shares
pushed from another machine, and it isn't checked against the server.`,
		Example: `  stashvent list`,
		Args:    exactArgs(0),
		RunE: func(*cobra.Command, []string) error {
			history, err := a.history()
			if err != nil {
				return err
			}
			now := a.now()
			shares, err := history.Load(now)
			if err != nil {
				return err
			}
			if len(shares) == 0 {
				a.info("No active shares were sent from this machine.")
				return nil
			}
			// Newest first; shares made in the same instant keep file order.
			slices.SortStableFunc(shares, func(x, y userfiles.Share) int { return y.CreatedAt.Compare(x.CreatedAt) })

			// Built first and written once, so a failed write is one error.
			// The values come from a file the user can edit, so they are
			// cleaned like anything else that reaches the terminal. The delete
			// token is never shown.
			var out strings.Builder
			table := tabwriter.NewWriter(&out, 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(table, "ID\tEXPIRES IN\tLINK\tSTASH")
			for _, s := range shares {
				_, _ = fmt.Fprintf(table, "%s\t%s\t%s\t%s\n",
					cleanText(s.ShareID, maxLinkLen), expiresIn(s.ExpiresAt.Sub(now)),
					cleanText(s.Link, maxLinkLen), cleanText(s.StashMessage, maxStashMessageLen))
			}
			if err := table.Flush(); err != nil {
				return err
			}
			_, err = fmt.Fprint(a.stdout, out.String())
			return err
		},
	}
}

// maxLinkLen caps a link shown from the history file, in runes.
const maxLinkLen = 200

// expiresIn writes the time a share has left in its two largest units:
// "6d 23h", "5h 40m", "12m".
func expiresIn(d time.Duration) string {
	d = d.Truncate(time.Minute)
	days := int(d / (24 * time.Hour))
	hours := int(d % (24 * time.Hour) / time.Hour)
	minutes := int(d % time.Hour / time.Minute)
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh %dm", hours, minutes)
	case minutes > 0:
		return fmt.Sprintf("%dm", minutes)
	default:
		return "<1m"
	}
}
