package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The published archive is also the checkpoint. A failed scan cannot advance it.
// Threads use their Discord channel ID here, independently of their nested disk path.
type snapshotCache struct {
	messages map[string]map[string]archivedMessage
	users    map[string]user
	cutoff   time.Time
}

func loadSnapshotCache(repo, guild string, cutoff time.Time) (*snapshotCache, error) {
	c := &snapshotCache{messages: map[string]map[string]archivedMessage{}, users: map[string]user{}, cutoff: cutoff}
	b, err := os.ReadFile(filepath.Join(repo, "data.json"))
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	var header struct {
		SchemaVersion int    `json:"schema_version"`
		GuildID       string `json:"guild_id"`
	}
	if err := json.Unmarshal(b, &header); err != nil {
		return nil, err
	}
	if header.SchemaVersion != 1 || header.GuildID != guild {
		return nil, errors.New("archive schema or guild does not match config")
	}
	err = filepath.WalkDir(filepath.Join(repo, "channels"), func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || entry.Name() != "data.json" {
			return nil
		}
		rel, err := filepath.Rel(filepath.Join(repo, "channels"), path)
		if err != nil {
			return err
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if len(parts) == 2 {
			var ch channel
			if err := json.Unmarshal(b, &ch); err != nil {
				return err
			}
			if ch.ID != parts[0] || !idPattern.MatchString(ch.ID) {
				return fmt.Errorf("invalid cached channel %s", rel)
			}
			if c.messages[ch.ID] == nil {
				c.messages[ch.ID] = map[string]archivedMessage{}
			}
			return nil
		}
		if (len(parts) != 4 && len(parts) != 5) || parts[1] != "messages" {
			return fmt.Errorf("unexpected archive path %s", rel)
		}
		var m archivedMessage
		if err := json.Unmarshal(b, &m); err != nil {
			return err
		}
		if !idPattern.MatchString(m.ID) || !idPattern.MatchString(m.ChannelID) || m.ID != parts[len(parts)-2] {
			return fmt.Errorf("invalid cached message %s", rel)
		}
		if m.Thread != nil {
			if m.Thread.ID != m.ID {
				return fmt.Errorf("invalid cached thread %s", rel)
			}
			if c.messages[m.Thread.ID] == nil {
				c.messages[m.Thread.ID] = map[string]archivedMessage{}
			}
		}
		if m.Missing {
			return nil
		}
		if m.Timestamp.IsZero() || !idPattern.MatchString(m.AuthorID) {
			return fmt.Errorf("incomplete cached message %s", rel)
		}
		if c.messages[m.ChannelID] == nil {
			c.messages[m.ChannelID] = map[string]archivedMessage{}
		}
		m.Thread = nil // Current thread discovery supplies metadata, including deletions.
		c.messages[m.ChannelID][m.ID] = m
		return nil
	})
	if err != nil {
		return nil, err
	}
	err = filepath.WalkDir(filepath.Join(repo, "users"), func(path string, entry fs.DirEntry, walkErr error) error {
		if errors.Is(walkErr, os.ErrNotExist) {
			return nil
		}
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || entry.Name() != "data.json" {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var u user
		if err := json.Unmarshal(b, &u); err != nil {
			return err
		}
		return rememberUser(c.users, u)
	})
	return c, err
}

func snowflakeAt(t time.Time) string {
	const discordEpoch = int64(1420070400000)
	ms := t.UnixMilli() - discordEpoch
	if ms <= 0 {
		return "0"
	}
	return strconv.FormatUint(uint64(ms)<<22, 10)
}

func (c *snapshotCache) lowerBound(id string) string {
	if c == nil {
		return ""
	}
	latest := ""
	for mid := range c.messages[id] {
		if latest == "" || idLess(latest, mid) {
			latest = mid
		}
	}
	// No checkpoint means full backfill, including streams that were previously empty.
	if latest == "" {
		return ""
	}
	cutoff := snowflakeAt(c.cutoff)
	if !idLess(latest, cutoff) {
		return cutoff
	}
	// An outage longer than the overlap must still fetch EVERY new message.
	// Outside the overlap, keep the cursor message without refetching its reactors.
	n, err := strconv.ParseUint(latest, 10, 64)
	if err != nil || n == ^uint64(0) {
		return latest
	}
	return strconv.FormatUint(n+1, 10)
}

func refreshMessages(ctx context.Context, api *discordClient, id string, users map[string]user, cache *snapshotCache) ([]archivedMessage, error) {
	lower := cache.lowerBound(id)
	fresh, err := api.messagesFrom(ctx, id, lower)
	if err != nil {
		return nil, err
	}
	merged := map[string]archivedMessage{}
	if cache != nil {
		for mid, m := range cache.messages[id] {
			// Replace the entire fetched range, so missing messages become deletions.
			if lower != "" && idLess(mid, lower) {
				merged[mid] = m
			}
		}
	}
	for i, m := range fresh {
		a, err := archiveMessage(ctx, api, m, users)
		if err != nil {
			return nil, err
		}
		merged[m.ID] = a
		if (i+1)%100 == 0 {
			logMessageProgress(id, i+1, len(fresh))
		}
	}
	result := make([]archivedMessage, 0, len(merged))
	for _, m := range merged {
		result = append(result, m)
	}
	sort.Slice(result, func(i, j int) bool { return idLess(result[i].ID, result[j].ID) })
	return result, nil
}

// Write only user records referenced by this snapshot, even when cache users
// came from channels that have since been removed from configuration.
func writeSnapshotUsers(stage string, users map[string]user) error {
	needed := map[string]bool{}
	err := filepath.WalkDir(filepath.Join(stage, "channels"), func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || entry.Name() != "data.json" || !strings.Contains(path, string(filepath.Separator)+"messages"+string(filepath.Separator)) {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var m archivedMessage
		if err := json.Unmarshal(b, &m); err != nil {
			return err
		}
		if m.AuthorID != "" {
			needed[m.AuthorID] = true
		}
		for _, id := range m.MentionIDs {
			needed[id] = true
		}
		for _, r := range m.Reactions {
			for _, id := range r.NormalUserIDs {
				needed[id] = true
			}
			for _, id := range r.BurstUserIDs {
				needed[id] = true
			}
		}
		if m.Thread != nil && m.Thread.OwnerID != "" {
			needed[m.Thread.OwnerID] = true
		}
		return nil
	})
	if err != nil {
		return err
	}
	for id := range needed {
		u, ok := users[id]
		if !ok {
			return fmt.Errorf("missing cached user %s", id)
		}
		if err := writeJSON(filepath.Join(stage, "users", id, "data.json"), u); err != nil {
			return err
		}
	}
	return nil
}
