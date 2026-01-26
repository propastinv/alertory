package workflows

import (
	"bytes"
	"log"
	"text/template"
	"io/ioutil"
	"path/filepath"
	"fmt"
	"encoding/json"
	"net/http"
	"context"
	"time"

	"gopkg.in/yaml.v3"
	"github.com/propastinv/alertory/internal/db"
	"github.com/jackc/pgx/v5/pgxpool"
)

type MatchRule struct {
	Labels map[string]string `yaml:"labels"`
}

type Action struct {
	Type    string `yaml:"type"`
	Channel string `yaml:"channel,omitempty"`
	Message string `yaml:"message,omitempty"`
}

type WorkflowRule struct {
	Name    string     `yaml:"name"`
	Match   MatchRule  `yaml:"match"`
	Actions []Action   `yaml:"actions"`
}

type SlackMessage struct {
	Channel string `json:"channel"`
	Text    string `json:"text"`
}

type SlackPostResult struct {
	Channel string `json:"channel"`
	TS      string `json:"ts"`
}



func (r WorkflowRule) Matches(alert db.AlertUpsert) bool {
	for k, v := range r.Match.Labels {
		if val, ok := alert.Labels[k]; !ok || val != v {
			return false
		}
	}
	return true
}

func renderTemplate(tmpl string, alert db.AlertUpsert) (string, error) {
	t, err := template.New("msg").Parse(tmpl)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, alert); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func ProcessAlert(ctx context.Context, alert db.AlertUpsert, rules []WorkflowRule, pool *pgxpool.Pool) db.AlertUpsert {
	for _, rule := range rules {
		if !rule.Matches(alert) {
			continue
		}

		for _, action := range rule.Actions {
			switch action.Type {
			case "slack":
				if alert.Meta == nil {
					alert.Meta = make(map[string]any)
				}

				workflowsMeta, ok := alert.Meta["workflows"].(map[string]any)
				if !ok || workflowsMeta == nil {
					workflowsMeta = make(map[string]any)
					alert.Meta["workflows"] = workflowsMeta
				}

				workflowMeta, ok := workflowsMeta[rule.Name].(map[string]any)
				if !ok || workflowMeta == nil {
					workflowMeta = make(map[string]any)
					workflowsMeta[rule.Name] = workflowMeta
				}

				slackMeta, exists := workflowMeta["slack"].(map[string]any)

				switch alert.Status {
				case "firing":
					send := true
					if exists {
						if sa, ok := slackMeta["startsAt"].(string); ok && sa == alert.StartsAt.Format(time.RFC3339) {
							send = false
						}
					}

					if !send {
						continue
					}

					msg, err := renderTemplate(action.Message, alert)
					if err != nil {
						log.Printf("template error: %v", err)
						continue
					}

					slackPayload, err := sendSlackMessage(pool, action.Channel, msg)
					if err != nil {
						log.Printf("failed to send Slack message: %v", err)
						continue
					}

					workflowMeta["slack"] = map[string]any{
						"channel":  slackPayload.Channel,
						"ts":       slackPayload.TS,
						"startsAt": alert.StartsAt.Format(time.RFC3339),
					}
				case "resolved":
					if exists {
						channel, _ := slackMeta["channel"].(string)
						ts, _ := slackMeta["ts"].(string)

						resolvedMsg := fmt.Sprintf("*RESOLVED:* alert %s is back to normal", alert.Alertname)
						if err := updateSlackMessage(pool, channel, ts, resolvedMsg); err != nil {
							log.Printf("failed to update Slack message: %v", err)
						} else {
							if slackMap, ok := workflowMeta["slack"].(map[string]any); ok {
								slackMap["resolved"] = true
								workflowMeta["slack"] = slackMap
							}
						}
					}

				}
			}
		}
	}

	return alert
}



func sendSlackMessage(pool *pgxpool.Pool, channel, text string) (*SlackPostResult, error) {
	token := db.GetProviderSetting(pool, "slack", "access_token")
	if token == "" {
		return nil, fmt.Errorf("no Slack token found in DB")
	}

	msg := SlackMessage{
		Channel: channel,
		Text:    text,
	}
	payload, _ := json.Marshal(msg)

	req, err := http.NewRequest("POST", "https://slack.com/api/chat.postMessage", bytes.NewBuffer(payload))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	client := &http.Client{}
	resp, err := client.Do(req)
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
		return nil, fmt.Errorf("Slack API error: %s", result.Error)
	}

	return &SlackPostResult{
		Channel: result.Channel,
		TS:      result.TS,
	}, nil
}

func updateSlackMessage(pool *pgxpool.Pool, channel, ts, text string) error {
	token := db.GetProviderSetting(pool, "slack", "access_token")
	if token == "" {
		return fmt.Errorf("no Slack token found in DB")
	}

	payload := map[string]string{
		"channel": channel,
		"ts":      ts,
		"text":    text,
	}
	data, _ := json.Marshal(payload)

	req, err := http.NewRequest("POST", "https://slack.com/api/chat.update", bytes.NewBuffer(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var result struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return err
	}

	if !result.OK {
		return fmt.Errorf("Slack API error: %s", result.Error)
	}

	return nil
}



func LoadWorkflowRules(dir string) ([]WorkflowRule, error) {
	var rules []WorkflowRule
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, err
	}

	for _, f := range files {
		data, err := ioutil.ReadFile(f)
		if err != nil {
			return nil, err
		}

		var r WorkflowRule
		if err := yaml.Unmarshal(data, &r); err != nil {
			return nil, err
		}

		rules = append(rules, r)
	}

	return rules, nil
}