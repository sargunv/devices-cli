package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/sargunv/devices-cli/internal/android"
	"github.com/sargunv/devices-cli/internal/owner"
	"github.com/sargunv/devices-cli/internal/state"
)

func (a *app) assignCommand() *cobra.Command {
	var ownerFlag string
	cmd := &cobra.Command{
		Use:   "assign <serial>",
		Short: "Assign a connected physical Android device to this worktree",
		Long: `Assign a connected physical Android device to this worktree.

"devices-cli get android" then returns the device to this worktree whenever it
is connected and runs the requested API level and ABI, and never returns it to
another worktree. "devices-cli release" unassigns it, as does removing the
worktree.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ownerPath, err := owner.Resolve(ownerFlag)
			if err != nil {
				return err
			}
			sdk, err := android.FindSDK()
			if err != nil {
				return err
			}
			if err := sdk.Connected(cmd.Context(), args[0]); err != nil {
				return err
			}
			m, err := a.manager()
			if err != nil {
				return err
			}
			if err := m.Assign(cmd.Context(), ownerPath, android.KindName, args[0]); err != nil {
				return err
			}
			fmt.Fprintf(a.stderr, "devices-cli: assigned %s to %s\n", args[0], ownerPath)
			return nil
		},
	}
	cmd.Flags().StringVar(&ownerFlag, "owner", "", "directory to assign the device to (default: the git worktree of the working directory)")
	return cmd
}

func (a *app) releaseCommand() *cobra.Command {
	var ownerFlag string
	cmd := &cobra.Command{
		Use:   "release",
		Short: "Stop this worktree's instances and unassign its devices",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ownerPath, err := owner.Resolve(ownerFlag)
			if err != nil {
				return err
			}
			m, err := a.manager()
			if err != nil {
				return err
			}
			return m.Release(cmd.Context(), ownerPath)
		},
	}
	cmd.Flags().StringVar(&ownerFlag, "owner", "", "directory whose devices to release (default: the git worktree of the working directory)")
	return cmd
}

func (a *app) gcCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "gc",
		Short: "Stop instances and remove definitions that are no longer needed",
		Long: `Stop instances and remove definitions that are no longer needed.

gc stops an instance whose owner directory is gone, that has stopped on its
own, or that no get has asked for in idle_timeout. It unassigns devices whose
owner directory is gone, and removes definitions that no get has asked for in
definition_ttl_days. Every command does this first; run gc on a schedule to
also catch machines where no command runs.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			m, err := a.manager()
			if err != nil {
				return err
			}
			return m.Collect(cmd.Context())
		},
	}
}

func (a *app) lsCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List running instances and assigned devices",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			m, err := a.manager()
			if err != nil {
				return err
			}
			st, err := m.List(cmd.Context())
			if err != nil {
				return err
			}
			if asJSON {
				enc := json.NewEncoder(a.stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(listing{MaxInstances: m.Config.MaxInstances, State: st})
			}
			a.printList(st, m.Config.MaxInstances, time.Now())
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print instances, assigned devices, and definitions as JSON")
	return cmd
}

type listing struct {
	MaxInstances int `json:"maxInstances"`
	*state.State
}

func (a *app) printList(st *state.State, maxInstances int, now time.Time) {
	fmt.Fprintf(a.stderr, "%d of %d slots in use\n", len(st.Instances), maxInstances)
	if len(st.Instances) == 0 && len(st.Pins) == 0 {
		return
	}
	w := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "OWNER\tKIND\tID\tDEVICE\tIDLE")
	for _, in := range st.Instances {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", shortPath(in.Owner), in.Kind, in.ID, in.Description, idle(now, in.LastUsed))
	}
	for _, p := range st.Pins {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", shortPath(p.Owner), p.Kind, p.ID, "assigned physical device", idle(now, p.LastUsed))
	}
	w.Flush()
}

func shortPath(path string) string {
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(path, home+string(os.PathSeparator)) {
		return "~" + path[len(home):]
	}
	return path
}

func idle(now, last time.Time) string {
	d := now.Sub(last)
	if d < time.Minute {
		return d.Round(time.Second).String()
	}
	return strings.TrimSuffix(d.Round(time.Minute).String(), "0s")
}
