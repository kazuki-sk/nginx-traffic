// Command nginx-sitemap statically analyses an nginx configuration and
// writes map.json, index.html and map.dot. It runs once and exits.
//
// Exit codes: 0 success, 1 partial success (output written, errors found),
// 2 fatal failure.
package main

import (
	"flag"
	"fmt"
	"os"

	"nginx-sitemap/internal/app"
)

var version = "dev" // set with -ldflags "-X main.version=..."

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fs := flag.NewFlagSet("nginx-sitemap", flag.ContinueOnError)
	cfg := fs.String("config", "/etc/nginx-sitemap/sitemap.conf", "tool settings / manual entries file (optional when it is the default path)")
	nginxConf := fs.String("nginx-conf", "", "main nginx configuration (overrides settings nginx_conf)")
	out := fs.String("out", "", "output directory (overrides settings output_dir)")
	resolve := fs.Bool("resolve", false, "add a generation-time DNS snapshot for host names (needs network)")
	skip := fs.Bool("skip-unchanged", false, "do nothing when the configuration hash equals the previous map.json")
	ver := fs.String("nginx-version", "", "nginx version the configuration targets (overrides settings)")
	showVersion := fs.Bool("version", false, "print version and exit")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: nginx-sitemap [flags]\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return app.ExitFatal
	}
	if *showVersion {
		fmt.Println(version)
		return 0
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	o := app.Options{
		ConfigFile:         *cfg,
		ConfigFileOptional: !set["config"],
		ToolVersion:        version,
		Log:                os.Stderr,
	}
	if set["nginx-conf"] {
		o.NginxConf = nginxConf
	}
	if set["out"] {
		o.OutputDir = out
	}
	if set["resolve"] {
		o.Resolve = resolve
	}
	if set["skip-unchanged"] {
		o.SkipUnchanged = skip
	}
	if set["nginx-version"] {
		o.NginxVersion = ver
	}
	res, err := app.Run(o)
	if err != nil {
		fmt.Fprintf(os.Stderr, "nginx-sitemap: %v\n", err)
		if res != nil {
			return res.Code
		}
		return app.ExitFatal
	}
	if res.Skipped {
		return res.Code
	}
	m := res.Map
	fmt.Fprintf(os.Stderr, "nginx-sitemap: %d errors, %d warnings, hash %s\n", len(m.Errors), len(m.Warnings), m.ConfigHash)
	return res.Code
}
