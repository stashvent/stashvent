package cmd

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/spf13/cobra"

	"github.com/stashvent/stashvent/internal/api"
	"github.com/stashvent/stashvent/internal/apiclient"
	"github.com/stashvent/stashvent/internal/userfiles"
)

// maxLinkHost is how much of a link's host a message repeats.
const maxLinkHost = 100

func (a *app) deleteCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "delete <id>",
		Short: "Delete a share from the server before it expires",
		Long: `Delete a share from the server before it expires. Its link stops working at
once.

A share pushed from this machine is deleted with the delete token kept in
its history (see "stashvent list"). The request goes to the server the share
was pushed to, whatever --server says.

A share pushed from another machine can be deleted with the API key that
pushed it, if you have run "stashvent login" here (or set STASHVENT_API_KEY).
Then the request goes to the server chosen with --server, STASHVENT_SERVER,
or the default; a link to any other server is refused, so your key never
goes to a host named in a link.

Your own stash, and copies that were already pulled, are ordinary Git stashes
and are not touched.`,
		Example: `  stashvent delete 8xK29qLm4Tz7
  stashvent delete https://stashvent.example/8xK29qLm4Tz7`,
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// A link is accepted for its ID. Where the request goes is decided
			// by the history, or else by the chosen server, never by the link.
			linkServer, shareID, err := apiclient.ParseLink(args[0], "")
			if err != nil {
				return usageError{err}
			}
			history, err := a.history()
			if err != nil {
				return err
			}
			shares, err := history.Load(a.now())
			if err != nil {
				return err
			}
			i := slices.IndexFunc(shares, func(s userfiles.Share) bool { return s.ShareID == shareID })
			if i < 0 {
				return a.deleteWithKey(cmd.Context(), linkServer, shareID)
			}
			share := shares[i]

			// The delete token goes to the server stored with the share and
			// nowhere else. The file can be edited, so the URL is checked
			// like one typed on the command line. The API key isn't sent.
			if err := apiclient.CheckServerURL(share.Server); err != nil {
				return userError{fmt.Sprintf("The history entry for %s has no usable server, so it can't be deleted from here.", shareID)}
			}
			err = apiclient.New(share.Server, a.version).DeleteShare(cmd.Context(), shareID, share.DeleteToken, "")
			gone := isNotFound(err)
			if err != nil && !gone {
				return err // the entry stays, so the delete can be tried again
			}
			if err := history.Remove(a.now(), shareID); err != nil {
				return err
			}
			if gone {
				// Unknown, expired, or deleted before: all the same to the sender.
				a.info("✓ %s was already gone from the server. Removed it from the history.", shareID)
			} else {
				a.info("✓ Deleted %s", shareID)
			}
			return nil
		},
	}
}

// deleteWithKey deletes a share this machine's history doesn't have, with
// the API key. The key goes only to the
// chosen server: linkServer, the host of the link the user gave (if any), may
// be anyone's, so a link to another host is refused before any request.
func (a *app) deleteWithKey(ctx context.Context, linkServer, shareID string) error {
	server, err := a.serverURL()
	if err != nil {
		return err
	}
	if linkServer != "" {
		if normalized, err := apiclient.NormalizeServer(linkServer); err != nil || normalized != server {
			host := cleanText(linkServer, maxLinkHost)
			return userError{fmt.Sprintf("%s isn't in this machine's history, and the link points to %s, not to %s.\n"+
				"Your API key is only sent to the server you choose. If you trust %s and have a key for it, run:\n"+
				"  stashvent delete --server %s %s", shareID, host, server, host, host, shareID)}
		}
	}

	key, _, err := a.apiKey(server)
	if err != nil {
		return err
	}
	if key == "" {
		return userError{fmt.Sprintf("%s isn't in this machine's history, so it can't be deleted from here.\n"+
			"A share can be deleted from the machine that pushed it, and expired shares leave the history.\n"+
			"If the server uses API keys, run \"stashvent login\" here with the key that pushed it, and try again.", shareID)}
	}
	if server != defaultServer {
		a.info("Server: %s (from %s)", server, a.serverFrom)
	}

	err = apiclient.New(server, a.version).DeleteShare(ctx, shareID, "", key)
	if isNotFound(err) {
		// Unlike an ID from the history, this one was typed: it may be wrong.
		a.info("%s isn't on the server: it expired, was already deleted, or the ID is mistyped.", shareID)
		return nil
	}
	// A server without keys ignores the key, and asks for the token.
	var apiErr *apiclient.Error
	if errors.As(err, &apiErr) && apiErr.Code == api.CodeMissingDeleteToken {
		return userError{fmt.Sprintf("%s isn't in this machine's history, and %s doesn't use API keys,\n"+
			"so the share can only be deleted from the machine that pushed it.", shareID, server)}
	}
	if err != nil {
		return err
	}
	a.info("✓ Deleted %s", shareID)
	return nil
}

// isNotFound reports whether err is the server saying the share is unknown,
// expired, or already deleted.
func isNotFound(err error) bool {
	var apiErr *apiclient.Error
	return errors.As(err, &apiErr) && apiErr.Code == api.CodeShareNotFound
}
