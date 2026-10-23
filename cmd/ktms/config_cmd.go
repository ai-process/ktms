package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/ai-process/ktms/internal/declarative"
)

func (a *app) configCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "config", Short: "Keep the configuration in a file: validate, plan, apply, export"}
	var file string
	load := func() (*declarative.Config, error) {
		if file == "" {
			return nil, usageError{errors.New("-f FILE is required")}
		}
		return declarative.Load(file)
	}
	fileFlag := func(c *cobra.Command) *cobra.Command {
		c.Flags().StringVarP(&file, "file", "f", "", "configuration file")
		_ = c.MarkFlagFilename("file", "yaml", "yml")
		return c
	}

	validate := fileFlag(&cobra.Command{
		Use: "validate -f FILE", Short: "Check a configuration file", Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			if _, err := load(); err != nil {
				return err
			}
			fmt.Fprintln(a.stdout, "The configuration is valid.")
			return nil
		},
	})
	schema := &cobra.Command{
		Use: "schema", Short: "Print the file's JSON Schema", Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			_, err := a.stdout.Write(declarative.Schema())
			return err
		},
	}

	var out string
	export := &cobra.Command{
		Use: "export [-o FILE]", Short: "Write the live configuration as a file", Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			c, closeConn, err := a.t.admin()
			if err != nil {
				return err
			}
			defer closeConn()
			ctx, cancel := a.ctx()
			defer cancel()
			live, err := declarative.Fetch(ctx, c)
			if err != nil {
				return err
			}
			var w io.Writer = a.stdout
			if out != "" {
				f, err := os.Create(out)
				if err != nil {
					return err
				}
				defer f.Close()
				w = f
			}
			return declarative.Export(live, w)
		},
	}
	export.Flags().StringVarP(&out, "output", "o", "", "output file (default stdout)")

	var prune, yes, detailed bool
	planOrApply := func(apply bool) func(*cobra.Command, []string) error {
		return func(*cobra.Command, []string) error {
			cfg, err := load()
			if err != nil {
				return err
			}
			c, closeConn, err := a.t.admin()
			if err != nil {
				return err
			}
			defer closeConn()
			ctx, cancel := a.ctx()
			defer cancel()
			live, err := declarative.Fetch(ctx, c)
			if err != nil {
				return err
			}
			plan, err := declarative.Build(cfg, live, declarative.Options{Prune: prune})
			if err != nil {
				return err
			}
			plan.Write(a.stdout)
			if !apply {
				if detailed && !plan.Empty() {
					return codeError{exitChanges}
				}
				return nil
			}
			if plan.Empty() {
				return nil
			}
			if !yes {
				if err := a.confirm("Apply these changes?"); err != nil {
					return err
				}
			}
			// Time at the confirmation prompt must not eat into the apply's deadline.
			actx, acancel := context.WithTimeout(context.Background(), a.timeout)
			defer acancel()
			res, err := declarative.Apply(actx, c, live, plan, a.stdout)
			printMinted(a.stdout, res)
			if err != nil {
				return err
			}
			fmt.Fprintln(a.stdout, "Applied.")
			return nil
		}
	}
	plan := fileFlag(&cobra.Command{Use: "plan -f FILE", Short: "Show what apply would change", Args: cobra.NoArgs, RunE: planOrApply(false)})
	plan.Flags().BoolVar(&prune, "prune", false, "delete what managed sections do not list")
	plan.Flags().BoolVar(&detailed, "detailed-exitcode", false, "exit 3 when there are changes")
	apply := fileFlag(&cobra.Command{Use: "apply -f FILE", Short: "Change Keystone to match the file", Args: cobra.NoArgs, RunE: planOrApply(true)})
	apply.Flags().BoolVar(&prune, "prune", false, "delete what managed sections do not list")
	apply.Flags().BoolVar(&yes, "yes", false, "apply without asking")

	cmd.AddCommand(validate, plan, apply, export, schema)
	return cmd
}

func printMinted(w io.Writer, res *declarative.Result) {
	if res == nil || len(res.Minted) == 0 {
		return
	}
	fmt.Fprintln(w, "\nNew client keys. They are shown only now; store them in your secret manager:")
	for name, key := range res.Minted {
		fmt.Fprintf(w, "  %s: %s\n", name, key)
	}
}
