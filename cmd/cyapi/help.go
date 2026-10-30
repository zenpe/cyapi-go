package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
)

var commandHelp = map[string]string{
	"install": `install [--dir DIRECTORY] [--listen HOST:PORT]
        [--service auto|systemd|none] [--user USER] [--non-interactive]
        [--username NAME --email ADDRESS]
Install in the current directory by default. Creates configuration, secrets,
SQLite schema and the first account; preserves existing settings and data.
Password is read without echo, or from CYAPI_INSTALL_PASSWORD for unattended use.
Linux systemd registration requires root. The server runs as an unprivileged user.
With --service none, a start.sh script is generated but autostart is not configured.
Example: ./cyapi install --listen 0.0.0.0:8080`,
	"serve": `serve [--config config.yaml] [--log-file PATH]
Run the API in the foreground. Validates schema; never migrates it automatically.
Installed services use bounded rotating logs. Stop with SIGTERM / Ctrl+C.
Example: ./cyapi serve --config ./config.yaml`,
	"doctor": `doctor [--config config.yaml] [--network]
Check local configuration, permissions, schema, source files, backups and service.
Does not create a database, migrate, send email or change configuration.
--network additionally checks GitHub connectivity (bounded timeout).
Exit 0: no blocking errors (warnings may exist); exit 1: repair required.`,
	"status": `status [--config config.yaml]
Show installed service, health endpoint and last maintenance results.
Returns failure if the API is not ready. Does not modify data.`,
	"upgrade": `upgrade --file NEW_BINARY [--sha256 EXPECTED_SHA256] [--config config.yaml]
Linux systemd installations only; requires service-management privileges.
Stages a matching Linux binary, stops the service, makes a recovery backup,
migrates using the candidate, switches binaries and checks readiness.
After migration failure or failed readiness, the service stays stopped for recovery.
Does not download updates. Obtain the binary and checksum from a trusted release.
Example: ./cyapi upgrade --file ./cyapi-new --sha256 <release-checksum>`,
	"backup": `backup --output NEW_DIRECTORY [--config config.yaml] [--include-config]
Create a consistent checksummed database and source snapshot backup.
--include-config adds restricted config/secrets recovery files; keep them private.
Does not capture credentials supplied only through process environment variables.
Example: ./cyapi backup --output ./backups/manual-20261029 --include-config`,
	"migrate": `migrate [--config config.yaml]
Explicitly apply transactional schema migrations. Stop the API first; back up before upgrading.
Does not require a login secret. Never use an older binary against a newer schema.`,
	"schema-status": `schema-status [--config config.yaml]
Verify that the database schema exactly matches this program. Does not migrate.`,
	"sync-sources": `sync-sources [--config config.yaml] [--force] [--dry-run]
Download the approved GitHub Release ZIP, validate and atomically publish sources.
--dry-run downloads and validates without publishing; --force republishes unchanged content.
An existing running synchronization is skipped. Failure preserves current sources.`,
	"sources": `sources status [--config config.yaml]
sources rollback --generation ID [--config config.yaml]
Status verifies the current source files. Rollback changes the published generation.`,
	"version": `version | --version
Print version, revision, Go version and target architecture. No config required.`,
}

func printHelp(command string) error {
	if command != "" {
		h, ok := commandHelp[command]
		if !ok {
			return fmt.Errorf("unknown command %q; run cyapi help", command)
		}
		fmt.Println("Usage: cyapi " + h)
		return nil
	}
	fmt.Print(`Cyapi — lightweight NAS music API
Usage: cyapi <command> [options]

Install/run:  install, serve, status
Maintenance:  doctor, backup, upgrade
Database:     migrate, schema-status
Sources:      sync-sources, sources status, sources rollback
Information:  help, version

./cyapi install                    Install in the current directory
./cyapi help install               Installation options and example
./cyapi doctor --network           Diagnose configuration and GitHub access
./cyapi status                     Check service and scheduled maintenance

Every command supports --help. Help/version do not require configuration.
`)
	return nil
}

func handleCLI(args []string, log *slog.Logger) (bool, error) {
	if len(args) == 0 {
		return true, printHelp("")
	}
	if args[0] == "--version" || args[0] == "-v" {
		if len(args) != 1 {
			return true, errors.New("--version takes no arguments")
		}
		fmt.Printf("cyapi %s commit=%s go=%s %s/%s\n", version, commit, runtimeVersion(), targetOS(), targetArch())
		return true, nil
	}
	if args[0] == "help" {
		if len(args) > 2 {
			return true, errors.New("usage: cyapi help [command]")
		}
		if len(args) == 2 {
			return true, printHelp(args[1])
		}
		return true, printHelp("")
	}
	if args[0] == "--help" || args[0] == "-h" {
		return true, printHelp("")
	}
	for _, arg := range args[1:] {
		if arg == "--help" || arg == "-h" {
			return true, printHelp(args[0])
		}
	}
	switch args[0] {
	case "install":
		return true, installCommand(args[1:], log)
	case "status":
		return true, inspectCommand(args[1:], false)
	case "doctor":
		return true, inspectCommand(args[1:], true)
	case "upgrade":
		return true, upgradeCommand(args[1:], log)
	}
	if _, ok := commandHelp[args[0]]; !ok {
		fmt.Fprintln(os.Stderr, "Run cyapi help for available commands.")
		return true, fmt.Errorf("unknown command %q", strings.TrimSpace(args[0]))
	}
	return false, nil
}
