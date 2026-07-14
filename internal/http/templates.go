package http

import (
	"embed"
	"html/template"
)

//go:embed templates/*.html
var templatesFS embed.FS

// pageTemplates holds one *template.Template per page, each built from
// base.html + the page's own file. They're parsed separately (rather than
// all together) because every page file defines a "content" block under
// the same name - parsing them into one shared template set would let the
// last-parsed page silently win for every route.
type pageTemplates struct {
	dashboard *template.Template
	rulesList *template.Template
	ruleForm  *template.Template
	settings  *template.Template
}

func loadTemplates() (*pageTemplates, error) {
	build := func(page string) (*template.Template, error) {
		return template.ParseFS(templatesFS, "templates/base.html", "templates/"+page)
	}

	dashboard, err := build("dashboard.html")
	if err != nil {
		return nil, err
	}
	rulesList, err := build("rules_list.html")
	if err != nil {
		return nil, err
	}
	ruleForm, err := build("rule_form.html")
	if err != nil {
		return nil, err
	}
	settings, err := build("settings.html")
	if err != nil {
		return nil, err
	}

	return &pageTemplates{
		dashboard: dashboard,
		rulesList: rulesList,
		ruleForm:  ruleForm,
		settings:  settings,
	}, nil
}
