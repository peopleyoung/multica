package llm

import (
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"strings"
)

// ParseCapabilities accepts one explicit capability document. Empty input
// leaves native execution disabled without disabling auxiliary generation.
func ParseCapabilities(raw string) (Capabilities, error) {
	var caps Capabilities
	if strings.TrimSpace(raw) == "" {
		return caps, nil
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&caps); err != nil {
		return Capabilities{}, errors.New("llm: capabilities must be a JSON object with known fields")
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return Capabilities{}, errors.New("llm: capabilities must contain exactly one JSON object")
	}
	if err := caps.Validate(); err != nil {
		return Capabilities{}, err
	}
	return caps, nil
}

// Validate fails startup on malformed configured endpoints or native settings.
// Error messages deliberately omit operator-provided URLs and credentials.
func (c Config) Validate() error {
	if base := strings.TrimSpace(c.BaseURL); base != "" {
		u, err := url.Parse(base)
		if err != nil || u.Hostname() == "" || u.Scheme != "http" && u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("llm: base URL must be an HTTP(S) endpoint without userinfo, query or fragment")
		}
	}
	if strings.ContainsAny(c.APIKey, "\r\n") {
		return errors.New("llm: API key must not contain line breaks")
	}
	if c.Capabilities != (Capabilities{}) {
		if err := c.Capabilities.Validate(); err != nil {
			return err
		}
		if strings.TrimSpace(c.DefaultModel) == "" {
			return errors.New("llm: native execution requires an explicit deployment model")
		}
	}
	return nil
}

func (c *Client) NativeCapabilities() (Capabilities, error) {
	if !c.Enabled() {
		return Capabilities{}, ErrNotConfigured
	}
	if c.configErr != nil {
		return Capabilities{}, c.configErr
	}
	if err := c.capabilities.Validate(); err != nil {
		return Capabilities{}, err
	}
	return c.capabilities, nil
}
