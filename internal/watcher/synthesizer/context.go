package synthesizer

import (
	"time"

	"github.com/therealtinhtute/llmhub/internal/config"
)

// SynthesisContext provides the context needed for auth synthesis.
type SynthesisContext struct {
	// Config is the current configuration
	Config *config.Config
	// AuthDir is the directory containing auth files
	AuthDir string
	// Now is the current time for timestamps
	Now time.Time
	// IDGenerator generates stable IDs for auth entries
	IDGenerator *StableIDGenerator
	// StrictFileParsing surfaces parse errors instead of silently skipping or falling back.
	StrictFileParsing bool
	// IncompleteFiles records auth paths a directory scan could not read to the
	// end: files whose os.ReadFile failed (the listing proved they exist, so the
	// failure cannot prove absence), files whose content is empty or not valid
	// JSON (an in-place write in progress), and the auth directory itself when
	// os.ReadDir failed with anything but fs.ErrNotExist.
	IncompleteFiles map[string]struct{}
}

// RecordIncompleteFile records path as unreadable or partially written during
// the current scan. It is nil-safe so single-file callers can leave it unset.
func (c *SynthesisContext) RecordIncompleteFile(path string) {
	if c == nil || path == "" {
		return
	}
	if c.IncompleteFiles == nil {
		c.IncompleteFiles = make(map[string]struct{})
	}
	c.IncompleteFiles[path] = struct{}{}
}
