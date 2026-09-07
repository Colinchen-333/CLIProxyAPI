package config

// SplitRelayConfig routes Claude traffic through its dedicated local identity
// proxy and dispatches registered own models directly into the API handler.
type SplitRelayConfig struct {
	// Listen is a comma-separated list of literal loopback addresses.
	Listen           string `yaml:"listen" json:"listen"`
	OfficialProxyURL string `yaml:"official-proxy-url" json:"official-proxy-url"`
	MaxRequestBytes  int64  `yaml:"max-request-bytes,omitempty" json:"max-request-bytes,omitempty"`
}
