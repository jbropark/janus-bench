// SPDX-FileCopyrightText: 2023 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package main

import (
	"fmt"
	"time"

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

func checkStats(statsGetter *stats.Getter, track *webrtc.TrackRemote) {
	for {
		stats := statsGetter.Get(uint32(track.SSRC()))

		fmt.Printf("Stats for: %s\n", track.Codec().MimeType)
		fmt.Println(stats.InboundRTPStreamStats)

		time.Sleep(time.Second * 5)
	}
}

func main() {
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
	gateway, err := janus.Connect("ws://172.20.0.2:8188/")
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

	// Get streaming list
	_, err = handle.Request(map[string]interface{}{
		"request": "list",
	})
	if err != nil {
		panic(err)
	}

	// Watch the second stream
	msg, err := handle.Message(map[string]interface{}{
		"request": "watch",
		"id":      1,
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
			go checkStats(statsGetter, track)

			codec := track.Codec()
			if codec.MimeType == "audio/opus" {
				fmt.Println("Got Opus track, saving to disk as output.ogg")
				writer, oggNewErr := oggwriter.New("output.ogg", codec.ClockRate, codec.Channels)
				if oggNewErr != nil {
					panic(oggNewErr)
				}
				saveToDisk(writer, track)
			} else if codec.MimeType == "video/VP8" {
				fmt.Println("Got VP8 track, saving to disk as output.ivf")
				writer, ivfNewErr := ivfwriter.New("output.ivf")
				if ivfNewErr != nil {
					panic(ivfNewErr)
				}
				saveToDisk(writer, track)
			} else {
				fmt.Println("Got track with unknown codec " + codec.MimeType)
			}
			fmt.Printf("[%d] ID : %s \n", handle.ID, track.ID())
			fmt.Printf("[%d] RTX: %t \n", handle.ID, track.HasRTX())
			fmt.Printf("[%d] RID: %s \n", handle.ID, track.RID())
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
