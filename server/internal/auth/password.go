package auth

import (
	"errors"
	"regexp"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

var usernamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{2,31}$`)

func NormalizeUsername(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if !usernamePattern.MatchString(value) {
		return "", errors.New("username must be 3–32 characters: letters, digits, dots, underscores or hyphens; start with a letter or digit")
	}
	return value, nil
}

func HashPassword(password string) (string, error) {
	if len(password) < 12 || len(password) > 72 {
		return "", errors.New("password must be 12–72 bytes")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), 12)
	return string(hash), err
}

// A missing account takes the same expensive comparison path as a wrong password.
var missingPasswordHash = func() string {
	hash, err := bcrypt.GenerateFromPassword([]byte("unusable-account-placeholder"), 12)
	if err != nil {
		panic(err)
	}
	return string(hash)
}()

func CheckPassword(hash, password string) bool {
	missing := hash == ""
	if missing {
		hash = missingPasswordHash
	}
	err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
	return err == nil && !missing
}
