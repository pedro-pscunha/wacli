package main

import "github.com/openclaw/wacli/internal/wa"

// validateNewMessageID checks the --id (or --new-id) value of a send. An empty
// value keeps the default random message ID.
func validateNewMessageID(id string) error {
	if id == "" {
		return nil
	}
	return wa.ValidateMessageID(id)
}
