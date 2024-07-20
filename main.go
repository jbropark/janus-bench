// SPDX-FileCopyrightText: 2023 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package main

import (
	"fmt"
	"time"
	"flag"

	janus "github.com/notedit/janus-go"
	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/stats"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/pion/webrtc/v4/pkg/media/ivfwriter"
	"github.com/pion/webrtc/v4/pkg/media/oggwriter"
)

func saveToDisk(writer media.Writer, track *webrtc.TrackRemote) {
	defer func() {
		if err := writer.Close(); err != nil {
			panic(err)
		}
	}()

	for {
		packet, _, err := track.ReadRTP()
		if err != nil {
			panic(err)
		}

		if err := writer.WriteRTP(packet); err != nil {
			panic(err)
		}
	}
}

func saveOpusToDisk(track *webrtc.TrackRemote, path string) {
	if len(path) == 0 {
		fmt.Println("do not save (path not given)")
		return
	}

	codec := track.Codec()
	fmt.Printf("save to '%s'\n", path)
	writer, err := oggwriter.New(path, codec.ClockRate, codec.Channels)
	if err != nil {
		panic(err)
	}
	saveToDisk(writer, track)
}

func saveVP8ToDisk(track *webrtc.TrackRemote, path string) {
	if len(path) == 0 {
		fmt.Println("do not save (path not given)")
		return
	}

	fmt.Printf("save to '%s'\n", path)
	writer, err := ivfwriter.New(path)
	if err != nil {
		panic(err)
	}
	saveToDisk(writer, track)
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

func saveBench(statsGetter stats.Getter, track *webrtc.TrackRemote, path string, interval int) {
	for {
		stats := statsGetter.Get(uint32(track.SSRC()))

		fmt.Printf("Stats for: %s\n", track.Codec().MimeType)
		fmt.Println(stats.InboundRTPStreamStats)

		time.Sleep(time.Second * time.Duration(interval))
	}
}

func main() {
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

	engine := &webrtc.MediaEngine{}

	if err := engine.RegisterDefaultCodecs(); err != nil {
		panic(err)
	}

	registry := &interceptor.Registry{}

	statsInterceptorFactory, err := stats.NewInterceptor()
	if err != nil {
		panic(err)
	}

	var statsGetter stats.Getter
	statsInterceptorFactory.OnNewPeerConnection(func(_ string, getter stats.Getter) {
		statsGetter = getter
	})
	registry.Add(statsInterceptorFactory)

	if err = webrtc.RegisterDefaultInterceptors(engine, registry); err != nil {
		panic(err)
	}

	api := webrtc.NewAPI(webrtc.WithMediaEngine(engine), webrtc.WithInterceptorRegistry(registry))

	// Janus
	gateway, err := janus.Connect(url)
	if err != nil {
		panic(err)
	}

	// Create session
	session, err := gateway.Create()
	if err != nil {
		panic(err)
	}

	// Create handle
	handle, err := session.Attach("janus.plugin.streaming")
	if err != nil {
		panic(err)
	}

	go watchHandle(handle)

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
			go saveBench(statsGetter, track, *argBenchPath, *argBenchInterval)

			codec := track.Codec()
			fmt.Printf("[%d] Got %s(%s, rtx=%t) track: ", handle.ID, track.ID(), codec.MimeType, track.HasRTX())
			if codec.MimeType == "audio/opus" {
				saveOpusToDisk(track, *argAudioPath)
			} else if codec.MimeType == "video/VP8" {
				saveVP8ToDisk(track, *argVideoPath)
			} else {
				fmt.Println("unknown codec")
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

	// health check
	for {
		if _, err = session.KeepAlive(); err != nil {
			panic(err)
		}

		time.Sleep(5 * time.Second)
	}
}
