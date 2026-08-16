package player

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	librespot "github.com/devgianlu/go-librespot"
	"github.com/devgianlu/go-librespot/mp3"
)

// maxNarrationSize caps what will be pulled into memory for one clip. Narration
// runs a couple of seconds and weighs a few hundred kilobytes; anything wildly
// larger means something other than a narration clip is on the other end.
const maxNarrationSize = 8 << 20

// NarrationSource plays a DJ narration clip: MP3 fetched from a CDN with a plain
// unauthenticated GET, with no Spotify id, audio key or PlayPlay involved.
//
// The whole clip is buffered. It is small, and the decoder needs to seek.
type NarrationSource struct {
	*mp3.Decoder
}

// NewNarrationSource fetches a synthesized narration clip and decodes it.
//
// The url from Spclient.NarrationUrl is already signed, so it is fetched without
// the Authorization header the rest of the client sends. The levels come from
// the track's narration.*.loudness and narration.*.true_peak metadata.
func NewNarrationSource(ctx context.Context, log librespot.Logger, client *http.Client,
	url string, loudnessDb, truePeakDb, pregain float32) (*NarrationSource, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed creating narration request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed fetching narration audio: %w", err)
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("invalid status code from narration audio: %d", resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxNarrationSize+1))
	if err != nil {
		return nil, fmt.Errorf("failed reading narration audio: %w", err)
	} else if len(data) > maxNarrationSize {
		return nil, fmt.Errorf("narration audio is too large (over %d bytes)", maxNarrationSize)
	} else if len(data) == 0 {
		return nil, fmt.Errorf("narration audio is empty")
	}

	gain := normalisationFactorFor(loudnessDb, truePeakDb, pregain)

	dec, err := mp3.New(log, bytes.NewReader(data), gain)
	if err != nil {
		return nil, fmt.Errorf("failed initializing narration mp3 stream: %w", err)
	}

	if dec.SampleRate != SampleRate {
		_ = dec.Close()
		return nil, fmt.Errorf("unsupported sample rate: %d", dec.SampleRate)
	} else if dec.Channels != Channels {
		_ = dec.Close()
		return nil, fmt.Errorf("unsupported channels: %d", dec.Channels)
	}

	log.Debugf("narration clip ready: %d bytes, gain = %.3f", len(data), gain)

	return &NarrationSource{Decoder: dec}, nil
}

// narrationStage tracks where in the intro/main/outro sequence a NarratedSource
// currently is.
type narrationStage int

const (
	stageIntro narrationStage = iota
	stageMain
	stageOutro
	stageDone
)

// NarratedSource plays a track with the DJ talking around it: an introduction
// before it and a closing remark after it, either of which may be absent.
//
// Playback is sequential rather than mixed, matching the ms_narration_overlapping
// of 0 the official client reports. Position stays the track's throughout, so it
// reads as not yet started while the DJ introduces it.
//
// Synthesizing a clip is a network round trip that can take a couple of
// seconds, so intro and outro each arrive on their own channel rather than as
// an already-resolved source: Read only ever waits on one of them at the
// point it is actually about to be needed, instead of the caller blocking
// upfront on synthesis of both before playback (or a reply to whatever
// command triggered it) can proceed at all. By the time playback reaches the
// outro, synthesis - kicked off back when the track started - has almost
// always long since finished, so that wait is normally instant.
type NarratedSource struct {
	log librespot.Logger

	stage narrationStage

	hasIntro bool
	introCh  <-chan librespot.AudioSource
	intro    librespot.AudioSource

	main librespot.AudioSource

	hasOutro bool
	outroCh  <-chan librespot.AudioSource
	outro    librespot.AudioSource
}

// NewNarratedSource wraps a track with its narration. introCh and outroCh
// each deliver exactly one value - the synthesized clip, or nil if there is
// none or synthesis failed - and may themselves be nil when hasIntro/hasOutro
// is false. hasIntro/hasOutro are known synchronously from track metadata
// (see NoCrossfade), independent of whether synthesis itself later succeeds.
func NewNarratedSource(log librespot.Logger, hasIntro bool, introCh <-chan librespot.AudioSource,
	main librespot.AudioSource, hasOutro bool, outroCh <-chan librespot.AudioSource) *NarratedSource {
	s := &NarratedSource{
		log:      log,
		hasIntro: hasIntro,
		introCh:  introCh,
		main:     main,
		hasOutro: hasOutro,
		outroCh:  outroCh,
	}

	if !hasIntro {
		s.stage = stageMain
	}

	return s
}

func (s *NarratedSource) Read(p []float32) (int, error) {
	for {
		src, ok := s.current()
		if !ok {
			return 0, io.EOF
		}

		n, err := src.Read(p)

		switch {
		case err == nil:
			return n, nil

		case errors.Is(err, io.EOF):
			s.advance()
			if n > 0 {
				return n, nil
			}

		default:
			// A failing track is a real error, but a failing narration clip is
			// not: drop it and carry on rather than losing the music.
			if s.stage == stageMain {
				return n, err
			}

			s.log.WithError(err).Warnf("narration playback failed, skipping it")
			s.advance()
			if n > 0 {
				return n, nil
			}
		}
	}
}

// EnsureReady blocks until whatever Read would return first is resolved -
// waiting out a still-synthesizing intro here, rather than leaving that wait
// for the audio output's first Read call.
//
// This matters because of how the pulseaudio backend's first Start() call
// works (see third_party/pulse/playback.go): it uncorks the server-side
// stream and only then waits for the reader to produce the first buffer.
// PulseAudio treats a slow-arriving first buffer as an underrun right at
// stream startup, which on at least one real backend (see driver-pulseaudio.go's
// PlaybackLatency comment) has been observed to audibly repeat the
// beginning of playback once data does arrive, rather than merely a longer
// silence. A synchronous Read taking that first hit is a much larger
// problem than the same wait happening here, before the source is ever
// handed to the output layer.
//
// Safe to skip - Read would resolve the same pending channel itself, just
// too late for the output layer's sake. Call it once, right before handing
// the source to the output layer (e.g. Player.SetPrimaryStream).
func (s *NarratedSource) EnsureReady() {
	s.current()
}

// current resolves (receiving from its channel, blocking if synthesis is
// still in flight) and returns the source for the current stage, skipping
// past any stage that turns out to have nothing to play.
func (s *NarratedSource) current() (librespot.AudioSource, bool) {
	for {
		switch s.stage {
		case stageIntro:
			if s.intro == nil {
				s.intro = <-s.introCh
			}
			if s.intro == nil {
				s.stage = stageMain
				continue
			}
			return s.intro, true

		case stageMain:
			return s.main, true

		case stageOutro:
			if s.outro == nil {
				s.outro = <-s.outroCh
			}
			if s.outro == nil {
				s.stage = stageDone
				continue
			}
			return s.outro, true

		default:
			return nil, false
		}
	}
}

func (s *NarratedSource) advance() {
	switch s.stage {
	case stageIntro:
		s.stage = stageMain
	case stageMain:
		if s.hasOutro {
			s.stage = stageOutro
		} else {
			s.stage = stageDone
		}
	case stageOutro:
		s.stage = stageDone
	}
}

// SetPositionMs seeks the track, abandoning an introduction still playing (or
// still synthesizing): a listener who scrubs wants the music. The closing
// remark still follows, since it belongs to the end of the track.
func (s *NarratedSource) SetPositionMs(pos int64) error {
	if s.stage == stageIntro {
		s.stage = stageMain
	}

	return s.main.SetPositionMs(pos)
}

// NoCrossfade keeps a closing remark out of the crossfade reserve, which would
// otherwise spend the whole of it fading out. A source with only an introduction
// ends in ordinary music and still crossfades.
func (s *NarratedSource) NoCrossfade() bool {
	return s.hasOutro
}

func (s *NarratedSource) PositionMs() int64 {
	return s.main.PositionMs()
}

func (s *NarratedSource) Close() error {
	var err error
	closeIfCloser := func(src librespot.AudioSource) {
		if src == nil {
			return
		}
		if closer, ok := src.(io.Closer); ok && closer != nil {
			if cerr := closer.Close(); cerr != nil {
				err = cerr
			}
		}
	}

	closeIfCloser(s.intro)
	closeIfCloser(s.main)
	closeIfCloser(s.outro)

	return err
}

// NarrationLoudness reads the loudness and true peak a track's narration
// metadata declares. Reporting absence matters: taking a missing value as zero
// would mean normalising against 0 LUFS and attenuating the clip to nothing.
func NarrationLoudness(metadata map[string]string, prefix string) (loudnessDb, truePeakDb float32, ok bool) {
	loudness, err := strconv.ParseFloat(metadata[prefix+".loudness"], 32)
	if err != nil {
		return 0, 0, false
	}

	truePeak, err := strconv.ParseFloat(metadata[prefix+".true_peak"], 32)
	if err != nil {
		return 0, 0, false
	}

	return float32(loudness), float32(truePeak), true
}
