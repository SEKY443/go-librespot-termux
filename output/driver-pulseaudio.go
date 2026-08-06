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

// defaultPulseCallTimeout is used when NewOutputOptions.CallTimeout is zero.
// See NewOutputOptions.CallTimeout's doc comment for why this needs to be
// bounded at all, and why the right bound varies by environment.
const defaultPulseCallTimeout = 10 * time.Second

// withPulseTimeout runs fn, which must perform exactly one PulseAudio client
// call, and bounds it to timeout, propagating fn's own error if it completes
// in time. See NewOutputOptions.CallTimeout's doc comment. done is buffered
// so a fn that only finishes after we've already timed out doesn't block
// trying to send its result - it's still a leaked goroutine, just not also a
// stuck one.
func withPulseTimeout(timeout time.Duration, fn func() error) error {
	done := make(chan error, 1)
	go func() {
		done <- fn()
	}()

	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
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
	callTimeout          time.Duration
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
	callTimeout := opts.CallTimeout
	if callTimeout <= 0 {
		callTimeout = defaultPulseCallTimeout
	}
	out := &pulseAudioOutput{
		log:                  opts.Log,
		sampleRate:           opts.SampleRate,
		reader:               opts.Reader,
		client:               client,
		externalVolumeUpdate: opts.VolumeUpdate,
		err:                  make(chan error, 2),
		callTimeout:          callTimeout,
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
		// Without an explicit target, the server picks its own buffer
		// target (observed: 2000ms), and the client library fills that
		// whole target in a single Send() on Start(). At 44.1kHz/stereo/
		// float32 that's ~705KB in one chunk - far past PulseAudio's
		// default 64KB memory pool block size, logged server-side as
		// "Memory block too large for pool". On at least one real setup
		// (Termux/Android, the OpenSL ES sink module) that oversized first
		// chunk was immediately followed by an underrun and the sink
		// dropping back to idle before ever confirming playback started -
		// exactly the hang Resume() was timing out on. 100ms keeps every
		// chunk comfortably under the pool size regardless of sample rate.
		//
		// PlaybackLatency alone sets AdjustLatency: true, letting the server
		// grow the target back up over the stream's lifetime if it decides
		// that's needed for stability - which would silently reintroduce
		// this exact bug well after the stream started, on a later Resume()
		// (e.g. after a track skip) rather than the first one. The
		// following raw option pins AdjustLatency back to false, keeping
		// the requested size fixed for the life of the stream.
		pulse.PlaybackLatency(0.1),
		pulse.PlaybackRawOption(func(req *proto.CreatePlaybackStream) {
			req.AdjustLatency = false
		}),
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
		err := withPulseTimeout(out.callTimeout, func() error {
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
	err := withPulseTimeout(out.callTimeout, func() error {
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
		err := withPulseTimeout(out.callTimeout, func() error {
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
	err := withPulseTimeout(out.callTimeout, func() error {
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
