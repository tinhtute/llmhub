package management

import (
	"net/http"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/therealtinhtute/llmhub/internal/registry"
)

// GetStaticModelDefinitions returns model metadata for a given channel.
// Channel is provided via path param (:channel) or query param (?channel=...).
//
// The static definitions are authoritative. When the channel has no static
// list, the handler falls back to the global registry's available models for
// that provider (ported from CLIProxyAPI 50cfd238; the upstream plugin-host
// branch is not ported, llmhub has no plugin runtime).
//
// Behavior change for API consumers: a channel outside the static switch now
// answers 200 with the registry-derived list whenever the registry has any
// available model for that provider, and 400 "unknown channel" only when both
// sources are empty. The response shape is unchanged.
func (h *Handler) GetStaticModelDefinitions(c *gin.Context) {
	channel := strings.TrimSpace(c.Param("channel"))
	if channel == "" {
		channel = strings.TrimSpace(c.Query("channel"))
	}
	if channel == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "channel is required"})
		return
	}

	models := registry.GetStaticModelDefinitionsByChannel(channel)
	if len(models) == 0 {
		models = availableModelDefinitionsByChannel(channel)
	}
	if len(models) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unknown channel", "channel": channel})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"channel": strings.ToLower(strings.TrimSpace(channel)),
		"models":  models,
	})
}

// availableModelDefinitionsByChannel returns the global registry's available
// models for a provider. GetAvailableModelsByProvider builds its result from a
// map, so the list is sorted by model id (nil entries last) to keep the response
// deterministic; it already returns at most one clone per model id.
func availableModelDefinitionsByChannel(channel string) []*registry.ModelInfo {
	models := registry.GetGlobalRegistry().GetAvailableModelsByProvider(channel)
	if len(models) > 1 {
		sort.Slice(models, func(i, j int) bool {
			if models[i] == nil {
				return models[j] != nil
			}
			if models[j] == nil {
				return false
			}
			return models[i].ID < models[j].ID
		})
	}
	return models
}
