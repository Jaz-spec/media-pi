// facpi — FAC classroom Pi recording orchestrator.
//
// Subcommands:
//
//	facpi daemon              run the background daemon (upload worker in Phase 1)
//	facpi enqueue <file>      insert a file into the upload queue
//	facpi list                print the current upload queue
//	facpi retry <id>          move a failed upload back to pending
//	facpi version             print build info
//
// Phase 1 deliberately skips `tui` and `record` subcommands — they arrive in
// Phases 2–4.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/foundersandcoders/media-pi/internal/config"
	"github.com/foundersandcoders/media-pi/internal/execsh"
	"github.com/foundersandcoders/media-pi/internal/orchestrator"
	"github.com/foundersandcoders/media-pi/internal/platform"
	"github.com/foundersandcoders/media-pi/internal/scheduler"
	"github.com/foundersandcoders/media-pi/internal/state"
	"github.com/foundersandcoders/media-pi/internal/tui"
	"github.com/foundersandcoders/media-pi/internal/worker"
)

var version = "dev" // overridable via -ldflags "-X main.version=..."

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	sub := os.Args[1]
	args := os.Args[2:]

	var err error
	switch sub {
	case "daemon":
		err = cmdDaemon(args)
	case "enqueue":
		err = cmdEnqueue(args)
	case "list":
		err = cmdList(args)
	case "retry":
		err = cmdRetry(args)
	case "record":
		err = cmdRecord(args)
	case "tui":
		err = cmdTUI(args)
	case "version", "--version", "-v":
		fmt.Println(version)
	case "help", "--help", "-h":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand: %s\n\n", sub)
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "facpi %s: %v\n", sub, err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `facpi — FAC Pi recording orchestrator

Usage:
  facpi daemon                 run the background daemon
  facpi tui                    open the Bubble Tea dashboard (read-only, Phase 3)
  facpi record start           start a recording via record.sh
  facpi record stop            stop active recording + enqueue upload
  facpi record status          show recorder status
  facpi enqueue <file>         add a file to the upload queue
  facpi list                   list recent uploads
  facpi retry <id>             retry a failed upload
  facpi version                print version

Reads configuration from .env in the current working directory.
`)
}

func cmdDaemon(_ []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := cfg.RequireUploadConfig(); err != nil {
		return fmt.Errorf("config: %w", err)
	}

	db, err := openWritable(cfg.DBPath)
	if err != nil {
		return err
	}
	defer db.Close()

	// Clean up any recordings left in 'recording' state from a previous crash.
	if n, err := db.AdoptStaleActiveRecordings(context.Background()); err != nil {
		return fmt.Errorf("adopt stale recordings: %w", err)
	} else if n > 0 {
		fmt.Printf("facpi: marked %d stale recording(s) as failed/ffmpeg_died\n", n)
	}

	execer, err := execsh.New("")
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	up := worker.NewUpload(db, execer, cfg)
	up.Wake()

	recorder := orchestrator.NewRecorder(db, execer, cfg)
	consumer := orchestrator.NewCommandConsumer(db, recorder, up, cfg)

	go runHeartbeat(ctx, db)
	go func() {
		if err := consumer.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			fmt.Fprintf(os.Stderr, "command consumer: %v\n", err)
		}
	}()

	// Scheduler (Phase 5). Only active if the platform API config is set.
	if cfg.FACAPIBaseURL != "" && cfg.FACPiID != "" {
		client := platform.New(cfg)
		sched := scheduler.New(db, cfg, client, recorder, up)
		go func() {
			if err := sched.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
				fmt.Fprintf(os.Stderr, "scheduler: %v\n", err)
			}
		}()
	} else {
		fmt.Println("facpi: scheduler disabled (FAC_API_BASE_URL or FAC_PI_ID unset)")
	}

	fmt.Printf("facpi daemon v%s starting (db=%s)\n", version, cfg.DBPath)
	err = up.Run(ctx)
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func cmdEnqueue(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: facpi enqueue <file>")
	}
	path := args[0]

	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if info.Size() == 0 {
		return fmt.Errorf("%s: file is empty", path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s: not a regular file", path)
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("abs path: %w", err)
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	db, err := openWritable(cfg.DBPath)
	if err != nil {
		return err
	}
	defer db.Close()

	id, err := db.EnqueueUpload(context.Background(), 0, abs)
	if err != nil {
		if errors.Is(err, state.ErrUploadExists) {
			return fmt.Errorf("%s is already in the queue", abs)
		}
		return err
	}
	fmt.Printf("enqueued id=%d file=%s\n", id, abs)
	return nil
}

func cmdList(_ []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	db, err := openWritable(cfg.DBPath)
	if err != nil {
		return err
	}
	defer db.Close()

	uploads, err := db.ListUploads(context.Background(), 50)
	if err != nil {
		return err
	}
	if len(uploads) == 0 {
		fmt.Println("(queue is empty)")
		return nil
	}
	fmt.Printf("%-4s %-10s %-7s %-19s %s\n", "ID", "STATUS", "TRIES", "ENQUEUED (UTC)", "FILE")
	for _, u := range uploads {
		fmt.Printf("%-4d %-10s %-7d %-19s %s\n",
			u.ID, u.Status, u.AttemptCount,
			u.EnqueuedAt.Format("2006-01-02 15:04:05"),
			u.FilePath,
		)
		if u.LastError.Valid && u.LastError.String != "" {
			fmt.Printf("     └─ last_error: %s\n", u.LastError.String)
		}
	}
	return nil
}

func cmdTUI(_ []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	// Sanity: if the DB doesn't exist yet, give a friendly hint rather than
	// a cryptic open error. The daemon is the one that runs migrations.
	if _, err := os.Stat(cfg.DBPath); err != nil {
		return fmt.Errorf("state db %s not found — run `facpi daemon` at least once first (%w)", cfg.DBPath, err)
	}
	return tui.Run(cfg)
}

func cmdRecord(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: facpi record {start|stop|status}")
	}
	action := args[0]

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	db, err := openWritable(cfg.DBPath)
	if err != nil {
		return err
	}
	defer db.Close()

	execer, err := execsh.New("")
	if err != nil {
		return err
	}
	rec := orchestrator.NewRecorder(db, execer, cfg)
	ctx := context.Background()

	switch action {
	case "start":
		r, err := rec.Start(ctx, "")
		if err != nil {
			return err
		}
		fmt.Printf("recording id=%d file=%s pid=%d\n",
			r.ID, r.FilePath, r.FFmpegPID.Int64)
		return nil
	case "stop":
		res, err := rec.Stop(ctx, "")
		if err != nil {
			return err
		}
		if res.Recording != nil {
			fmt.Printf("stopped recording id=%d file=%s\n",
				res.Recording.ID, res.Recording.FilePath)
		}
		if res.UploadID > 0 {
			fmt.Printf("enqueued upload id=%d\n", res.UploadID)
		}
		return nil
	case "status":
		s, err := rec.Status(ctx)
		if err != nil {
			return err
		}
		fmt.Println(s)
		return nil
	default:
		return fmt.Errorf("unknown record action %q (want start|stop|status)", action)
	}
}

func cmdRetry(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: facpi retry <id>")
	}
	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return fmt.Errorf("invalid id %q: %w", args[0], err)
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	db, err := openWritable(cfg.DBPath)
	if err != nil {
		return err
	}
	defer db.Close()

	if err := db.RetryUpload(context.Background(), id); err != nil {
		return err
	}
	fmt.Printf("id=%d moved back to pending — run `facpi daemon` (or it'll pick up on next wake)\n", id)
	return nil
}

func openWritable(path string) (*state.DB, error) {
	db, err := state.Open(path, false)
	if err != nil {
		return nil, err
	}
	if err := db.Migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func runHeartbeat(ctx context.Context, db *state.DB) {
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	pid := os.Getpid()
	beat := func() {
		now := time.Now().UTC().Unix()
		_, err := db.ExecContext(ctx, `
			INSERT INTO daemon_heartbeat (id, last_beat_at, version, pid)
			VALUES (1, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET last_beat_at=excluded.last_beat_at,
			                              version=excluded.version,
			                              pid=excluded.pid`,
			now, version, pid)
		// Ignore context-cancelled errors; surface real failures.
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, sql.ErrConnDone) {
			// Don't spam — one heartbeat fail per tick is enough.
		}
	}
	beat()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			beat()
		}
	}
}

