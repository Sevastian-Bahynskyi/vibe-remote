package model

import "time"

type ActivityItem struct {
	Name      string    `json:"name"`
	StartedAt time.Time `json:"startedAt"`
}

type WorkerActivity struct {
	Tools         []ActivityItem `json:"tools,omitempty"`
	Confirmations []ActivityItem `json:"confirmations,omitempty"`
}
