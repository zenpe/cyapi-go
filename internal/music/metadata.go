package music

// Metadata is account-independent song information, not a resolved playback URL.
type Metadata struct {
	ID, Title, Artist, Album string
	DurationMS               int64
}
