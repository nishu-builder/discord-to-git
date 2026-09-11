// Command discord-to-git exports Discord messages as JSON and pushes Git snapshots.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

type channelConfig struct {
	ID string `json:"id"`
}

type config struct {
	GuildID  string          `json:"guild_id"`
	Channels []channelConfig `json:"channels"`
	Branch   string          `json:"branch"`
	Remote   string          `json:"remote"`
}

var idPattern = regexp.MustCompile("^[0-9]{1,20}$")

func loadConfig(path string) (config, error) {
	var c config
	f, err := os.Open(path)
	if err != nil {
		return c, err
	}
	defer f.Close()
	decoder := json.NewDecoder(f)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&c); err != nil {
		return c, err
	}
	if !idPattern.MatchString(c.GuildID) || len(c.Channels) == 0 {
		return c, errors.New("config needs a guild ID and at least one channel")
	}
	ids := map[string]bool{}
	for _, ch := range c.Channels {
		if !idPattern.MatchString(ch.ID) || ids[ch.ID] {
			return c, errors.New("channel IDs must be valid and unique")
		}
		ids[ch.ID] = true
	}
	if c.Branch == "" {
		c.Branch = "discord"
	}
	if strings.ContainsAny(c.Remote, "\r\n") || strings.HasPrefix(c.Remote, "-") {
		return c, errors.New("invalid Git remote")
	}
	return c, nil
}

// A cycle builds a complete new directory before touching the published archive.
// A failed API request leaves the previous Git snapshot unchanged.
func syncOnce(ctx context.Context, api *discordClient, c config, repo string) (string, int, error) {
	stage, err := os.MkdirTemp(filepath.Dir(repo), ".snapshot-")
	if err != nil {
		return "", 0, err
	}
	defer os.RemoveAll(stage)
	count, err := buildSnapshot(ctx, api, c, stage)
	if err != nil {
		return "", 0, err
	}
	rev, err := publish(ctx, c, repo, stage)
	return rev, count, err
}

type syncState struct {
	LastFull time.Time `json:"last_full_reconciliation"`
}

func loadSyncState(path string, now time.Time) (syncState, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return syncState{LastFull: now}, nil
	}
	if err != nil {
		return syncState{}, err
	}
	var state syncState
	if err := json.Unmarshal(b, &state); err != nil {
		return state, err
	}
	return state, nil
}

func syncIncrementalOnce(ctx context.Context, api *discordClient, c config, repo string, cutoff time.Time, full bool) (string, int, error) {
	var cache *snapshotCache
	published, err := restoreCheckpoint(ctx, c, repo)
	if err != nil {
		return "", 0, err
	}
	if !full && published {
		cache, err = loadSnapshotCache(repo, c.GuildID, cutoff)
		if err != nil {
			return "", 0, err
		}
	}
	stage, err := os.MkdirTemp(filepath.Dir(repo), ".snapshot-")
	if err != nil {
		return "", 0, err
	}
	defer os.RemoveAll(stage)
	count, err := buildSnapshotCached(ctx, api, c, stage, cache)
	if err != nil {
		return "", 0, err
	}
	rev, err := publish(ctx, c, repo, stage)
	return rev, count, err
}

func execute() error {
	configPath := flag.String("config", "config.local.json", "JSON file listing the channels to archive")
	out := flag.String("out", "data/archive", "dedicated Git checkout for generated JSON")
	interval := flag.Duration("interval", time.Minute, "pause between incremental snapshots")
	lookback := flag.Duration("lookback", 24*time.Hour, "recheck messages created within this window")
	fullInterval := flag.Duration("full-interval", 24*time.Hour, "interval between complete history reconciliations")
	syncTimeout := flag.Duration("sync-timeout", 2*time.Hour, "maximum duration of one scan, including initial backfill")
	forceFull := flag.Bool("full", false, "reread complete history on every scan")
	once := flag.Bool("once", false, "publish one snapshot and exit")
	tokenSocket := flag.String("token-socket", "", "receive the bot token once on a private Unix socket")
	flag.Parse()
	if *interval <= 0 || *lookback <= 0 || *fullInterval <= 0 || *syncTimeout <= 0 {
		return errors.New("sync durations must be positive")
	}
	c, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	repo, err := filepath.Abs(*out)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(repo), 0700); err != nil {
		return err
	}

	// The OS releases this lock even if the process crashes.
	lock, err := os.OpenFile(repo+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("another archiver owns %s: %w", repo, err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	token, err := readToken(ctx, *tokenSocket)
	if err != nil {
		return err
	}
	api := newDiscordClient(token)
	if err := api.checkBot(ctx); err != nil {
		return err
	}
	if err := initRepo(ctx, c, repo); err != nil {
		return err
	}

	statePath := repo + ".sync-state.json"
	state, err := loadSyncState(statePath, time.Now())
	if err != nil {
		return err
	}
	for {
		start := time.Now()
		full := *forceFull || start.Sub(state.LastFull) >= *fullInterval
		mode := "incremental"
		if full {
			mode = "full"
		}
		log.Printf("starting %s scan of %d channels", mode, len(c.Channels))
		cycleCtx, cancel := context.WithTimeout(ctx, *syncTimeout)
		rev, count, err := syncIncrementalOnce(cycleCtx, api, c, repo, start.Add(-*lookback), full)
		cancel()
		status := map[string]any{"checked_at": time.Now().UTC(), "duration_seconds": time.Since(start).Seconds(), "mode": mode, "interval_seconds": interval.Seconds()}
		if err != nil {
			status["error"] = strings.ReplaceAll(err.Error(), token, "[redacted]")
			log.Printf("snapshot failed: %s", status["error"])
		} else {
			if full {
				state.LastFull = time.Now().UTC()
			}
			if err := writeJSON(statePath+".tmp", state); err != nil {
				return err
			}
			if err := os.Rename(statePath+".tmp", statePath); err != nil {
				return err
			}
			status["next_full_reconciliation"] = state.LastFull.Add(*fullInterval)
			status["commit"], status["messages"] = rev, count
			log.Printf("published %d messages at %s in %s", count, rev, time.Since(start).Round(time.Millisecond))
		}
		data, marshalErr := json.MarshalIndent(status, "", "  ")
		if marshalErr != nil {
			return marshalErr
		}
		if writeErr := os.WriteFile(repo+".status.json.tmp", append(data, '\n'), 0600); writeErr != nil {
			return writeErr
		}
		if renameErr := os.Rename(repo+".status.json.tmp", repo+".status.json"); renameErr != nil {
			return renameErr
		}
		if *once {
			return err
		}
		if err := wait(ctx, *interval); err != nil {
			return nil
		}
	}
}

func main() {
	syscall.Umask(0077)
	if err := execute(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
