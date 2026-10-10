package lab

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"unicode/utf8"
)

// Limits for sending to a service of your own.
const (
	// MaxExternalBatch is the largest batch (or array) sent to your own service. The built-in
	// service has its own, smaller limit.
	MaxExternalBatch = 5000
	// MaxExternalRate caps records per second towards a host that is not this computer, so the app
	// cannot be pointed at someone else's server as a flood tool by a typo.
	MaxExternalRate = 2000.0

	maxHeaders     = 20
	maxHeaderValue = 4096
	maxURLLength   = 2048
)

// Payload formats for a service of your own. Every record keeps the Signal Lab field names.
const (
	FormatBatch  = "batch"  // {"events":[ ... ]}, what the built-in service expects
	FormatArray  = "array"  // [ ... ]
	FormatNDJSON = "ndjson" // one JSON record per line
	FormatSingle = "single" // one record per request, sent as a bare JSON object
)

// External reports whether the replay goes to a service of the user's own.
func (c Config) External() bool { return strings.TrimSpace(c.TargetURL) != "" }

// ParseTarget checks a destination URL: http or https, a host, no embedded credentials.
func ParseTarget(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("the service address is empty")
	}
	if len(raw) > maxURLLength {
		return nil, fmt.Errorf("the service address is longer than %d characters", maxURLLength)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil, errors.New("the service address must look like https://example.com/path")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, errors.New("the service address must start with http:// or https://")
	}
	if u.User != nil {
		return nil, errors.New("do not put a user name or password in the address; add an Authorization header instead")
	}
	if u.Fragment != "" {
		return nil, errors.New("the service address must not contain a # fragment")
	}
	if u.Hostname() == "" {
		return nil, errors.New("the service address has no host name")
	}
	return u, nil
}

// IsLoopbackHost reports whether host (without a port) is this computer.
func IsLoopbackHost(host string) bool {
	host = strings.Trim(strings.ToLower(host), "[]")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// DisplayURL is the address without anything that could be secret: only scheme://host[:port].
// Many services keep an API key in the query string and webhook-style endpoints keep one in the
// path, so neither is ever shown or logged, and credentials in the address are dropped.
func DisplayURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

func validHeaderName(s string) bool {
	if s == "" || len(s) > 100 {
		return false
	}
	for i := 0; i < len(s); i++ {
		b := s[i]
		ok := b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || strings.IndexByte("!#$%&'*+-.^_`|~", b) >= 0
		if !ok {
			return false
		}
	}
	return true
}

func validHeaderValue(s string) bool {
	if len(s) > maxHeaderValue || !utf8.ValidString(s) {
		return false
	}
	for i := 0; i < len(s); i++ {
		if b := s[i]; (b < 0x20 && b != '\t') || b == 0x7f {
			return false
		}
	}
	return true
}

// Headers the app sets itself; letting a user override them would break the request.
var reservedHeaders = map[string]bool{
	"host": true, "content-length": true, "transfer-encoding": true, "connection": true,
	"keep-alive": true, "upgrade": true, "te": true, "trailer": true,
}

// validateTarget checks the destination fields of c. Without a TargetURL no destination field
// may be set, so a header typed for one service can never go to the built-in one by accident.
func (c Config) validateTarget() []error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	if !c.External() {
		if len(c.TargetHeaders) > 0 {
			bad("headers need a service address")
		}
		if c.PayloadFormat != "" && c.PayloadFormat != FormatBatch {
			bad("a payload format other than the default needs a service address")
		}
		return errs
	}
	switch c.PayloadFormat {
	case "", FormatBatch, FormatArray, FormatNDJSON, FormatSingle:
	default:
		bad("payload format must be batch, array, ndjson or single")
	}
	if len(c.TargetHeaders) > maxHeaders {
		bad("at most %d headers", maxHeaders)
	}
	for k, v := range c.TargetHeaders {
		switch {
		case !validHeaderName(k):
			bad("%q is not a valid header name", k)
		case reservedHeaders[strings.ToLower(k)]:
			bad("the %s header is set by the app and cannot be changed", k)
		case !validHeaderValue(v):
			bad("the value of the %s header is not valid (no line breaks or control characters, at most %d characters)", k, maxHeaderValue)
		}
	}
	u, err := ParseTarget(c.TargetURL)
	if err != nil {
		bad("%v", err)
		return errs
	}
	if !IsLoopbackHost(u.Hostname()) {
		if !c.TargetConfirmed {
			bad("confirm that you own this service or have permission to send test traffic to it")
		}
		if c.RatePerS <= 0 || c.RatePerS > MaxExternalRate {
			bad("for a service outside this computer the rate must be between 1 and %d records per second", int(MaxExternalRate))
		}
		if c.RampToPerS > MaxExternalRate {
			bad("for a service outside this computer the ramp must stay within %d records per second", int(MaxExternalRate))
		}
	}
	return errs
}

// Redacted returns a copy that is safe to show or log: header values are hidden and the address
// is reduced to scheme://host. The runner keeps the real values only for the requests themselves.
func (c Config) Redacted() Config {
	out := c
	if c.TargetURL != "" {
		out.TargetURL = DisplayURL(c.TargetURL)
	}
	if len(c.TargetHeaders) > 0 {
		out.TargetHeaders = make(map[string]string, len(c.TargetHeaders))
		for k := range c.TargetHeaders {
			out.TargetHeaders[k] = "(hidden)"
		}
	}
	return out
}
