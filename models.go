package main

import (
	"encoding/json"
	"time"
)

// Keep Discord snowflakes as strings so JSON readers cannot lose precision.
type user struct {
	ID         string  `json:"id"`
	Username   string  `json:"username"`
	GlobalName string  `json:"global_name"`
	Avatar     *string `json:"avatar"`
	Bot        bool    `json:"bot"`
}
type overwrite struct {
	ID    string `json:"id"`
	Type  int    `json:"type"`
	Allow string `json:"allow"`
	Deny  string `json:"deny"`
}
type threadMetadata struct {
	Archived            bool   `json:"archived"`
	ArchiveTimestamp    string `json:"archive_timestamp"`
	AutoArchiveDuration int    `json:"auto_archive_duration"`
	Locked              bool   `json:"locked"`
	Invitable           bool   `json:"invitable,omitempty"`
	CreateTimestamp     string `json:"create_timestamp,omitempty"`
}
type channel struct {
	ID             string          `json:"id"`
	GuildID        string          `json:"guild_id"`
	ParentID       string          `json:"parent_id"`
	Name           string          `json:"name"`
	Topic          string          `json:"topic,omitempty"`
	Type           int             `json:"type"`
	OwnerID        string          `json:"owner_id,omitempty"`
	NSFW           bool            `json:"nsfw"`
	Overwrites     []overwrite     `json:"permission_overwrites,omitempty"`
	ThreadMetadata *threadMetadata `json:"thread_metadata,omitempty"`
}
type emoji struct {
	ID       string `json:"id,omitempty"`
	Name     string `json:"name"`
	Animated bool   `json:"animated,omitempty"`
}
type reaction struct {
	Emoji        emoji `json:"emoji"`
	Count        int   `json:"count"`
	CountDetails struct {
		Normal int `json:"normal"`
		Burst  int `json:"burst"`
	} `json:"count_details"`
	NormalUserIDs []string `json:"normal_user_ids"`
	BurstUserIDs  []string `json:"burst_user_ids"`
}

// These fields have the same representation in the API and on disk.
type messageBody struct {
	ID              string            `json:"id"`
	ChannelID       string            `json:"channel_id"`
	Content         string            `json:"content"`
	Type            int               `json:"type"`
	Timestamp       time.Time         `json:"timestamp"`
	EditedTimestamp *time.Time        `json:"edited_timestamp"`
	Pinned          bool              `json:"pinned"`
	Flags           int               `json:"flags"`
	WebhookID       string            `json:"webhook_id,omitempty"`
	MentionEveryone bool              `json:"mention_everyone"`
	MentionRoles    []string          `json:"mention_roles"`
	Attachments     []json.RawMessage `json:"attachments"`
	Embeds          []json.RawMessage `json:"embeds"`
	Reactions       []reaction        `json:"reactions"`
	Reference       *messageReference `json:"message_reference,omitempty"`
}
type messageReference struct {
	MessageID string `json:"message_id,omitempty"`
	ChannelID string `json:"channel_id,omitempty"`
	GuildID   string `json:"guild_id,omitempty"`
	Type      int    `json:"type"`
}

// API messages embed user objects; archive messages refer to the users directory.
type message struct {
	messageBody
	Author   user   `json:"author"`
	Mentions []user `json:"mentions"`
}
type archivedMessage struct {
	messageBody
	AuthorID   string   `json:"author_id,omitempty"`
	MentionIDs []string `json:"mention_ids"`
	Thread     *channel `json:"thread,omitempty"`
	// Threads created without a message, or with a deleted starter, need an anchor.
	Missing bool `json:"missing,omitempty"`
}
