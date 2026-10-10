package issues

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	ErrNotFound = errors.New("issue not found")
	ErrConflict = errors.New("issue changed; reload before trying again")
	idPattern   = regexp.MustCompile(`^[a-f0-9]{32}$`)
)

type ValidationError struct {
	Message string
}

func (e *ValidationError) Error() string { return e.Message }

type Issue struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	Revision  string    `json:"revision,omitempty"`
}

type Input struct {
	ID       string `json:"id,omitempty"`
	Title    string `json:"title"`
	Body     string `json:"body"`
	Status   string `json:"status"`
	Revision string `json:"revision,omitempty"`
}

func NewID() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", fmt.Errorf("generate issue ID: %w", err)
	}
	return hex.EncodeToString(bytes[:]), nil
}

func ValidateID(id string) error {
	if !idPattern.MatchString(id) {
		return &ValidationError{"issue ID must be 32 lowercase hexadecimal characters"}
	}
	return nil
}

func validateInput(input Input) error {
	if strings.TrimSpace(input.Title) == "" || utf8.RuneCountInString(input.Title) > 200 {
		return &ValidationError{"title must contain between 1 and 200 characters"}
	}
	if !utf8.ValidString(input.Title) || !utf8.ValidString(input.Body) {
		return &ValidationError{"issue text must be valid UTF-8"}
	}
	if len(input.Body) > 1024*1024 {
		return &ValidationError{"body must not exceed 1 MiB"}
	}
	if input.Status != "open" && input.Status != "closed" {
		return &ValidationError{"status must be open or closed"}
	}
	return nil
}
