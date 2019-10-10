package event

import "fmt"

// StorageEvent is the payload of a GCS event.
type StorageEvent struct {
	Bucket string `json:"bucket"`
	Name   string `json:"name"`
}

//URL returns event source URL
func (e StorageEvent) URL() string {
	return fmt.Sprintf("gs://%v/%v", e.Bucket, e.Name)
}