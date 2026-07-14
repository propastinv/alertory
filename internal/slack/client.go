// Package slack is a small client for the two Slack Web API calls this
// service needs: posting a new message and updating an existing one in
// place. Kept separate from the workflows package so message rendering,
// grouping logic, and the actual HTTP call are independently testable.
package slack

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type Field struct {
	Title string `json:"title"`
	Value string `json:"value"`
	Short bool   `json:"short,omitempty"`
}

type Attachment struct {
	Color  string  `json:"color,omitempty"`
	Title  string  `json:"title,omitempty"`
	Fields []Field `json:"fields,omitempty"`
}

type PostResult struct {
	Channel string
	TS      string
}

// httpClient has an explicit timeout - the original code used
// &http.Client{} with no timeout at all for Slack calls, so a hung
// request could tie up a goroutine (and hold the group's claim lease)
// indefinitely under load.
var httpClient = &http.Client{Timeout: 10 * time.Second}

// Post sends a brand new message and returns its channel/ts so it can be
// edited later.
func Post(token, channel, text string, attachments []Attachment) (*PostResult, error) {
	return send(token, "https://slack.com/api/chat.postMessage", channel, "", text, attachments)
}

// Update rewrites an existing message in place. Callers should always pass
// the full desired content - Slack's chat.update replaces the message
// wholesale, it doesn't merge.
func Update(token, channel, ts, text string, attachments []Attachment) error {
	_, err := send(token, "https://slack.com/api/chat.update", channel, ts, text, attachments)
	return err
}

func send(token, url, channel, ts, text string, attachments []Attachment) (*PostResult, error) {
	if token == "" {
		return nil, fmt.Errorf("no Slack token configured")
	}
	if text == "" && len(attachments) > 0 {
		text = " " // Slack rejects a fully empty message
	}

	payload := map[string]any{
		"channel":     channel,
		"text":        text,
		"attachments": attachments,
	}
	if ts != "" {
		payload["ts"] = ts
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest("POST", url, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result struct {
		OK      bool   `json:"ok"`
		Error   string `json:"error"`
		Channel string `json:"channel"`
		TS      string `json:"ts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	if !result.OK {
		return nil, fmt.Errorf("slack API error: %s", result.Error)
	}

	return &PostResult{Channel: result.Channel, TS: result.TS}, nil
}
