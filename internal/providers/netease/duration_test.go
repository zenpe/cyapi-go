package netease

import "testing"

func TestTrackPreservesOfficialMilliseconds(t *testing.T) {
	s := rawSong{ID: 123, Name: "Song", Duration: 212345}
	got, err := s.track()
	if err != nil || got.DurationMS != 212345 {
		t.Fatalf("%+v %v", got, err)
	}
}
