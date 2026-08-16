//go:build test_integration

package player_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	librespot "github.com/devgianlu/go-librespot"
	"github.com/devgianlu/go-librespot/player"
	narrationpb "github.com/devgianlu/go-librespot/proto/spotify/narration"
	"github.com/devgianlu/go-librespot/spclient"
)

// This is the mechanism daemon.AppPlayer.narrate uses to keep DJ narration
// synthesis off the player's single event loop - and off its reply to
// whatever command (typically a manual skip) triggered the load: kick
// synthesis off in a goroutine, hand NewNarratedSource a channel instead of
// an already-resolved source, and only actually wait on it once playback
// reaches that stage.
//
// spclient.Spclient.NarrationUrl and player.NewNarrationSource run for real
// here, against a local HTTPS server that deliberately takes its time on the
// synthesis request - the same kind of slow round trip that, before this
// fix, blocked a DJ skip's reply for as long as synthesis took (see
// daemon/narration.go). No live Spotify account is involved: the server is
// fully local, so this needs nothing beyond `go test -tags test_integration`.
func TestNarrationSynthesisDoesNotBlockPlayback(t *testing.T) {
	const synthesisDelay = 2 * time.Second

	clip, err := os.ReadFile("../mp3/testdata/sine_mono.mp3")
	if err != nil {
		t.Fatalf("failed reading narration audio fixture: %v", err)
	}

	var fulfillCalls int
	mux := http.NewServeMux()
	mux.HandleFunc("/narration.mp3", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write(clip)
	})

	server := httptest.NewUnstartedServer(mux)
	server.StartTLS()
	defer server.Close()

	// The Location a real fulfill response carries is an absolute CDN url,
	// which NarrationUrl hands back verbatim - registered once the server's
	// own address is known.
	mux.HandleFunc("/client-tts/v1/fulfill", func(w http.ResponseWriter, r *http.Request) {
		fulfillCalls++
		time.Sleep(synthesisDelay)
		w.Header().Set("Location", server.URL+"/narration.mp3")
		w.WriteHeader(http.StatusSeeOther)
	})

	addr := server.Listener.Addr().String()
	sp, err := spclient.NewSpclient(context.Background(), &librespot.NullLogger{}, server.Client(),
		func(context.Context) string { return addr },
		func(context.Context, bool) (string, error) { return "test-access-token", nil },
		"test-device-id", "")
	if err != nil {
		t.Fatalf("failed creating spclient: %v", err)
	}

	// Mirrors daemon.AppPlayer.narrationFor: resolve the clip's url, then
	// fetch and decode it.
	synthesizeIntro := func() librespot.AudioSource {
		ctx := context.Background()
		req := spclient.NewNarrationRequest("<speak>hello</speak>",
			narrationpb.ResolveRequest_VOICE1, narrationpb.ResolveRequest_SONANTIC_FAST, player.SampleRate)

		url, err := sp.NarrationUrl(ctx, req)
		if err != nil {
			t.Errorf("NarrationUrl failed: %v", err)
			return nil
		}

		src, err := player.NewNarrationSource(ctx, &librespot.NullLogger{}, server.Client(), url, 0, 0, 0)
		if err != nil {
			t.Errorf("NewNarrationSource failed: %v", err)
			return nil
		}
		return src
	}

	main, err := player.NewNarrationSource(context.Background(), &librespot.NullLogger{}, server.Client(),
		server.URL+"/narration.mp3", 0, 0, 0)
	if err != nil {
		t.Fatalf("failed building the stand-in main track: %v", err)
	}

	// Mirrors daemon.AppPlayer.narrate: synthesis on its own goroutine,
	// NewNarratedSource gets the channel rather than waiting itself.
	introCh := make(chan librespot.AudioSource, 1)
	go func() { introCh <- synthesizeIntro() }()

	buildStart := time.Now()
	narrated := player.NewNarratedSource(&librespot.NullLogger{}, true, introCh, main, false, nil)
	if buildElapsed := time.Since(buildStart); buildElapsed > 100*time.Millisecond {
		t.Fatalf("NewNarratedSource took %v to return, want near-instant regardless of synthesis: "+
			"it must not wait on the intro", buildElapsed)
	}

	// The first Read legitimately does wait, since there is real audio for
	// the intro and it has to come from somewhere - this is the ordinary,
	// now-relocated wait, moved off the daemon's command-reply path and onto
	// the audio output goroutine where a couple of seconds of buffering
	// delay is unremarkable.
	readStart := time.Now()
	buf := make([]float32, 4096)
	n, err := narrated.Read(buf)
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}
	if n == 0 {
		t.Fatal("read returned no samples")
	}
	if readElapsed := time.Since(readStart); readElapsed < synthesisDelay {
		t.Fatalf("Read returned after only %v, want at least the %v synthesis delay - "+
			"this test isn't actually exercising the slow path it's meant to", readElapsed, synthesisDelay)
	}

	if fulfillCalls != 1 {
		t.Fatalf("synthesis endpoint was called %d times, want exactly 1", fulfillCalls)
	}
}
