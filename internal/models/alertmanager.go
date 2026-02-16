package models

import "time"

type WebhookPayload struct {
	Version           string            `json:"version,omitempty"`
	GroupKey          string            `json:"groupKey,omitempty"`
	Status            string            `json:"status"`
	Receiver          string            `json:"receiver,omitempty"`
	GroupLabels       map[string]string `json:"groupLabels,omitempty"`
	CommonLabels      map[string]string `json:"commonLabels,omitempty"`
	CommonAnnotations map[string]string `json:"commonAnnotations,omitempty"`
	ExternalURL       string            `json:"externalURL,omitempty"`

	Alerts []Alert `json:"alerts"`
}

type Alert struct {
	Status      string     `json:"status"`
	Fingerprint string     `json:"fingerprint"`
	StartsAt    *time.Time `json:"startsAt"`
	EndsAt      *time.Time `json:"endsAt"`

	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`

	GeneratorURL string `json:"generatorURL,omitempty"` // необязательное поле
}
