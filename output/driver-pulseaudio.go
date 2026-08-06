package output

import (
	"fmt"
	"io"
	"sync"
	"time"

	librespot "github.com/devgianlu/go-librespot"
	"github.com/jfreymuth/pulse"
	"github.com/jfreymuth/pulse/proto"
)

// pulseCallTimeout bounds how long we wait for a PulseAudio server round
// trip. The jfreymuth/pulse client performs these as plain blocking calls
// with no timeout or cancellation of its own: if the server never replies
// (observed in practice: a sink stuck "suspended", e.g. via PulseAudio's
// macOS CoreAudio bridge, so the *proto.Started event Start() waits for is
// never sent), the call blocks forever. Since these methods run on the
// player's single command loop, an unbounded hang here would freeze the
// entire daemon, not just audio - including replying to Spotify. A caller
// that gets a timeout here must treat the output as broken and discard it
// rather than reuse it: the library call's goroutine is abandoned in place
// (leaked) rather than left to mutate the stream's state after we've moved
// on, but that also means e.g. an abandoned Start() leaves the stream
// thinking it's already running, silently no-oping any future Start() call.
const pulseCallTimeout = 10 * time.Second

// withPulseTimeout runs fn, which must perform exactly one PulseAudio client
// call, and bounds it to pulseCallTimeout, propagating fn's own error if it
// completes in time. See pulseCallTimeout's doc comment. done is buffered so
// a fn that only finishes after we've already timed out doesn't block trying
// to send its result - it's still a leaked goroutine, just not also a stuck
// one.
func withPulseTimeout(fn func() error) error {
	done := make(chan error, 1)
	go func() {
		done <- fn()
	}()

	select {
	case err := <-done:
		return err
	case <-time.After(pulseCallTimeout):
		return fmt.Errorf("timed out waiting for pulseaudio server")
	}
}

type pulseAudioOutput struct {
	log librespot.Logger

	sampleRate           int
	reader               librespot.Float32Reader
	client               *pulse.Client
	stream               *pulse.PlaybackStream
	volume               proto.Volume
	volumeLock           sync.Mutex
	externalVolumeUpdate chan float32
	err                  chan error
}

func newPulseAudioOutput(opts *NewOutputOptions) (*pulseAudioOutput, error) {
	// Initialize the PulseAudio client.
	// The device name is shown by PulseAudio volume controls (usually built
	// into a desktop environment), so we might want to use device_name here.
	// We could also maybe change the application icon name by device_type.
	clientopts := []pulse.ClientOption{pulse.ClientApplicationName("go-librespot"), pulse.ClientApplicationIconName("speaker")}

	if opts.RuntimeSocket != "" {
		clientopts = append(clientopts, pulse.ClientServerString(opts.RuntimeSocket))
	}

	client, err := pulse.NewClient(clientopts...)
	if err != nil {
		return nil, err
	}
	out := &pulseAudioOutput{
		log:                  opts.Log,
		sampleRate:           opts.SampleRate,
		reader:               opts.Reader,
		client:               client,
		externalVolumeUpdate: opts.VolumeUpdate,
		err:                  make(chan error, 2),
	}

	// Create a new playback.
	var channelOpt pulse.PlaybackOption
	switch opts.ChannelCount {
	case 1:
		channelOpt = pulse.PlaybackMono
	case 2:
		channelOpt = pulse.PlaybackStereo
	default:
		return nil, fmt.Errorf("cannot play %d channels, pulse only supports mono and stereo", opts.ChannelCount)
	}
	// Deliberately not requesting pulse.PlaybackVolumeChanges here (external
	// volume-mixer changes reflected back into Spotify): the library's event
	// handler for it has a real race with Close() - it can observe the
	// stream's sink-input already deleted server-side while p.Closed() still
	// reports false, and unconditionally panics in that case, which crashes
	// the whole process since it happens on a goroutine we don't control and
	// can't recover(). Spotify-driven volume changes (SetVolume below) are
	// unaffected; only picking up changes made through an external mixer is
	// lost.
	lplaybackopts := []pulse.PlaybackOption{
		pulse.PlaybackSampleRate(out.sampleRate),
		channelOpt,
	}

	if opts.Device != "" {
		var lsink *pulse.Sink
		if opts.Device == "default" {
			lsink, err = client.DefaultSink()
		} else {
			lsink, err = client.SinkByID(opts.Device)
		}

		if err != nil {
			client.Close()
			return nil, fmt.Errorf("cannot find pulseaudio sink %s: %w", opts.Device, err)
		}

		lplaybackopts = append(lplaybackopts, pulse.PlaybackSink(lsink))
	}

	out.stream, err = out.client.NewPlayback(pulse.Float32Reader(out.float32Reader), lplaybackopts...)
	if err != nil {
		return nil, err
	}

	// Read the initial volume from PulseAudio.
	// PulseAudio strongly recommends against setting a default volume at
	// startup (especially if it's 100%), so instead we just follow the
	// PulseAudio provided volume.
	cvol, _ := out.stream.Volume()
	out.volume = cvol.Avg()
	sendVolumeUpdate(opts.VolumeUpdate, float32(out.volume.Norm()))

	return out, nil
}

func (out *pulseAudioOutput) float32Reader(buf []float32) (int, error) {
	n, err := out.reader.Read(buf)
	if err != nil {
		if err == io.EOF {
			// Might happen, so translate this error message.
			return n, pulse.EndOfData
		}

		// Encountered another error. This will result in a stopped player, so
		// send the error back to the player using a non-blocking send.
		select {
		case out.err <- err:
		default:
		}
		return n, err
	}
	return n, err
}

func (out *pulseAudioOutput) Pause() error {
	if out.stream.Running() {
		// Stop() will stop new samples from being requested, but will continue
		// to play whatever is in the buffer.
		out.stream.Stop()

		// To really stop playback *now*, we have to also flush everything
		// that's in the buffer.
		err := withPulseTimeout(func() error {
			return out.client.RawRequest(&proto.FlushPlaybackStream{
				StreamIndex: out.stream.StreamIndex(),
			}, nil)
		})
		if err != nil {
			return fmt.Errorf("Pause: could not flush playback: %w", err)
		}
	} else {
		// Nothing to do: we're already paused.
	}

	return nil
}

func (out *pulseAudioOutput) Resume() error {
	// Start the stream. This will start reading samples from out.reader and
	// push it to PulseAudio. It will do nothing if the playback is already
	// started.
	err := withPulseTimeout(func() error {
		out.stream.Start()
		return nil
	})
	if err != nil {
		return fmt.Errorf("Resume: could not start playback: %w", err)
	}
	return nil
}

func (out *pulseAudioOutput) Drop() error {
	if out.stream.Running() {
		// Stop and flush the buffer. We do not restart here: the caller
		// resumes once the new source is set. Restarting raced with the
		// source switch and clipped the start of the track (#292).
		out.stream.Stop()
		err := withPulseTimeout(func() error {
			return out.client.RawRequest(&proto.FlushPlaybackStream{
				StreamIndex: out.stream.StreamIndex(),
			}, nil)
		})
		if err != nil {
			return fmt.Errorf("Drop: could not flush playback: %w", err)
		}
	} else {
		// Already stopped, e.g. flushed in Pause().
	}
	return nil
}

func (out *pulseAudioOutput) DelayMs() (int64, error) {
	samples := out.stream.BufferSize()
	delay := int64(samples) * 1000 / int64(out.sampleRate)
	return delay, nil
}

func (out *pulseAudioOutput) SetVolume(vol float32) {
	volume := proto.NormVolume(float64(vol))

	out.volumeLock.Lock()
	if volume == out.volume {
		out.volumeLock.Unlock()
		return
	}
	out.volume = volume
	sendVolumeUpdate(out.externalVolumeUpdate, vol)
	out.volumeLock.Unlock()

	cvol := proto.ChannelVolumes{volume}
	err := withPulseTimeout(func() error {
		return out.stream.SetVolume(cvol)
	})
	if err != nil {
		out.log.WithError(err).Warn("failed to set volume")
	}
}

func (out *pulseAudioOutput) Error() <-chan error {
	return out.err
}

func (out *pulseAudioOutput) Close() error {
	out.stream.Close()
	out.client.Close()
	return nil
}
