package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strconv"

	"github.com/relux-works/a2a-swarm/internal/daemon"
)

func cmdDaemon(ctx context.Context, a *App, args []string) error {
	const help = "usage: a2a daemon serve|register-station|unregister-station|enroll|migrate|status; migrate <identity> --to <daemon-base> [--resume|--rollback]; enroll --dir HOME/.a2a --from - [--rotate]; serve|register-station|unregister-station --home ABSOLUTE_PRIVATE_HOME [--socket ABSOLUTE_PATH]; register-station --station S --identity I --uid U --emit - [--rotate]; unregister-station --station S"
	if len(args) == 0 {
		return usagef("%s", help)
	}
	if args[0] == "--help" || args[0] == "-h" || args[0] == "help" {
		_, err := fmt.Fprintln(a.Stdout, help)
		return err
	}
	if args[0] == "migrate" {
		return cmdDaemonMigrate(ctx, a, args[1:])
	}
	if args[0] == "status" && len(args) == 1 {
		return daemonStationStatus(a)
	}
	if args[0] == "enroll" {
		return cmdDaemonEnroll(ctx, a, args[1:])
	}
	sub := args[0]
	if sub != "serve" && sub != "register-station" && sub != "unregister-station" {
		return usagef("%s", help)
	}
	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // flag errors can contain argv; keep diagnostics closed.
	home := fs.String("home", "", "Daemon-only home (0700)")
	var station, identity, uidText, emit string
	var rotate bool
	socket := daemon.DefaultSocket()
	if sub != "unregister-station" {
		fs.StringVar(&socket, "socket", socket, "absolute Unix socket path")
	}
	if sub != "serve" {
		fs.StringVar(&station, "station", "", "station local name")
	}
	if sub == "register-station" {
		fs.StringVar(&identity, "identity", "", "bound local identity")
		fs.StringVar(&uidText, "uid", "", "kernel peer uid")
		fs.StringVar(&emit, "emit", "", "- for enrollment pipe")
		fs.BoolVar(&rotate, "rotate", false, "explicitly replace an existing station binding")
	}
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, err = fmt.Fprintln(a.Stdout, help)
			return err
		}
		return usagef("%s", help)
	}
	if fs.NArg() != 0 || !filepath.IsAbs(*home) || filepath.Clean(*home) != *home || *home == "/" || (sub != "serve" && station == "") {
		return usagef("%s", help)
	}
	if sub != "unregister-station" && (daemon.ValidateSocketPath(socket) != nil || filepath.Clean(socket) != socket) {
		return usagef("%s", help)
	}
	switch sub {
	case "serve":
		return daemon.ServeWithHandler(ctx, *home, socket, a.Stderr, a.daemonHandler(*home))
	case "unregister-station":
		return daemon.Unregister(*home, station)
	default:
		uid, err := strconv.ParseUint(uidText, 10, 32)
		if err != nil || strconv.FormatUint(uid, 10) != uidText || emit != "-" || identity == "" {
			return usagef("%s", help)
		}
		secret, err := daemon.Register(*home, station, identity, uint32(uid), rotate)
		if err != nil {
			return err
		}
		payload := struct {
			Socket   string `json:"socket"`
			Identity string `json:"identity"`
			Station  string `json:"station"`
			Secret   string `json:"secret_b64"`
		}{socket, identity, station, base64.StdEncoding.EncodeToString(secret)}
		// This is the sole deliberate secret output, directly into the caller's pipe.
		// On an output failure, registration may have committed; rotate to recover.
		encoded, err := json.Marshal(payload)
		if err != nil {
			return errors.New("daemon: enrollment encoding failed")
		}
		encoded = append(encoded, '\n')
		n, err := a.Stdout.Write(encoded)
		if err != nil || n != len(encoded) {
			return errors.New("daemon: enrollment output failed; registration may have committed")
		}
		return nil
	}
}
