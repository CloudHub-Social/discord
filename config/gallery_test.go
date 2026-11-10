package config

import (
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestAttachmentGalleriesEnabledDefaultAndOverride(t *testing.T) {
	for _, tc := range []struct {
		input   string
		enabled bool
	}{{"", true}, {"discord_attachment_galleries: true", true}, {"discord_attachment_galleries: false", false}} {
		var config BridgeConfig
		require.NoError(t, yaml.Unmarshal([]byte("username_template: 'discord_{{.}}'\n"+tc.input), &config))
		require.Equal(t, tc.enabled, config.AttachmentGalleriesEnabled())
	}
}
