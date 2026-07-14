package workflows

import (
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/propastinv/alertory/internal/db"
)

// The types below mirror only the shape of the old workflows/*.yaml files,
// used once to import them into the DB on first boot (see
// db.SeedWorkflowRulesFromYAML). The free-form message/attachment
// templates are intentionally dropped: the Slack layout is now fixed in
// code (see RenderBucketMessage). Only the structural bits - match labels,
// channel, a best-effort "target" label, and enrichments - are carried
// over; Team and GroupBy are new fields with no YAML equivalent, so they
// come through blank and are meant to be filled in from the web UI.
type legacyMatch struct {
	Labels map[string]string `yaml:"labels"`
}

type legacyEnrichmentParam struct {
	Name  string `yaml:"name"`
	Value string `yaml:"value"`
}

type legacyEnrichment struct {
	Name          string                  `yaml:"name"`
	URL           string                  `yaml:"url"`
	Method        string                  `yaml:"method,omitempty"`
	Params        []legacyEnrichmentParam `yaml:"params,omitempty"`
	ResponseField string                  `yaml:"response_field,omitempty"`
	StoreIn       string                  `yaml:"store_in,omitempty"`
}

type legacyField struct {
	Title string `yaml:"title"`
	Value string `yaml:"value"`
}

type legacyAttachment struct {
	Title  string        `yaml:"title,omitempty"`
	Fields []legacyField `yaml:"fields,omitempty"`
}

type legacyAction struct {
	Type        string             `yaml:"type"`
	Channel     string             `yaml:"channel,omitempty"`
	Attachments []legacyAttachment `yaml:"attachments,omitempty"`
}

type legacyRule struct {
	Name        string             `yaml:"name"`
	Match       legacyMatch        `yaml:"match"`
	Enrichments []legacyEnrichment `yaml:"enrichments,omitempty"`
	Actions     []legacyAction     `yaml:"actions"`
}

// LoadLegacyYAMLRules parses workflows/*.yaml for one-time import into the
// DB. Missing directory is not an error - it just means nothing to seed.
func LoadLegacyYAMLRules(dir string) ([]db.WorkflowRule, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, err
	}

	var out []db.WorkflowRule
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}

		var lr legacyRule
		if err := yaml.Unmarshal(data, &lr); err != nil {
			return nil, err
		}

		rule := db.WorkflowRule{
			Name:        lr.Name,
			MatchLabels: lr.Match.Labels,
			Enabled:     true,
		}

		for _, e := range lr.Enrichments {
			params := make([]db.EnrichmentParam, 0, len(e.Params))
			for _, p := range e.Params {
				params = append(params, db.EnrichmentParam{Name: p.Name, Value: p.Value})
			}
			rule.Enrichments = append(rule.Enrichments, db.Enrichment{
				Name:          e.Name,
				URL:           e.URL,
				Method:        e.Method,
				Params:        params,
				ResponseField: e.ResponseField,
				StoreIn:       e.StoreIn,
			})
		}

		for _, a := range lr.Actions {
			if a.Type != "slack" || a.Channel == "" {
				continue
			}
			rule.Channel = a.Channel
			for _, att := range a.Attachments {
				for _, f := range att.Fields {
					if strings.EqualFold(f.Title, "Target") {
						if label := labelFromTemplate(f.Value); label != "" {
							rule.TargetLabel = label
						}
					}
				}
			}
		}

		if rule.Channel == "" {
			continue // no Slack action in this file, nothing to import
		}
		out = append(out, rule)
	}

	return out, nil
}

// labelFromTemplate pulls NAME out of a "{{ .Labels.NAME }}"-shaped string,
// used only to migrate the old Target field convention.
func labelFromTemplate(tmpl string) string {
	const marker = ".Labels."
	idx := strings.Index(tmpl, marker)
	if idx < 0 {
		return ""
	}
	rest := tmpl[idx+len(marker):]
	end := 0
	for end < len(rest) {
		c := rest[end]
		isIdentChar := c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
		if !isIdentChar {
			break
		}
		end++
	}
	return rest[:end]
}
