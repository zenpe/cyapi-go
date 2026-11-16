package qq

import (
	"cyapi-go/internal/music"
	p "cyapi-go/internal/providers/platformhttp"
)

func optionalDuration(value any) *int64 {
	ms, ok := value.(int64)
	if !ok {
		ms = p.Int(value)
	}
	if !music.ValidDurationMS(ms) {
		return nil
	}
	return &ms
}
