package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestOperationalErrorDoesNotPrintUsage(t *testing.T) {
	root := newTestCommand(func(cmd *cobra.Command, args []string) error {
		return errors.New("container creation failed")
	})
	root.SetArgs([]string{"up"})

	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)

	if err := root.Execute(); err == nil {
		t.Fatal("Execute() returned nil, want an operational error")
	}
	if strings.Contains(output.String(), "Usage:") {
		t.Fatalf("operational error printed usage:\n%s", output.String())
	}
}

func TestInvalidArgumentsStillPrintUsage(t *testing.T) {
	root := newTestCommand(func(cmd *cobra.Command, args []string) error {
		t.Fatal("RunE called despite invalid arguments")
		return nil
	})
	root.SetArgs([]string{"up", "unexpected"})

	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)

	if err := root.Execute(); err == nil {
		t.Fatal("Execute() returned nil, want an argument error")
	}
	if !strings.Contains(output.String(), "Usage:") {
		t.Fatalf("argument error did not print usage:\n%s", output.String())
	}
}

func newTestCommand(runE func(*cobra.Command, []string) error) *cobra.Command {
	root := &cobra.Command{Use: "dworm"}
	root.AddCommand(&cobra.Command{
		Use:  "up",
		Args: cobra.NoArgs,
		RunE: runOperationalCommand(runE),
	})
	return root
}
