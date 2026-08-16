//go:build test_unit

package player_test

import (
	"errors"
	"io"
	"testing"

	librespot "github.com/devgianlu/go-librespot"
	"github.com/devgianlu/go-librespot/player"
	"github.com/stretchr/testify/mock"
)

// resolved wraps src (nil included) in an already-filled channel, standing in
// for narration synthesis that has already finished by the time it's needed -
// the common case NewNarratedSource is built around.
func resolved(src librespot.AudioSource) <-chan librespot.AudioSource {
	ch := make(chan librespot.AudioSource, 1)
	ch <- src
	return ch
}

// emitting sets a mock up to serve n samples of value v, then io.EOF, so the
// join between the lead-in and the track can be checked sample by sample.
func emitting(t *testing.T, n int, v float32) *librespot.MockAudioSource {
	t.Helper()

	src := librespot.NewMockAudioSource(t)
	remaining := n

	src.EXPECT().Read(mock.Anything).RunAndReturn(func(p []float32) (int, error) {
		if remaining == 0 {
			return 0, io.EOF
		}

		got := min(len(p), remaining)
		for i := range got {
			p[i] = v
		}
		remaining -= got

		if remaining == 0 {
			return got, io.EOF
		}
		return got, nil
	}).Maybe()

	return src
}

func drainSource(t *testing.T, s librespot.AudioSource) []float32 {
	t.Helper()

	var out []float32
	buf := make([]float32, 64)
	for range 1000 {
		n, err := s.Read(buf)
		out = append(out, buf[:n]...)
		if errors.Is(err, io.EOF) {
			return out
		} else if err != nil {
			t.Fatalf("read failed: %v", err)
		}
	}

	t.Fatal("source never ended")
	return nil
}

func TestNarratedPlaysIntroThenTrack(t *testing.T) {
	lead := emitting(t, 100, 0.25)
	main := emitting(t, 200, 0.75)

	got := drainSource(t, player.NewNarratedSource(&librespot.NullLogger{}, true, resolved(lead), main, false, nil))

	if len(got) != 300 {
		t.Fatalf("got %d samples, want 300", len(got))
	}
	for i, v := range got {
		want := float32(0.75)
		if i < 100 {
			want = 0.25
		}
		if v != want {
			t.Fatalf("sample %d = %f, want %f (lead-in first, then the track)", i, v, want)
		}
	}
}

// The whole point of the design: narration is optional, the music is not.
func TestNarratedSurvivesBrokenIntro(t *testing.T) {
	lead := librespot.NewMockAudioSource(t)
	lead.EXPECT().Read(mock.Anything).Return(0, errors.New("cdn went away")).Maybe()

	main := emitting(t, 128, 0.5)

	got := drainSource(t, player.NewNarratedSource(&librespot.NullLogger{}, true, resolved(lead), main, false, nil))

	if len(got) != 128 {
		t.Fatalf("got %d samples, want the full track (128) despite the broken lead-in", len(got))
	}
}

func TestNarratedEmptyIntro(t *testing.T) {
	lead := emitting(t, 0, 0)
	main := emitting(t, 64, 0.5)

	if got := drainSource(t, player.NewNarratedSource(&librespot.NullLogger{}, true, resolved(lead), main, false, nil)); len(got) != 64 {
		t.Fatalf("got %d samples, want 64", len(got))
	}
}

// A pending intro - synthesis not resolved by the time it's needed - is
// waited on rather than skipped.
func TestNarratedWaitsForPendingIntro(t *testing.T) {
	lead := emitting(t, 40, 0.25)
	main := emitting(t, 60, 0.75)

	ch := make(chan librespot.AudioSource, 1)
	go func() { ch <- lead }()

	got := drainSource(t, player.NewNarratedSource(&librespot.NullLogger{}, true, ch, main, false, nil))
	if len(got) != 100 {
		t.Fatalf("got %d samples, want 100 (40 intro + 60 track)", len(got))
	}
}

// Synthesis resolving to nil (failed or timed out) is the same as never
// having had an intro at all.
func TestNarratedIntroSynthesisFailed(t *testing.T) {
	main := emitting(t, 64, 0.5)

	if got := drainSource(t, player.NewNarratedSource(&librespot.NullLogger{}, true, resolved(nil), main, false, nil)); len(got) != 64 {
		t.Fatalf("got %d samples, want the full track (64) with no intro", len(got))
	}
}

// Seeking is a request for the track, so the introduction is abandoned.
func TestNarratedSeekSkipsIntro(t *testing.T) {
	lead := emitting(t, 100, 0.25)

	main := emitting(t, 100, 0.75)
	main.EXPECT().SetPositionMs(int64(5000)).Return(nil).Once()

	s := player.NewNarratedSource(&librespot.NullLogger{}, true, resolved(lead), main, false, nil)
	if err := s.SetPositionMs(5000); err != nil {
		t.Fatalf("seek failed: %v", err)
	}

	for i, v := range drainSource(t, s) {
		if v != 0.75 {
			t.Fatalf("sample %d = %f, want only track audio after a seek", i, v)
		}
	}
}

// Seeking abandons a still-synthesizing intro too, without waiting on it.
func TestNarratedSeekAbandonsPendingIntro(t *testing.T) {
	main := emitting(t, 50, 0.75)
	main.EXPECT().SetPositionMs(int64(2000)).Return(nil).Once()

	introCh := make(chan librespot.AudioSource) // never sent to

	s := player.NewNarratedSource(&librespot.NullLogger{}, true, introCh, main, false, nil)
	if err := s.SetPositionMs(2000); err != nil {
		t.Fatalf("seek failed: %v", err)
	}

	got := drainSource(t, s)
	if len(got) != 50 {
		t.Fatalf("got %d samples, want 50 (track only, seek must not block on the abandoned intro)", len(got))
	}
}

// Position is the track's, so a controller shows the track as not yet started
// while the DJ is talking rather than jumping around.
func TestNarratedPositionIsTrackPosition(t *testing.T) {
	lead := librespot.NewMockAudioSource(t)

	main := librespot.NewMockAudioSource(t)
	main.EXPECT().PositionMs().Return(42).Once()

	if pos := player.NewNarratedSource(&librespot.NullLogger{}, true, resolved(lead), main, false, nil).PositionMs(); pos != 42 {
		t.Errorf("PositionMs = %d, want the main source's 42", pos)
	}
}

func TestNarratedPlaysOutroAfterTrack(t *testing.T) {
	intro := emitting(t, 50, 0.25)
	main := emitting(t, 100, 0.75)
	outro := emitting(t, 30, 0.5)

	got := drainSource(t, player.NewNarratedSource(&librespot.NullLogger{}, true, resolved(intro), main, true, resolved(outro)))

	if len(got) != 180 {
		t.Fatalf("got %d samples, want 180 (50 intro + 100 track + 30 outro)", len(got))
	}
	for i, v := range got {
		var want float32
		switch {
		case i < 50:
			want = 0.25
		case i < 150:
			want = 0.75
		default:
			want = 0.5
		}
		if v != want {
			t.Fatalf("sample %d = %f, want %f (intro, then track, then outro)", i, v, want)
		}
	}
}

func TestNarratedOutroOnly(t *testing.T) {
	main := emitting(t, 64, 0.75)
	outro := emitting(t, 16, 0.5)

	if got := drainSource(t, player.NewNarratedSource(&librespot.NullLogger{}, false, nil, main, true, resolved(outro))); len(got) != 80 {
		t.Fatalf("got %d samples, want 80", len(got))
	}
}

// A broken outro must not swallow the end of the track or error the stream.
func TestNarratedSurvivesBrokenOutro(t *testing.T) {
	main := emitting(t, 64, 0.75)

	outro := librespot.NewMockAudioSource(t)
	outro.EXPECT().Read(mock.Anything).Return(0, errors.New("cdn went away")).Maybe()

	if got := drainSource(t, player.NewNarratedSource(&librespot.NullLogger{}, false, nil, main, true, resolved(outro))); len(got) != 64 {
		t.Fatalf("got %d samples, want the full track (64)", len(got))
	}
}

// The common case: outro synthesis, kicked off well before the track ends,
// has already resolved by the time playback actually reaches it, so Read
// does not block waiting on it.
func TestNarratedOutroAlreadyReadyDoesNotBlock(t *testing.T) {
	main := emitting(t, 64, 0.75)
	outro := emitting(t, 16, 0.5)

	// A channel nothing is still sending to - if Read blocked on it, this
	// would hang instead of returning EOF once drained.
	ch := resolved(outro)

	if got := drainSource(t, player.NewNarratedSource(&librespot.NullLogger{}, false, nil, main, true, ch)); len(got) != 80 {
		t.Fatalf("got %d samples, want 80 (64 track + 16 outro)", len(got))
	}
}

// Seeking abandons the introduction but keeps the closing remark, which belongs
// to the end of the track rather than to how it was reached.
func TestNarratedSeekKeepsOutro(t *testing.T) {
	intro := emitting(t, 50, 0.25)

	main := emitting(t, 100, 0.75)
	main.EXPECT().SetPositionMs(int64(1000)).Return(nil).Once()

	outro := emitting(t, 20, 0.5)

	s := player.NewNarratedSource(&librespot.NullLogger{}, true, resolved(intro), main, true, resolved(outro))
	if err := s.SetPositionMs(1000); err != nil {
		t.Fatalf("seek failed: %v", err)
	}

	got := drainSource(t, s)
	if len(got) != 120 {
		t.Fatalf("got %d samples, want 120 (track + outro, no intro)", len(got))
	}
	for i, v := range got {
		if i < 100 && v != 0.75 {
			t.Fatalf("sample %d = %f, want track audio", i, v)
		}
		if i >= 100 && v != 0.5 {
			t.Fatalf("sample %d = %f, want outro audio", i, v)
		}
	}
}

// The bug this guards: with crossfading on, player.SwitchingAudioSource withholds the
// last crossfadeSamples of the primary and only releases them as a fade under
// the next track. An outro is shorter than a typical fade, so the whole of it
// was consumed fading out and never actually heard.
func TestNarratedOutroSurvivesCrossfade(t *testing.T) {
	const crossfadeSamples = 4096

	track := emitting(t, 2000, 0.75)
	outro := emitting(t, 500, 0.5)
	narrated := player.NewNarratedSource(&librespot.NullLogger{}, false, nil, track, true, resolved(outro))

	if !narrated.NoCrossfade() {
		t.Fatal("a source ending in an outro should decline crossfading")
	}

	s := player.NewSwitchingAudioSource(crossfadeSamples)
	s.SetPrimary(narrated)
	// A next track is waiting, which is exactly when the fade would engage.
	s.SetSecondary(emitting(t, 1000, 0.25))

	var track1, outroN, next int
	buf := make([]float32, 128)
	for range 1000 {
		n, err := s.Read(buf)
		for _, v := range buf[:n] {
			switch v {
			case 0.75:
				track1++
			case 0.5:
				outroN++
			case 0.25:
				next++
			default:
				t.Fatalf("sample %f is neither source: it was faded", v)
			}
		}
		if errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatalf("read failed: %v", err)
		}
	}

	if track1 != 2000 {
		t.Errorf("track samples = %d, want 2000", track1)
	}
	if outroN != 500 {
		t.Errorf("outro samples = %d, want all 500 heard at full level", outroN)
	}
	if next == 0 {
		t.Error("never reached the next track")
	}
}

// A track with only an introduction still crossfades: its tail is ordinary
// music, so there is nothing to protect.
func TestNarratedIntroOnlyStillCrossfades(t *testing.T) {
	narrated := player.NewNarratedSource(&librespot.NullLogger{}, true, resolved(emitting(t, 100, 0.25)), emitting(t, 500, 0.75), false, nil)

	if narrated.NoCrossfade() {
		t.Error("a source with no outro should crossfade as usual")
	}
}

// hasOutro is known from metadata up front, independent of whether synthesis
// itself later succeeds - so a track declaring an outro still declines
// crossfading even if that outro's synthesis ultimately fails.
func TestNarratedDeclinesCrossfadeEvenIfOutroSynthesisFails(t *testing.T) {
	narrated := player.NewNarratedSource(&librespot.NullLogger{}, false, nil, emitting(t, 100, 0.75), true, resolved(nil))

	if !narrated.NoCrossfade() {
		t.Fatal("a track declaring an outro should decline crossfading regardless of synthesis outcome")
	}
}
