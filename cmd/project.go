package cmd

import (
	"fmt"
	"path/filepath"
	"sort"

	"github.com/spf13/cobra"

	gwconfig "github.com/memcode-ai/memcode/internal/gateway/config"
)

// projectCmd manages the registry of working directories the gateway may execute
// against. Registration is the boundary that keeps a remote chat message from
// turning into execution against an arbitrary filesystem path: the gateway can
// only run against a project that was explicitly registered here.
var projectCmd = &cobra.Command{
	Use:   "project",
	Short: "Register the projects the gateway may work in",
}

var projectAddCmd = &cobra.Command{
	Use:   "add <path>",
	Short: "Register a project directory the gateway may execute against",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		root, err := gwconfig.CanonicalRoot(args[0])
		if err != nil {
			return err
		}
		settings, err := gwconfig.Load()
		if err != nil {
			return err
		}
		if settings.Projects == nil {
			settings.Projects = map[string]gwconfig.Project{}
		}
		id := filepath.Base(root)
		if existing, ok := settings.Projects[id]; ok && existing.Path != root {
			return fmt.Errorf("a different project is already registered as %q (%s); rename the directory or edit gateway.yaml", id, existing.Path)
		}
		settings.Projects[id] = gwconfig.Project{Path: root, Enabled: true}
		if settings.DefaultProject == "" {
			settings.DefaultProject = id // first registered project becomes the gateway default
		}
		if err := gwconfig.Save(settings); err != nil {
			return err
		}
		cmd.Printf("Registered project %q → %s\n", id, root)
		if settings.DefaultProject == id {
			cmd.Printf("Default project: %s\n", id)
		}
		return nil
	},
}

var projectListCmd = &cobra.Command{
	Use:   "list",
	Short: "List registered projects",
	RunE: func(cmd *cobra.Command, args []string) error {
		settings, err := gwconfig.Load()
		if err != nil {
			return err
		}
		if len(settings.Projects) == 0 {
			cmd.Println("No projects registered. Add one with `memcode project add <path>`.")
			return nil
		}
		ids := make([]string, 0, len(settings.Projects))
		for id := range settings.Projects {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			p := settings.Projects[id]
			marker := " "
			if id == settings.DefaultProject {
				marker = "*"
			}
			state := ""
			if !p.Enabled {
				state = " (disabled)"
			}
			cmd.Printf("%s %s → %s%s\n", marker, id, p.Path, state)
		}
		return nil
	},
}

func init() {
	projectCmd.AddCommand(projectAddCmd)
	projectCmd.AddCommand(projectListCmd)
	rootCmd.AddCommand(projectCmd)
}
