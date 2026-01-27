package workflows

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"text/template"
	"time"

	"gopkg.in/yaml.v3"
	"io/ioutil"
	"path/filepath"

	"github.com/propastinv/alertory/internal/db"
	"github.com/jackc/pgx/v5/pgxpool"
)

type MatchRule struct {
	Labels map[string]string `yaml:"labels"`
}

type HTTPEnrichmentParam struct {
	Name  string `yaml:"name"`
	Value string `yaml:"value"`
}

type HTTPEnrichment struct {
	Name          string                  `yaml:"name"`
	URL           string                  `yaml:"url"`
	Method        string                  `yaml:"method,omitempty"`
	Params        []HTTPEnrichmentParam   `yaml:"params,omitempty"`
	ResponseField string                  `yaml:"response_field,omitempty"`
	StoreIn       string                  `yaml:"store_in,omitempty"`
}

type SlackAttachmentField struct {
	Title string `yaml:"title" json:"title"`
	Value string `yaml:"value" json:"value"`
	Short bool   `yaml:"short,omitempty" json:"short,omitempty"`
}

type SlackAttachment struct {
	Color     string                 `yaml:"color,omitempty" json:"color,omitempty"`
	Title     string                 `yaml:"title,omitempty" json:"title,omitempty"`
	TitleLink string                 `yaml:"title_link,omitempty" json:"title_link,omitempty"`
	Fields    []SlackAttachmentField `yaml:"fields,omitempty" json:"fields,omitempty"`
}

type Action struct {
	Type                string            `yaml:"type"`
	Channel             string            `yaml:"channel,omitempty"`
	Message             string            `yaml:"message,omitempty"`
	Attachments         []SlackAttachment `yaml:"attachments,omitempty"`
	ResolveMessage      string            `yaml:"resolve_message,omitempty"`
	ResolveAttachments  []SlackAttachment `yaml:"resolve_attachments,omitempty"`
}

type WorkflowRule struct {
	Name        string              `yaml:"name"`
	Match       MatchRule           `yaml:"match"`
	Enrichments []HTTPEnrichment    `yaml:"enrichments,omitempty"`
	Actions     []Action            `yaml:"actions"`
}

type SlackMessage struct {
	Channel     string            `json:"channel"`
	Text        string            `json:"text,omitempty"`
	Attachments []SlackAttachment `json:"attachments,omitempty"`
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


func enrichAlert(alert db.AlertUpsert, enrichments []HTTPEnrichment) (db.AlertUpsert, error) {
	for _, enrichment := range enrichments {
		renderedURL, err := renderTemplate(enrichment.URL, alert)
		if err != nil {
			log.Printf("failed to render enrichment URL: %v", err)
			continue
		}

		params := make(map[string]string)
		for _, p := range enrichment.Params {
			renderedValue, err := renderTemplate(p.Value, alert)
			if err != nil {
				log.Printf("failed to render enrichment param %s: %v", p.Name, err)
				continue
			}
			params[p.Name] = renderedValue
		}

		method := enrichment.Method
		if method == "" {
			method = "GET"
		}

		if method == "GET" && len(params) > 0 {
			query := ""
			for k, v := range params {
				if query != "" {
					query += "&"
				}
				query += k + "=" + v
			}
			if query != "" {
				renderedURL += "?" + query
			}
		}

		req, _ := http.NewRequest(method, renderedURL, nil)
		req.Header.Set("Content-Type", "application/json")

		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			log.Printf("failed to make enrichment request to %s: %v", renderedURL, err)
			continue
		}
		defer resp.Body.Close()

		var result map[string]interface{}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			log.Printf("failed to decode enrichment response: %v", err)
			continue
		}

		storeIn := enrichment.StoreIn
		if storeIn == "" {
			storeIn = enrichment.Name
		}

		var value interface{} = result
		if enrichment.ResponseField != "" {
			value = result[enrichment.ResponseField]
		}

		if alert.Annotations == nil {
			alert.Annotations = make(map[string]string)
		}
		alert.Annotations[storeIn] = fmt.Sprintf("%v", value)
	}

	return alert, nil
}

func renderTemplate(tmpl string, alert db.AlertUpsert) (string, error) {
	t, err := template.New("tmpl").Parse(tmpl)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, alert); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func renderAttachment(a SlackAttachment, alert db.AlertUpsert) SlackAttachment {
	ra := a
	if a.Title != "" {
		t, _ := template.New("title").Parse(a.Title)
		var buf bytes.Buffer
		_ = t.Execute(&buf, alert)
		ra.Title = buf.String()
	}
	if a.TitleLink != "" {
		t, _ := template.New("link").Parse(a.TitleLink)
		var buf bytes.Buffer
		_ = t.Execute(&buf, alert)
		ra.TitleLink = buf.String()
	}
	ra.Fields = make([]SlackAttachmentField, len(a.Fields))
	for i, f := range a.Fields {
		t, _ := template.New("field").Parse(f.Value)
		var buf bytes.Buffer
		_ = t.Execute(&buf, alert)
		ra.Fields[i] = SlackAttachmentField{
			Title: f.Title,
			Value: buf.String(),
			Short: f.Short,
		}
	}
	return ra
}

func ProcessAlert(ctx context.Context, alert db.AlertUpsert, rules []WorkflowRule, pool *pgxpool.Pool, isNew bool) db.AlertUpsert {
	for _, rule := range rules {
		if !rule.Matches(alert) {
			continue
		}

		if len(rule.Enrichments) > 0 && isNew {
			enrichedAlert, err := enrichAlert(alert, rule.Enrichments)
			if err != nil {
				log.Printf("enrichment error: %v", err)
			} else {
				alert = enrichedAlert
			}
		}

		for _, action := range rule.Actions {
			if action.Type != "slack" {
				continue
			}

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

			slackMeta, _ := workflowMeta["slack"].(map[string]any)

			switch alert.Status {
			case "firing":
				send := true
				if slackMeta != nil {
					if sa, ok := slackMeta["startsAt"].(string); ok && sa == alert.StartsAt.Format(time.RFC3339) {
						send = false
					}
				}
				if !send {
					continue
				}

				var attachments []SlackAttachment
				if len(action.Attachments) > 0 {
					for _, a := range action.Attachments {
						attachments = append(attachments, renderAttachment(a, alert))
					}
				}

				var text string
			if action.Message != "" {
				t, err := renderTemplate(action.Message, alert)
				if err != nil {
					log.Printf("template error: %v", err)
					continue
				}
				text = t
			}

			slackPayload, err := sendSlackMessage(pool, action.Channel, text, attachments)
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
				if slackMeta != nil {
					channel, _ := slackMeta["channel"].(string)
					ts, _ := slackMeta["ts"].(string)

					dbAlert, err := db.GetAlert(ctx, pool, alert.Fingerprint)
					if err != nil {
						log.Printf("failed to load alert from DB: %v", err)
						dbAlert = &alert
					}
					if dbAlert == nil {
						dbAlert = &alert
					}

					dbAlert.EndsAt = alert.EndsAt
					dbAlert.StartsAt = alert.StartsAt

					var resolveAttachments []SlackAttachment
					if len(action.ResolveAttachments) > 0 {
						for _, a := range action.ResolveAttachments {
							resolveAttachments = append(resolveAttachments, renderAttachment(a, *dbAlert))
						}
					}

					var resolvedMsg string
					if action.ResolveMessage != "" {
						t, err := renderTemplate(action.ResolveMessage, *dbAlert)
						if err != nil {
							log.Printf("template error: %v", err)
							continue
						}
						resolvedMsg = t
					} else if len(resolveAttachments) == 0 {
						resolvedMsg = fmt.Sprintf("*Resolved:* alert %s is back to normal", alert.Alertname)
					}

					if err := updateSlackMessageWithAttachments(pool, channel, ts, resolvedMsg, resolveAttachments); err != nil {
						log.Printf("failed to update Slack message on resolve: %v", err)
					} else {
						slackMeta["resolved"] = true
						workflowMeta["slack"] = slackMeta
					}
				}
			}
		}
	}

	return alert
}

func sendSlackMessage(pool *pgxpool.Pool, channel, text string, attachments []SlackAttachment) (*SlackPostResult, error) {
	token := db.GetProviderSetting(pool, "slack", "access_token")
	if token == "" {
		return nil, fmt.Errorf("no Slack token found in DB")
	}

	msg := SlackMessage{
		Channel:     channel,
		Text:        text,
		Attachments: attachments,
	}

	if text == "" && len(attachments) > 0 {
		text = " "
	}
	msg.Text = text
	payload, _ := json.Marshal(msg)

	req, _ := http.NewRequest("POST", "https://slack.com/api/chat.postMessage", bytes.NewBuffer(payload))
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

	req, _ := http.NewRequest("POST", "https://slack.com/api/chat.update", bytes.NewBuffer(data))
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

func updateSlackMessageWithAttachments(pool *pgxpool.Pool, channel, ts, text string, attachments []SlackAttachment) error {
	token := db.GetProviderSetting(pool, "slack", "access_token")
	if token == "" {
		return fmt.Errorf("no Slack token found in DB")
	}

	msg := SlackMessage{
		Channel:     channel,
		Text:        text,
		Attachments: attachments,
	}

	if text == "" && len(attachments) > 0 {
		text = " "
		msg.Text = text
	}

	payload := map[string]interface{}{
		"channel":     channel,
		"ts":          ts,
		"text":        msg.Text,
		"attachments": msg.Attachments,
	}
	data, _ := json.Marshal(payload)

	req, _ := http.NewRequest("POST", "https://slack.com/api/chat.update", bytes.NewBuffer(data))
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
