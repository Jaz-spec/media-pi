// Package config loads runtime configuration from .env and environment.
// The same .env file is sourced by the bash scripts; facpi reads it via
// godotenv so it inherits the operator's setup without duplication.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/joho/godotenv"
)

type Config struct {
	// Pipeline (shared with bash scripts)
	FFmpegInputArgs           string
	RecordingsDir             string
	LogDir                    string
	PIDFile                   string
	DiskSpaceMinMB            int
	FACAPIURL                 string
	FACAPIKey                 string
	UploadMaxAttempts         int
	UploadBackoffStartSeconds int

	// facpi-specific
	FACPiID               string
	FACAPIBaseURL         string
	DBPath                string
	SchedulePollInterval  time.Duration
	ScheduleLookahead     time.Duration
	ScheduleLookbehind    time.Duration
	SchedulerTick         time.Duration
	InterlockWindow       time.Duration
	InterlockOverrideLead time.Duration
	TUIRefresh            time.Duration
	ReportStatus          bool
}

// Load reads .env from the current working directory (falling back to process
// env if absent), and returns a validated Config. Missing required fields
// produce an explicit error rather than a silent default.
func Load() (Config, error) {
	// godotenv.Load is best-effort: if .env doesn't exist we still read from
	// the real environment, which is what systemd/tests will use.
	_ = godotenv.Load()

	c := Config{
		FFmpegInputArgs:           os.Getenv("FFMPEG_INPUT_ARGS"),
		RecordingsDir:             getenvDefault("RECORDINGS_DIR", "./recordings"),
		LogDir:                    getenvDefault("LOG_DIR", "./logs"),
		PIDFile:                   getenvDefault("PID_FILE", "/tmp/fac-recorder.pid"),
		FACAPIURL:                 os.Getenv("FAC_API_URL"),
		FACAPIKey:                 os.Getenv("FAC_API_KEY"),
		FACPiID:                   os.Getenv("FAC_PI_ID"),
		FACAPIBaseURL:             os.Getenv("FAC_API_BASE_URL"),
		DBPath:                    getenvDefault("FACPI_DB_PATH", "./state.db"),
	}

	var err error
	if c.DiskSpaceMinMB, err = getIntDefault("DISK_SPACE_MIN_MB", 500); err != nil {
		return c, err
	}
	if c.UploadMaxAttempts, err = getIntDefault("UPLOAD_MAX_ATTEMPTS", 3); err != nil {
		return c, err
	}
	if c.UploadBackoffStartSeconds, err = getIntDefault("UPLOAD_BACKOFF_START_SECONDS", 1); err != nil {
		return c, err
	}
	if c.SchedulePollInterval, err = getDurationSecondsDefault("SCHEDULE_POLL_INTERVAL_SECONDS", 60); err != nil {
		return c, err
	}
	if c.ScheduleLookahead, err = getDurationSecondsDefault("SCHEDULE_LOOKAHEAD_SECONDS", 172800); err != nil {
		return c, err
	}
	if c.ScheduleLookbehind, err = getDurationSecondsDefault("SCHEDULE_LOOKBEHIND_SECONDS", 3600); err != nil {
		return c, err
	}
	if c.SchedulerTick, err = getDurationSecondsDefault("SCHEDULER_TICK_SECONDS", 5); err != nil {
		return c, err
	}
	if c.InterlockWindow, err = getDurationSecondsDefault("INTERLOCK_WINDOW_SECONDS", 1800); err != nil {
		return c, err
	}
	if c.InterlockOverrideLead, err = getDurationSecondsDefault("INTERLOCK_OVERRIDE_LEAD_SECONDS", 300); err != nil {
		return c, err
	}
	if c.TUIRefresh, err = getDurationMillisDefault("TUI_REFRESH_MS", 500); err != nil {
		return c, err
	}
	c.ReportStatus = getBoolDefault("FAC_REPORT_STATUS", false)

	return c, nil
}

// RequireUploadConfig returns an error if the bits needed to run an upload are
// missing. Callers that only touch recording/queue state don't need them, so
// we don't enforce in Load().
func (c Config) RequireUploadConfig() error {
	if c.FACAPIURL == "" {
		return errors.New("FAC_API_URL not set")
	}
	if c.FACAPIKey == "" {
		return errors.New("FAC_API_KEY not set")
	}
	return nil
}

// RequireRecordConfig returns an error if the bits needed to drive record.sh
// are missing.
func (c Config) RequireRecordConfig() error {
	if c.FFmpegInputArgs == "" {
		return errors.New("FFMPEG_INPUT_ARGS not set")
	}
	return nil
}

func getenvDefault(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func getIntDefault(key string, fallback int) (int, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}

func getDurationSecondsDefault(key string, fallbackSeconds int) (time.Duration, error) {
	n, err := getIntDefault(key, fallbackSeconds)
	if err != nil {
		return 0, err
	}
	return time.Duration(n) * time.Second, nil
}

func getDurationMillisDefault(key string, fallbackMillis int) (time.Duration, error) {
	n, err := getIntDefault(key, fallbackMillis)
	if err != nil {
		return 0, err
	}
	return time.Duration(n) * time.Millisecond, nil
}

func getBoolDefault(key string, fallback bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return fallback
	}
	return b
}
