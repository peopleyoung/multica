// Package nativeruntime defines the credential-free native execution settings.
package nativeruntime

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"regexp"
	"strings"
)

type Config struct {
	Environment string    `json:"environment,omitempty"`
	WriteFiles  bool      `json:"write_files"`
	Shell       bool      `json:"shell"`
	Services    []Service `json:"services,omitempty"`
}

type Service struct {
	Name       string `json:"name"`
	BaseURL    string `json:"base_url"`
	TokenEnv   string `json:"token_env,omitempty"`
	AuthScheme string `json:"auth_scheme,omitempty"`
	AllowWrite bool   `json:"allow_write"`
}

// Limits are advertised by the host owner. Agent settings cannot widen them.
type Limits struct {
	WriteFiles  bool     `json:"write_files"`
	Shell       bool     `json:"shell"`
	HTTPOrigins []string `json:"http_origins"`
}

var namePattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]{0,39}$`)
var envPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,99}$`)

func Parse(raw []byte) (Config, error) {
	var cfg Config
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return cfg, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if len(raw) > 32768 || dec.Decode(&cfg) != nil || dec.Decode(new(any)) != io.EOF {
		return Config{}, errors.New("invalid native runtime configuration")
	}
	// Non-object values must not silently become an empty policy.
	if bytes.TrimSpace(raw)[0] != '{' {
		return Config{}, errors.New("native runtime configuration must be an object")
	}
	if cfg.Environment != "" && cfg.Environment != "local" && cfg.Environment != "multica_jobs" {
		return Config{}, errors.New("native environment must be local or multica_jobs")
	}
	if cfg.Environment == "multica_jobs" {
		found := false
		for _, s := range cfg.Services {
			if s.Name == "multica_jobs" {
				found = true
				if (cfg.Shell || cfg.WriteFiles) && !s.AllowWrite {
					return Config{}, errors.New("remote execution requires multica_jobs write permission")
				}
			}
		}
		if !found {
			return Config{}, errors.New("remote environment requires multica_jobs service")
		}
	}
	if len(cfg.Services) > 8 {
		return Config{}, errors.New("native runtime supports at most eight external services")
	}
	seen := map[string]bool{}
	for _, s := range cfg.Services {
		u, err := url.Parse(s.BaseURL)
		if !namePattern.MatchString(s.Name) || seen[s.Name] || err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" || strings.Contains(u.Path, "..") {
			return Config{}, errors.New("invalid external service name or base URL")
		}
		if s.TokenEnv != "" && (!envPattern.MatchString(s.TokenEnv) || strings.HasPrefix(s.TokenEnv, "MULTICA_") || s.TokenEnv == "OPENAI_API_KEY") {
			return Config{}, errors.New("invalid external service credential environment name")
		}
		if s.AuthScheme != "" && s.AuthScheme != "Bearer" && s.AuthScheme != "Token" {
			return Config{}, errors.New("external service authentication must be Bearer or Token")
		}
		seen[s.Name] = true
	}
	return cfg, nil
}

func Origin(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.User != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	return strings.ToLower(u.Scheme + "://" + u.Host)
}

// Public strips URL paths and credentials before host policy enters metadata.
func (l Limits) Public() Limits {
	origins := []string{}
	seen := map[string]bool{}
	for _, raw := range l.HTTPOrigins {
		origin := Origin(raw)
		if origin != "" && !seen[origin] {
			origins = append(origins, origin)
			seen[origin] = true
		}
	}
	l.HTTPOrigins = origins
	return l
}

func (c Config) Check(l Limits) error {
	if c.WriteFiles && !l.WriteFiles {
		return errors.New("native file writes are disabled by the host owner")
	}
	if c.Shell && !l.Shell {
		return errors.New("native shell execution is disabled by the host owner")
	}
	for _, s := range c.Services {
		allowed := false
		for _, o := range l.HTTPOrigins {
			if Origin(o) != "" && Origin(o) == Origin(s.BaseURL) {
				allowed = true
				break
			}
		}
		if !allowed {
			return errors.New("native external service origin is not approved by the host owner")
		}
	}
	return nil
}
