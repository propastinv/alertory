package models

import "time"

type WebhookPayload struct {
	Status string  `json:"status"`
	Alerts []Alert `json:"alerts"`
}

type Alert struct {
	Status       string            `json:"status"`
	Fingerprint  string            `json:"fingerprint"`
	StartsAt     time.Time         `json:"startsAt"`
	EndsAt       *time.Time        `json:"endsAt"`

	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
}
