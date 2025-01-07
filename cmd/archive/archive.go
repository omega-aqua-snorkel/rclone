// Package cat provides the cat command.
package archive

import (
	"errors"

	"github.com/rclone/rclone/cmd"
	"github.com/spf13/cobra"

	"github.com/rclone/rclone/cmd/archive/create"
	"github.com/rclone/rclone/cmd/archive/list"
	"github.com/rclone/rclone/cmd/archive/extract"
)

// Globals
var (
	fullpath = bool(false)
	format = string("")
)

// RCloneMetadata

func init() {
	Command.AddCommand(create.Command)
	Command.AddCommand(list.Command)
	Command.AddCommand(extract.Command)
	cmd.Root.AddCommand(Command)
}

// archive command

var Command = &cobra.Command{
	Use:   "archive <action> [opts] <source> [<destination>]",
	Short: `Perform an action on an archive.`,
	Long: `Perform an action on an archive.. Requires the use of a
subcommand to specify the protocol, e.g.

    rclone archive list remote:

Each subcommand has its own options which you can see in their help.
`,
	Annotations: map[string]string{
		"versionIntroduced": "v1.68",
	},
	RunE: func(command *cobra.Command, args []string) error {
		if len(args) == 0 {
			return errors.New("archive requires an action, e.g. 'rclone archive list remote:'")
		}
		return errors.New("unknown action")
	},
}
