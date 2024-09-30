package fluentbitconfig

import (
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestConfig_Generate(t *testing.T) {
	tests := []struct {
		name   string
		config *Config
		want   string
	}{
		{
			name: "full config",
			config: &Config{
				Service: map[string]string{
					"flush":     "1",
					"log_level": "info",
				},
				Input: []Input{
					map[string]string{
						"name": "http",
					},
				},
				Output: []Output{
					{
						"name":        {"    stdout  "},
						"match":       {"*"},
						"event_field": {"key1 value1", "key2 value2"},
					},
					{
						"name": {"null"},
					},
				},
				Includes: []Include{
					"data/*.conf",
				},
			},
			want: `[SERVICE]
    flush 1
    log_level info

[INPUT]
    name http

[OUTPUT]
    event_field key1 value1
    event_field key2 value2
    match *
    name stdout
[OUTPUT]
    name null

@INCLUDE data/*.conf`,
		},
		{
			name: "only output section",
			config: &Config{
				Output: []Output{
					map[string][]string{
						"name":        {"stdout"},
						"match":       {"*"},
						"event_field": {"key1 value1", "key2 value2"},
					},
				},
			},
			want: `[OUTPUT]
    event_field key1 value1
    event_field key2 value2
    match *
    name stdout`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.config.Generate()
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Logf("Got:\n\n%s\n", got)
				t.Errorf("diff: %s", diff)
			}
		})
	}
}

func TestOutput_Add(t *testing.T) {
	tt := []struct {
		desc      string
		values    []string
		newValues []string
		expected  []string
	}{
		{
			desc:      "add to new key",
			values:    nil,
			newValues: []string{"one"},
			expected:  []string{"one"},
		},
		{
			desc:      "add to existing key",
			values:    []string{"one"},
			newValues: []string{"two"},
			expected:  []string{"one", "two"},
		},
	}
	for _, tc := range tt {
		t.Run(tc.desc, func(t *testing.T) {
			key := "key"
			config := Output{key: tc.values}
			config.Add(key, tc.newValues...)

			if diff := cmp.Diff(tc.expected, config[key]); diff != "" {
				t.Logf("Got:\n\n%+v\n", config)
				t.Errorf("diff: %s", diff)
			}
		})
	}
}
