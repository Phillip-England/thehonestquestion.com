package main

import (
	"bufio"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
)

type settings struct {
	Port, AdminUsername, AdminPassword, SessionSecret string
	TrustedProxyCIDRs                                 []*net.IPNet
	Email                                             emailGateway
}

func loadSettings(path string) (settings, error) {
	values := map[string]string{}
	file, err := os.Open(path)
	if err != nil && !os.IsNotExist(err) {
		return settings{}, err
	}
	if err == nil {
		defer file.Close()
		scan := bufio.NewScanner(file)
		for scan.Scan() {
			line := strings.TrimSpace(scan.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			key, value, ok := strings.Cut(line, "=")
			if !ok {
				return settings{}, fmt.Errorf("invalid config line in %s", path)
			}
			value = strings.TrimSpace(value)
			if len(value) >= 2 && (value[0] == '\'' && value[len(value)-1] == '\'' || value[0] == '"' && value[len(value)-1] == '"') {
				value = value[1 : len(value)-1]
			}
			values[strings.TrimSpace(key)] = value
		}
		if err := scan.Err(); err != nil {
			return settings{}, err
		}
	}
	// Environment values take precedence for local process management.
	for _, key := range []string{"PORT", "ADMIN_USERNAME", "ADMIN_PASSWORD", "SESSION_SECRET", "TRUSTED_PROXY_CIDRS", "EGATE_HOST", "EGATE_API_KEY", "EGATE_SENDER_EMAIL", "EGATE_TO_EMAIL"} {
		if value, ok := os.LookupEnv(key); ok {
			values[key] = value
		}
	}
	s := settings{Port: values["PORT"], AdminUsername: values["ADMIN_USERNAME"], AdminPassword: values["ADMIN_PASSWORD"], SessionSecret: values["SESSION_SECRET"]}
	s.Email = emailGateway{Host: strings.TrimSpace(values["EGATE_HOST"]), APIKey: strings.TrimSpace(values["EGATE_API_KEY"]), Sender: strings.TrimSpace(values["EGATE_SENDER_EMAIL"]), Recipient: strings.TrimSpace(values["EGATE_TO_EMAIL"])}
	if s.Email.Host != "" || s.Email.APIKey != "" || s.Email.Sender != "" || s.Email.Recipient != "" {
		parsed, err := url.Parse(s.Email.Host)
		if !s.Email.enabled() || err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || !emailPattern.MatchString(s.Email.Sender) || !emailPattern.MatchString(s.Email.Recipient) {
			return s, fmt.Errorf("set a valid EGATE_HOST, EGATE_API_KEY, EGATE_SENDER_EMAIL, and EGATE_TO_EMAIL together")
		}
	}
	if s.Port == "" {
		s.Port = "8818"
	}
	if strings.TrimSpace(s.AdminUsername) == "" || strings.TrimSpace(s.AdminPassword) == "" || len(strings.TrimSpace(s.SessionSecret)) < 32 {
		return s, fmt.Errorf("set ADMIN_USERNAME, ADMIN_PASSWORD, and a SESSION_SECRET of at least 32 characters in %s", path)
	}
	if raw := strings.TrimSpace(values["TRUSTED_PROXY_CIDRS"]); raw != "" {
		for _, part := range strings.Split(raw, ",") {
			_, network, err := net.ParseCIDR(strings.TrimSpace(part))
			if err != nil {
				return s, fmt.Errorf("invalid TRUSTED_PROXY_CIDRS entry %q: %w", part, err)
			}
			s.TrustedProxyCIDRs = append(s.TrustedProxyCIDRs, network)
		}
	}
	return s, nil
}
