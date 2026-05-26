package audit

import "time"

type Status string

const (
	StatusSuccess Status = "success"
	StatusDenied  Status = "denied"
	StatusFailure Status = "failure"
)

type Record struct {
	At          time.Time
	ActorUserID int64
	Action      string
	Target      string
	Status      Status
	MessageID   int64
	OldValue    string
	NewValue    string
	Error       string
}

func (record Record) WithTime(now time.Time) Record {
	if record.At.IsZero() {
		record.At = now
	}
	return record
}
