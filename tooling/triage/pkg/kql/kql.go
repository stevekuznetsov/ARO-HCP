package kql

import (
	"bytes"
	"fmt"
	"os"
	"text/template"
	"time"

	"github.com/spf13/cobra"
)

// TemplateData holds the context available to KQL Go templates.
type TemplateData struct {
	ClusterName     string
	ClusterURI      string
	Region          string
	HCPDatabase     string
	ServiceDatabase string
	StartTime       time.Time
	EndTime         time.Time
	ResourceGroup   string
	Extra           map[string]string
}

// KustoEndpoint constructs the Kusto cluster URL from a cluster name and region.
func KustoEndpoint(clusterName, region string) string {
	return fmt.Sprintf("https://%s.%s.kusto.windows.net", clusterName, region)
}

// KustoHost returns just the hostname of a Kusto cluster endpoint.
func KustoHost(clusterName, region string) string {
	return fmt.Sprintf("%s.%s.kusto.windows.net", clusterName, region)
}

// kqlDatetime formats a time.Time as a KQL datetime literal, e.g. datetime(2024-01-15T10:30:00Z).
func kqlDatetime(t time.Time) string {
	return fmt.Sprintf("datetime(%s)", t.UTC().Format(time.RFC3339))
}

// RenderTemplate reads a KQL template file and renders it with the given data.
func RenderTemplate(kqlFilePath string, data TemplateData) (string, error) {
	kqlTemplate, err := os.ReadFile(kqlFilePath)
	if err != nil {
		return "", fmt.Errorf("failed to read KQL file %q: %w", kqlFilePath, err)
	}

	funcMap := template.FuncMap{
		"kqlDatetime": kqlDatetime,
	}
	tmpl, err := template.New("kql").Funcs(funcMap).Parse(string(kqlTemplate))
	if err != nil {
		return "", fmt.Errorf("failed to parse KQL template: %w", err)
	}

	var rendered bytes.Buffer
	if err := tmpl.Execute(&rendered, data); err != nil {
		return "", fmt.Errorf("failed to render KQL template: %w", err)
	}
	return rendered.String(), nil
}

// RawTemplateOptions holds the common CLI flags for KQL template rendering.
type RawTemplateOptions struct {
	ClusterName     string
	Region          string
	HCPDatabase     string
	ServiceDatabase string
	StartTime       string
	EndTime         string
	ResourceGroup   string
	KQLFile         string
	ExtraVars       map[string]string
}

// BindTemplateOptions binds the standard KQL template flags to a cobra command.
func BindTemplateOptions(opts *RawTemplateOptions, cmd *cobra.Command) {
	cmd.Flags().StringVar(&opts.ClusterName, "cluster-name", opts.ClusterName, "Kusto cluster name.")
	cmd.Flags().StringVar(&opts.Region, "region", opts.Region, "Azure region where the Kusto cluster is located (e.g. westus3).")
	cmd.Flags().StringVar(&opts.HCPDatabase, "hcp-database", opts.HCPDatabase, "Kusto database for hosted control plane logs.")
	cmd.Flags().StringVar(&opts.ServiceDatabase, "service-database", opts.ServiceDatabase, "Kusto database for service logs.")
	cmd.Flags().StringVar(&opts.StartTime, "start-time", opts.StartTime, "Query start time in RFC3339 format (e.g. 2024-01-15T10:30:00Z).")
	cmd.Flags().StringVar(&opts.EndTime, "end-time", opts.EndTime, "Query end time in RFC3339 format (e.g. 2024-01-16T10:30:00Z).")
	cmd.Flags().StringVar(&opts.ResourceGroup, "resource-group", opts.ResourceGroup, "Azure resource group name (available as a template variable).")
	cmd.Flags().StringVar(&opts.KQLFile, "kql-file", opts.KQLFile, "Path to a KQL file to use as a Go template for the query.")
	cmd.Flags().StringToStringVar(&opts.ExtraVars, "extra-var", opts.ExtraVars, "Extra key=value pairs available to the KQL template as .Extra.<key>. Can be repeated.")
}

// ValidatedTemplateOptions holds validated and parsed versions of the template options.
type ValidatedTemplateOptions struct {
	ClusterName     string
	Region          string
	HCPDatabase     string
	ServiceDatabase string
	StartTime       time.Time
	EndTime         time.Time
	ResourceGroup   string
	KQLFile         string
	ExtraVars       map[string]string
}

// Validate checks all required fields and parses time values.
func (o *RawTemplateOptions) Validate() (*ValidatedTemplateOptions, error) {
	if o.ClusterName == "" {
		return nil, fmt.Errorf("--cluster-name is required")
	}
	if o.Region == "" {
		return nil, fmt.Errorf("--region is required")
	}
	if o.HCPDatabase == "" {
		return nil, fmt.Errorf("--hcp-database is required")
	}
	if o.ServiceDatabase == "" {
		return nil, fmt.Errorf("--service-database is required")
	}
	if o.StartTime == "" {
		return nil, fmt.Errorf("--start-time is required")
	}
	if o.EndTime == "" {
		return nil, fmt.Errorf("--end-time is required")
	}
	if o.ResourceGroup == "" {
		return nil, fmt.Errorf("--resource-group is required")
	}
	if o.KQLFile == "" {
		return nil, fmt.Errorf("--kql-file is required")
	}

	startTime, err := time.Parse(time.RFC3339, o.StartTime)
	if err != nil {
		return nil, fmt.Errorf("invalid --start-time %q: must be RFC3339 format (e.g. 2024-01-15T10:30:00Z): %w", o.StartTime, err)
	}
	endTime, err := time.Parse(time.RFC3339, o.EndTime)
	if err != nil {
		return nil, fmt.Errorf("invalid --end-time %q: must be RFC3339 format (e.g. 2024-01-16T10:30:00Z): %w", o.EndTime, err)
	}
	if !startTime.Before(endTime) {
		return nil, fmt.Errorf("--start-time (%s) must be before --end-time (%s)", o.StartTime, o.EndTime)
	}

	if _, err := os.Stat(o.KQLFile); err != nil {
		return nil, fmt.Errorf("--kql-file %q: %w", o.KQLFile, err)
	}

	return &ValidatedTemplateOptions{
		ClusterName:     o.ClusterName,
		Region:          o.Region,
		HCPDatabase:     o.HCPDatabase,
		ServiceDatabase: o.ServiceDatabase,
		StartTime:       startTime,
		EndTime:         endTime,
		ResourceGroup:   o.ResourceGroup,
		KQLFile:         o.KQLFile,
		ExtraVars:       o.ExtraVars,
	}, nil
}

// TemplateData builds the TemplateData struct for rendering.
func (o *ValidatedTemplateOptions) TemplateData() TemplateData {
	return TemplateData{
		ClusterName:     o.ClusterName,
		ClusterURI:      KustoEndpoint(o.ClusterName, o.Region),
		Region:          o.Region,
		HCPDatabase:     o.HCPDatabase,
		ServiceDatabase: o.ServiceDatabase,
		StartTime:       o.StartTime,
		EndTime:         o.EndTime,
		ResourceGroup:   o.ResourceGroup,
		Extra:           o.ExtraVars,
	}
}

// Render reads the KQL template file and renders it using the validated options.
func (o *ValidatedTemplateOptions) Render() (string, error) {
	return RenderTemplate(o.KQLFile, o.TemplateData())
}
