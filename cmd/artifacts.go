package cmd

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/memcode-ai/memcode/internal/artifacts"
	"github.com/memcode-ai/memcode/internal/authflow"
)

// memcode artifacts — manage the single-page artifacts the agent publishes to
// memcode.ai (stable URLs at memcode.ai/code/artifact/<id>). Publishing itself
// stays with the agent's artifact tool; this surface is list/open/delete only.
var artifactsCmd = &cobra.Command{
	Use:   "artifacts",
	Short: "List, open, and delete your published artifact pages",
	Long: `Manage your memcode artifacts (agent-published HTML pages).

  memcode artifacts list          Your org's artifacts, newest first
  memcode artifacts open <id>     Open an artifact's page in the browser
  memcode artifacts delete <id>   Take a published artifact down`,
	RunE: func(cmd *cobra.Command, args []string) error { return runArtifactsList(cmd) },
}

var artifactsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List your org's artifacts",
	RunE:  func(cmd *cobra.Command, args []string) error { return runArtifactsList(cmd) },
}

var artifactsOpenCmd = &cobra.Command{
	Use:   "open <id>",
	Short: "Open an artifact's page in the browser",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		url, err := artifactURL(cmd, args[0])
		if err != nil {
			return err
		}
		fmt.Println(url)
		if err := authflow.OpenBrowser(url); err != nil {
			fmt.Println("(could not open a browser — use the URL above)")
		}
		return nil
	},
}

var artifactsDeleteCmd = &cobra.Command{
	Use:   "delete <id>",
	Short: "Take a published artifact down",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := artifacts.New()
		if err != nil {
			return err
		}
		id := args[0]
		yes, _ := cmd.Flags().GetBool("yes")
		if !yes {
			if !term.IsTerminal(int(os.Stdin.Fd())) {
				return fmt.Errorf("refusing to delete without confirmation — pass --yes")
			}
			fmt.Printf("Delete artifact %s? Anyone holding its link loses access. [y/N] ", id)
			line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
			if ans := strings.ToLower(strings.TrimSpace(line)); ans != "y" && ans != "yes" {
				fmt.Println("Cancelled.")
				return nil
			}
		}
		if err := c.Delete(cmd.Context(), id); err != nil {
			return err
		}
		fmt.Printf("Deleted %s\n", id)
		return nil
	},
}

// artifactURL resolves an artifact's server-reported URL by id, falling back
// to the canonical URL shape when the id isn't in the listing.
func artifactURL(cmd *cobra.Command, id string) (string, error) {
	c, err := artifacts.New()
	if err != nil {
		return "", err
	}
	list, err := c.List(cmd.Context())
	if err != nil {
		return "", err
	}
	for _, a := range list {
		if a.ID == id && a.URL != "" {
			return a.URL, nil
		}
	}
	base := strings.TrimSpace(os.Getenv("MEMCODE_WEB_APP_URL"))
	if base == "" {
		base = "https://memcode.ai"
	}
	return strings.TrimRight(base, "/") + "/code/artifact/" + id, nil
}

func runArtifactsList(cmd *cobra.Command) error {
	c, err := artifacts.New()
	if err != nil {
		return err
	}
	list, err := c.List(cmd.Context())
	if err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Println("No artifacts yet — ask the agent to publish one with the artifact tool")
		return nil
	}
	fmt.Println(artifacts.RenderList(list))
	return nil
}

func init() {
	artifactsDeleteCmd.Flags().Bool("yes", false, "delete without prompting (required when stdin is not a TTY)")
	artifactsCmd.AddCommand(artifactsListCmd, artifactsOpenCmd, artifactsDeleteCmd)
	rootCmd.AddCommand(artifactsCmd)
}
