package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/stashvent/stashvent/apikey"
	"github.com/stashvent/stashvent/internal/userfiles"
)

func (a *app) loginCommand() *cobra.Command {
	var withToken bool
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Save the API key you were given for a server",
		Long: `Save the API key you were given for a server, so "push" and "delete" can use
it. A server that requires keys refuses pushes without one; its operator
hands them out. A server that doesn't require them needs no login.

The key is read from a hidden prompt, or from stdin with --with-token, never
from the command line. It is saved in config.json for the server chosen with
--server, STASHVENT_SERVER, or the default, and sent only to that server,
only on push and delete. Pulling never sends it.

In CI, STASHVENT_API_KEY can hold the key instead; it wins over a saved one.`,
		Example: `  stashvent login
  stashvent login --server https://stashvent.example

  # From a file or a secret store, without a terminal
  stashvent login --with-token < key.txt`,
		Args: exactArgs(0),
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			server, err := a.serverURL()
			if err != nil {
				return err
			}

			var key string
			if withToken {
				line, err := a.readLine(ctx)
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if err != nil || strings.TrimSpace(line) == "" {
					return userError{"No API key on stdin. Pipe the key into \"stashvent login --with-token\"."}
				}
				key = line
			} else if key, err = a.promptSecret(ctx, "API key for "+server+": ", "--with-token"); err != nil {
				return err
			}
			// The key is never repeated, also not when it is refused: it may
			// be something else pasted by mistake.
			key = strings.TrimSpace(key)
			if !apikey.Valid(key) {
				return userError{fmt.Sprintf("That isn't a StashVent API key. Keys start with %s and are %d characters long; "+
					"check that it was copied whole.", apikey.Prefix, apikey.Length)}
			}

			config, err := a.config()
			if err != nil {
				return err
			}
			if err := config.SetAPIKey(a.now(), server, key, userfiles.SourcePasted); err != nil {
				return configError{err}
			}
			a.info("✓ Saved the API key for %s", server)
			if a.getenv(apiKeyEnv) != "" {
				a.info("⚠ %s is set, so it is used instead of the saved key.", apiKeyEnv)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&withToken, "with-token", false, "read the key from stdin")
	return cmd
}

func (a *app) logoutCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Remove the saved API key for a server",
		Long: `Remove the API key saved for the server chosen with --server,
STASHVENT_SERVER, or the default. Keys for other servers stay.

This only forgets the key on this machine. If it may have leaked, ask the
server's operator to revoke it.`,
		Example: `  stashvent logout
  stashvent logout --server https://stashvent.example`,
		Args: exactArgs(0),
		RunE: func(*cobra.Command, []string) error {
			server, err := a.serverURL()
			if err != nil {
				return err
			}
			config, err := a.config()
			if err != nil {
				return err
			}
			removed, err := config.RemoveAPIKey(server)
			if err != nil {
				return configError{err}
			}
			if removed {
				a.info("✓ Removed the API key for %s", server)
			} else {
				a.info("No API key was saved for %s.", server)
			}
			if a.getenv(apiKeyEnv) != "" {
				a.info("⚠ %s is still set in your environment.", apiKeyEnv)
			}
			return nil
		},
	}
}
