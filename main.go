// SPDX-FileCopyrightText: 2023 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package main

import (
	"os"
	"fmt"
	"time"
	"flag"
	"encoding/csv"
	"context"
	"sync"
	"os/signal"
	"syscall"

	janus "github.com/notedit/janus-go"
	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/stats"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media"
	"github.com/pion/webrtc/v3/pkg/media/ivfwriter"
	"github.com/pion/webrtc/v3/pkg/media/oggwriter"
)


var wg sync.WaitGroup


func readToDiscard(track *webrtc.TrackRemote) {
	for {
		_, _, err := track.ReadRTP()
		if err != nil {
			panic(err)
		}
	}
}

func saveToDisk(ctx context.Context, writer media.Writer, track *webrtc.TrackRemote) {
	wg.Add(1)
	defer wg.Done()

	defer func() {
		fmt.Printf("Try to close disk\n")
		if err := writer.Close(); err != nil {
			panic(err)
		}
	}()

	for {
		select {
		case <-ctx.Done():
			fmt.Println("Stop save to disk")
			return
		default:
			packet, _, err := track.ReadRTP()
			if err != nil {
				panic(err)
			}

			if err := writer.WriteRTP(packet); err != nil {
				panic(err)
			}
		}
	}
}

func saveOpusToDisk(ctx context.Context, track *webrtc.TrackRemote, path string) {
	codec := track.Codec()
	mime := codec.MimeType
	if len(path) == 0 {
		fmt.Printf("knwon codec %s: do not save (path not given)\n", mime)
		readToDiscard(track)
		return
	}

	fmt.Printf("known codec %s: save to '%s'\n", mime, path)
	writer, err := oggwriter.New(path, codec.ClockRate, codec.Channels)
	if err != nil {
		panic(err)
	}
	saveToDisk(ctx, writer, track)
}

func saveVP8ToDisk(ctx context.Context, track *webrtc.TrackRemote, path string) {
	codec := track.Codec()
	mime := codec.MimeType
	if len(path) == 0 {
		fmt.Printf("known codec %s: do not save (path not given)\n", mime)
		readToDiscard(track)
		return
	}

	fmt.Printf("known codec $s: save to '%s'\n", mime, path)
	writer, err := ivfwriter.New(path)
	if err != nil {
		panic(err)
	}
	saveToDisk(ctx, writer, track)
}

func watchHandle(handle *janus.Handle) {
	// wait for event
	for {
		msg := <-handle.Events
		switch msg := msg.(type) {
		case *janus.SlowLinkMsg:
			fmt.Printf("[%d] Got SlowLinkMsg\n", handle.ID)
		case *janus.MediaMsg:
			fmt.Printf("[%d] Got MediaMsg\n", handle.ID)
		case *janus.WebRTCUpMsg:
			fmt.Printf("[%d] Got WebRTCUpMsg\n", handle.ID)
		case *janus.HangupMsg:
			fmt.Printf("[%d] Got HangupMsg\n", handle.ID)
		case *janus.EventMsg:
			fmt.Printf("[%d] Got EventMsg %+v\n", handle.ID, msg.Plugindata.Data)
		}
	}
}

var statsGetter stats.Getter

var BENCH_COLUMN_NAMES = []string{"asdf", "asdf"}

func saveBench(ctx context.Context, track *webrtc.TrackRemote, writer *csv.Writer, interval int) {
	wg.Add(1)
	defer wg.Done()

	ticker := time.NewTicker(time.Duration(interval) * time.Second)
	defer ticker.Stop()

	trackID := track.ID()
	ssrc := uint32(track.SSRC())
	for {
		select {
		case <-ctx.Done():
			fmt.Println("stop save bench")
			return
		case <-ticker.C:
			stats := statsGetter.Get(ssrc)
			inbound := stats.InboundRTPStreamStats

			/*
			inbound.PacketsReceived
			inbound.PacketsLost
			inbound.Jitter
			inbound.LastPacketReceivedTimestamp
			inbound.HeaderBytesReceived
			inbound.BytesReceived
			inbound.NACKCount
			*/

			// fmt.Println(inbound)
			//fmt.Println(trackID, inbound)
			_ = inbound
			_ = trackID


		}
	}
}

func keepAlive(session *janus.Session) {
	// health check
	for {
		if _, err := session.KeepAlive(); err != nil {
			panic(err)
		}
		time.Sleep(5 * time.Second)
	}
}

func createWebRTCAPI(engine *webrtc.MediaEngine, registry *interceptor.Registry) *webrtc.API {
	if err := engine.RegisterDefaultCodecs(); err != nil {
		panic(err)
	}

	statsInterceptorFactory, err := stats.NewInterceptor()
	if err != nil {
		panic(err)
	}

	statsInterceptorFactory.OnNewPeerConnection(func(_ string, getter stats.Getter) {
		statsGetter = getter
	})
	registry.Add(statsInterceptorFactory)

	if err = webrtc.RegisterDefaultInterceptors(engine, registry); err != nil {
		panic(err)
	}

	return webrtc.NewAPI(webrtc.WithMediaEngine(engine), webrtc.WithInterceptorRegistry(registry))
}

func createNewStreamingHandle(gateway *janus.Gateway) (*janus.Session, *janus.Handle) {
	session, err := gateway.Create()
	if err != nil {
		panic(err)
	}
	go keepAlive(session)

	// Create handle
	handle, err := session.Attach("janus.plugin.streaming")
	if err != nil {
		panic(err)
	}
	go watchHandle(handle)

	return session, handle
}

func main() {
	// parse arguments
	argHost := flag.String("host", "172.20.0.2", "janus ip")
	argPort := flag.Int("port", 8188, "janus websocket port")
	argRoomID := flag.Int("room", 1, "target room id")
	argVideoPath := flag.String("video", "", "output video path")
	argAudioPath := flag.String("audio", "", "output audio path")
	argBenchPath := flag.String("o", "bench.csv", "output bench path")
	argBenchInterval := flag.Int("interval", 5, "bench sample interval")

	flag.Parse()

	url := fmt.Sprintf("ws://%s:%d/", *argHost, *argPort)
	fmt.Printf("URL: %s\n", url)

	// graceful stop setup
	cancelChan := make(chan os.Signal)
	signal.Notify(cancelChan, syscall.SIGINT, syscall.SIGTERM)
	ctx, cancel := context.WithCancel(context.Background())

	// WebRTC
	var engine webrtc.MediaEngine
	var registry interceptor.Registry
	api := createWebRTCAPI(&engine, &registry)

	// Janus
	gateway, err := janus.Connect(url)
	if err != nil {
		panic(err)
	}

	// Create session
	_, handle := createNewStreamingHandle(gateway)

	// init bench csv
	benchFile, err := os.Create(*argBenchPath)
	if err != nil {
		panic(err)
	}
	defer benchFile.Close()

	benchWriter := csv.NewWriter(benchFile)
	defer benchWriter.Flush()

	benchWriter.Write(BENCH_COLUMN_NAMES)

	// Watch the second stream
	msg, err := handle.Message(map[string]interface{}{
		"request": "watch",
		"id":      *argRoomID,
	}, nil)
	if err != nil {
		panic(err)
	}

	if msg.Jsep != nil {
		sdpVal, ok := msg.Jsep["sdp"].(string)
		if !ok {
			panic("failed to cast")
		}

		offer := webrtc.SessionDescription{
			Type: webrtc.SDPTypeOffer,
			SDP:  sdpVal,
		}

		// Create a new RTCPeerConnection
		var peerConnection *webrtc.PeerConnection
		peerConnection, err = api.NewPeerConnection(webrtc.Configuration{
			ICEServers: []webrtc.ICEServer{
				{
					URLs: []string{"stun:stun.l.google.com:19302"},
				},
			},
			SDPSemantics: webrtc.SDPSemanticsUnifiedPlanWithFallback,
		})
		if err != nil {
			panic(err)
		}

		// We must offer to send media for Janus to send anything
		if _, err = peerConnection.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{
			Direction: webrtc.RTPTransceiverDirectionRecvonly,
		}); err != nil {
			panic(err)
		} else if _, err = peerConnection.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{
			Direction: webrtc.RTPTransceiverDirectionRecvonly,
		}); err != nil {
			panic(err)
		}

		peerConnection.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
			fmt.Printf("[%d] Connection State has changed to %s \n", handle.ID, connectionState.String())
		})

		peerConnection.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
			go saveBench(ctx, track, benchWriter, *argBenchInterval)

			codec := track.Codec()
			fmt.Printf("[%d] Got TrackRemote %s(%s, rtx=%t)\n", handle.ID, track.ID(), codec.MimeType, track.HasRTX())
			if codec.MimeType == "audio/opus" {
				saveOpusToDisk(ctx, track, *argAudioPath)
			} else if codec.MimeType == "video/VP8" {
				saveVP8ToDisk(ctx, track, *argVideoPath)
			} else {
				fmt.Printf("Unknown codec: %s\n", codec.MimeType)
			}
		})

		if err = peerConnection.SetRemoteDescription(offer); err != nil {
			panic(err)
		}

		// Create channel that is blocked until ICE Gathering is complete
		gatherComplete := webrtc.GatheringCompletePromise(peerConnection)

		answer, answerErr := peerConnection.CreateAnswer(nil)
		if answerErr != nil {
			panic(answerErr)
		}

		if err = peerConnection.SetLocalDescription(answer); err != nil {
			panic(err)
		}

		// Block until ICE Gathering is complete, disabling trickle ICE
		// we do this because we only can exchange one signaling message
		// in a production application you should exchange ICE Candidates via OnICECandidate
		<-gatherComplete

		// now we start
		_, err = handle.Message(map[string]interface{}{
			"request": "start",
		}, map[string]interface{}{
			"type":    "answer",
			"sdp":     peerConnection.LocalDescription().SDP,
			"trickle": false,
		})
		if err != nil {
			panic(err)
		}
	}

	// wait signal
	sig := <-cancelChan
	fmt.Printf("Got %v: clean up...\n", sig)
	cancel()
	wg.Wait()
	fmt.Printf("Done!\n")
}
