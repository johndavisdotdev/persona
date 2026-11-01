package cmd

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
	"github.com/spf13/cobra"

	"github.com/johndavisdev/persona/internal/docker"
	"github.com/johndavisdev/persona/internal/instance"
	"github.com/johndavisdev/persona/internal/persona"
	"github.com/johndavisdev/persona/internal/ui"
)

func newListCmd() *cobra.Command {
	var asJSON, instances bool
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List configured personas (or running instances with -i)",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, out := cmd.Context(), cmd.OutOrStdout()
			var cs []instance.Container
			dockerErr := docker.Available(ctx)
			rt := instance.Current()
			rtErr := rt.Available(ctx)
			if rtErr == nil {
				var err error
				if cs, err = rt.List(ctx, ""); err != nil {
					return err
				}
			}

			if instances {
				if rtErr != nil {
					return rtErr
				}
				if asJSON {
					return json.NewEncoder(out).Encode(cs)
				}
				if len(cs) == 0 {
					fmt.Fprintln(out, ui.Dim.Render("No persona instances. Start one with `persona spawn <persona>`."))
					return nil
				}
				t := newTable("INSTANCE", "PERSONA", "STATE", "STATUS")
				for _, c := range cs {
					state := c.State
					if c.Running() {
						state = ui.OK.Render(state)
					}
					t.Row(c.Name, c.Persona, state, c.Status)
				}
				fmt.Fprintln(out, t)
				return nil
			}

			ps, errs := persona.List()
			for _, e := range errs {
				ui.Warning(cmd.ErrOrStderr(), e.Error())
			}
			running := map[string]int{}
			for _, c := range cs {
				if c.Running() {
					running[c.Persona]++
				}
			}
			if asJSON {
				type row struct {
					*persona.Persona
					Running int  `json:"running"`
					Built   bool `json:"built"`
				}
				var rows []row
				for _, p := range ps {
					rows = append(rows, row{p, running[p.Name], dockerErr == nil && docker.ImageExists(ctx, p.Image())})
				}
				return json.NewEncoder(out).Encode(rows)
			}
			if len(ps) == 0 {
				fmt.Fprintln(out, ui.Dim.Render("No personas yet. Create one with `persona new`."))
				return nil
			}
			t := newTable("NAME", "PLUGINS", "MCP", "SKILLS", "IMAGE", "RUNNING", "SUMMARY")
			for _, p := range ps {
				img := ui.Dim.Render("unknown")
				if dockerErr == nil {
					img = ui.WarnS.Render("not built")
					if docker.ImageExists(ctx, p.Image()) {
						img = ui.OK.Render("built")
					}
				}
				if edited, _ := p.EditedFiles(); len(edited) > 0 {
					img += ui.Dim.Render(" (custom)")
				}
				run := ui.Dim.Render("0")
				if n := running[p.Name]; n > 0 {
					run = ui.OK.Render(strconv.Itoa(n))
				}
				t.Row(ui.Bold.Render(p.Name), strconv.Itoa(len(p.Plugins)), strconv.Itoa(len(p.MCPServers)),
					strconv.Itoa(len(p.Skills)), img, run, clip(p.Summary, 60))
			}
			fmt.Fprintln(out, t)
			if dockerErr != nil {
				ui.Warning(cmd.ErrOrStderr(), dockerErr.Error())
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "output JSON")
	cmd.Flags().BoolVarP(&instances, "instances", "i", false, "list spawned containers instead of personas")
	return cmd
}

func newTable(headers ...string) *table.Table {
	return table.New().
		Border(lipgloss.RoundedBorder()).
		BorderStyle(lipgloss.NewStyle().Foreground(ui.Subtle)).
		Headers(headers...).
		StyleFunc(func(row, _ int) lipgloss.Style {
			if row == table.HeaderRow {
				return ui.Heading.Padding(0, 1)
			}
			return lipgloss.NewStyle().Padding(0, 1)
		})
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}
