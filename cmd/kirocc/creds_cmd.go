// [fork] New file added in fork. Implements the `kirocc creds` subcommand
// family for offline credential-pool inspection: listing accounts, printing
// the default path, etc. Never starts the proxy; never makes upstream calls.

package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/niuma/kirocc-pro/internal/config"
	"github.com/niuma/kirocc-pro/internal/pool"
)

func runCredsCmd(args []string) error {
	if len(args) == 0 {
		printCredsUsage(os.Stderr)
		return fmt.Errorf("missing creds subcommand")
	}
	switch args[0] {
	case "list":
		return runCredsList(args[1:])
	case "path":
		return runCredsPath()
	case "-h", "--help", "help":
		printCredsUsage(os.Stdout)
		return nil
	default:
		printCredsUsage(os.Stderr)
		return fmt.Errorf("unknown creds subcommand: %s", args[0])
	}
}

func printCredsUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage: kirocc creds <subcommand>")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Subcommands:")
	fmt.Fprintln(w, "  list [--json PATH]   List credentials from the JSON pool file")
	fmt.Fprintln(w, "  path                 Print the default credentials.json path")
	fmt.Fprintln(w, "  help                 Show this message")
}

func runCredsPath() error {
	p := config.DefaultCredsJSONPath()
	if p == "" {
		return fmt.Errorf("unable to resolve home directory")
	}
	fmt.Println(p)
	return nil
}

func runCredsList(args []string) error {
	fs := flag.NewFlagSet("kirocc creds list", flag.ContinueOnError)
	var jsonPath string
	fs.StringVar(&jsonPath, "json", "", "credentials JSON path (default ~/.config/kirocc/credentials.json)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if jsonPath == "" {
		jsonPath = config.DefaultCredsJSONPath()
		if jsonPath == "" {
			return fmt.Errorf("could not resolve default creds path; pass --json")
		}
	}
	creds, err := pool.LoadFromJSON(jsonPath)
	if err != nil {
		return fmt.Errorf("load %s: %w", jsonPath, err)
	}

	fmt.Fprintf(os.Stderr, "loaded %d credential(s) from %s\n\n", len(creds), jsonPath)

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tLABEL\tPRIORITY\tSTATUS\tREGION\tPROFILE ARN\tEXPIRES")
	for _, c := range creds {
		status := "active"
		if c.Disabled {
			status = "disabled"
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%s\t%s\n",
			c.ID,
			c.Label,
			c.Priority,
			status,
			c.Region,
			maskProfileARN(c.ProfileARN),
			formatExpiry(c.ExpiresAt),
		)
	}
	return tw.Flush()
}

// maskProfileARN keeps the resource shape readable but hides the account ID,
// e.g. "arn:aws:codewhisperer:us-east-1:123456789012:profile/EXAMPLE" →
//      "arn:aws:codewhisperer:us-east-1:****:profile/EXAMPLE".
func maskProfileARN(arn string) string {
	parts := strings.Split(arn, ":")
	if len(parts) < 6 {
		return arn
	}
	parts[4] = "****"
	return strings.Join(parts, ":")
}

// formatExpiry renders a unix-second token expiry as "in 3h" / "expired 2d ago".
func formatExpiry(unix int64) string {
	if unix == 0 {
		return "-"
	}
	d := time.Until(time.Unix(unix, 0))
	if d > 0 {
		return "in " + roughDuration(d)
	}
	return "expired " + roughDuration(-d) + " ago"
}

func roughDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
