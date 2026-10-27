package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/chux0519/memspace/internal/client"
	"github.com/chux0519/memspace/internal/model"
	"github.com/chux0519/memspace/internal/project"
	"github.com/chux0519/memspace/internal/syncer"
)

func searchCmd() *cobra.Command {
	var allProjects, asJSON bool
	var mode string
	var projectIDs, kinds, agents []string
	var limit int
	var since time.Duration
	cmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Search history (current project by default)",
		Example: `  memspace search 认证超时
  memspace search --all "Stripe webhook 幂等"
  memspace search --kind failure --since 72h "deadline exceeded"`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			creds, err := syncer.LoadCredentials(cfg.Client.CredentialsPath())
			if err != nil {
				return err
			}
			req := model.SearchRequest{Query: strings.Join(args, " "), Kinds: kinds, AgentTypes: agents, Limit: limit, Mode: mode}
			switch {
			case allProjects:
				req.Scope = model.ScopeAllProjects
			case len(projectIDs) > 0:
				req.Scope, req.ProjectIDs = model.ScopeSelectedProjects, projectIDs
			default:
				h := project.Detect("")
				req.Scope, req.GitIdentity = model.ScopeCurrentProject, h.GitIdentity
				if h.GitIdentity == "" {
					req.ProjectName = h.Name
				}
			}
			if since > 0 {
				t := time.Now().Add(-since)
				req.Since = &t
			}
			url := creds.ServerURL
			if cfg.Client.ServerURL != "" {
				url = cfg.Client.ServerURL
			}
			resp, err := client.New(url, creds.Token).Search(cmd.Context(), req)
			if err != nil {
				return err
			}
			if asJSON {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(resp)
			}
			if resp.Note != "" {
				fmt.Println(resp.Note)
			}
			if len(resp.Results) == 0 {
				fmt.Println("No results.")
				return nil
			}
			for _, r := range resp.Results {
				where := r.ProjectName
				if r.DeviceName != "" {
					where += " · " + r.DeviceName
				}
				if r.AgentType != "" {
					where += " · " + r.AgentType
				}
				match := r.Match
				if r.Similarity > 0 {
					match += fmt.Sprintf(" %.2f", r.Similarity)
				}
				fmt.Printf("%s  %-8s %s  [%s]\n", r.OccurredAt.Local().Format("2006-01-02 15:04"), r.Kind, where, match)
				if r.Title != "" {
					fmt.Printf("  %s\n", r.Title)
				}
				fmt.Printf("  %s\n", strings.ReplaceAll(strings.TrimSpace(r.Snippet), "\n", " "))
				fmt.Printf("  ref %s\n\n", r.RefID)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&allProjects, "all", false, "search every project")
	cmd.Flags().StringSliceVar(&projectIDs, "project", nil, "search these project IDs")
	cmd.Flags().StringSliceVar(&kinds, "kind", nil, "prompt, reply, failure, command, change, summary")
	cmd.Flags().StringSliceVar(&agents, "agent", nil, "claude-code, codex, dsh")
	cmd.Flags().IntVar(&limit, "limit", 20, "maximum results")
	cmd.Flags().DurationVar(&since, "since", 0, "only results newer than this (e.g. 72h)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the raw response")
	cmd.Flags().StringVar(&mode, "mode", "", "keyword or semantic (default: hybrid when the server has semantic search)")
	return cmd
}
