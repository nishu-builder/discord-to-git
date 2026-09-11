// Command discord-to-git exports Discord messages as Markdown and pushes Git snapshots.
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
	ID     string `json:"id"`
	Folder string `json:"folder"`
}

type config struct {
	GuildID  string          `json:"guild_id"`
	Folder   string          `json:"folder"`
	Channels []channelConfig `json:"channels"`
	Branch   string          `json:"branch"`
	Remote   string          `json:"remote"`
}

var idPattern = regexp.MustCompile("^[0-9]{1,20}$")
var folderPattern = regexp.MustCompile("^[a-zA-Z0-9][a-zA-Z0-9_-]*$")

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
	if !idPattern.MatchString(c.GuildID) || !folderPattern.MatchString(c.Folder) || len(c.Channels) == 0 {
		return c, errors.New("config needs a guild ID, safe folder name, and at least one channel")
	}
	ids, folders := map[string]bool{}, map[string]bool{}
	for _, ch := range c.Channels {
		if !idPattern.MatchString(ch.ID) || !folderPattern.MatchString(ch.Folder) || ids[ch.ID] || folders[ch.Folder] {
			return c, errors.New("channel IDs and folder names must be valid and unique")
		}
		ids[ch.ID], folders[ch.Folder] = true, true
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

func execute() error {
	configPath := flag.String("config", "config.local.json", "JSON file listing the channels to archive")
	out := flag.String("out", "data/archive", "dedicated Git checkout for generated Markdown")
	interval := flag.Duration("interval", 5*time.Minute, "pause between complete snapshots")
	once := flag.Bool("once", false, "publish one snapshot and exit")
	tokenSocket := flag.String("token-socket", "", "receive the bot token once on a private Unix socket")
	flag.Parse()
	if *interval <= 0 {
		return errors.New("interval must be positive")
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

	for {
		start := time.Now()
		cycleCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
		rev, count, err := syncOnce(cycleCtx, api, c, repo)
		cancel()
		status := map[string]any{"checked_at": time.Now().UTC(), "duration_seconds": time.Since(start).Seconds()}
		if err != nil {
			status["error"] = strings.ReplaceAll(err.Error(), token, "[redacted]")
			log.Printf("snapshot failed: %s", status["error"])
		} else {
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
