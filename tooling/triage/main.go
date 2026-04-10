package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/dusted-go/logging/prettylog"
	"github.com/go-logr/logr"
	"github.com/spf13/cobra"

	failingtests "github.com/Azure/ARO-HCP/tooling/triage/cmd/failing-tests"
	jobfailures "github.com/Azure/ARO-HCP/tooling/triage/cmd/job-failures"
	kustodeeplink "github.com/Azure/ARO-HCP/tooling/triage/cmd/kusto-deeplink"
	kustoquery "github.com/Azure/ARO-HCP/tooling/triage/cmd/kusto-query"
	kustoquerytable "github.com/Azure/ARO-HCP/tooling/triage/cmd/kusto-query-table"
	tracerequest "github.com/Azure/ARO-HCP/tooling/triage/cmd/trace-request"
)

func main() {
	logger := createLogger(0)

	// Create a root context with the logger and signal handling
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var logVerbosity int

	cmd := &cobra.Command{
		Use:              "triage",
		Short:            "Triage tools for ARO HCP test failures.",
		SilenceUsage:     true,
		TraverseChildren: true,
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			ctx = logr.NewContext(ctx, createLogger(logVerbosity))
			cmd.SetContext(ctx)
		},
		CompletionOptions: cobra.CompletionOptions{
			HiddenDefaultCmd: true,
		},
	}

	cmd.PersistentFlags().IntVarP(&logVerbosity, "verbosity", "v", 0, "set the verbosity level")

	commands := []func() (*cobra.Command, error){
		failingtests.NewCommand,
		jobfailures.NewCommand,
		kustodeeplink.NewCommand,
		kustoquery.NewCommand,
		kustoquerytable.NewCommand,
		tracerequest.NewCommand,
	}
	for _, newCmd := range commands {
		c, err := newCmd()
		if err != nil {
			logger.Error(err, "failed to create command")
			os.Exit(1)
		}
		cmd.AddCommand(c)
	}

	cmd.SetHelpCommand(&cobra.Command{Hidden: true})

	if err := cmd.ExecuteContext(ctx); err != nil {
		logger.Error(err, "command failed")
		os.Exit(1)
	}
}

func createLogger(verbosity int) logr.Logger {
	level := slog.Level(verbosity * -1)
	prettyHandler := prettylog.NewHandler(&slog.HandlerOptions{
		Level:       level,
		AddSource:   false,
		ReplaceAttr: nil,
	})
	slog.SetDefault(slog.New(prettyHandler))
	slog.SetLogLoggerLevel(level)
	return logr.FromSlogHandler(prettyHandler)
}
