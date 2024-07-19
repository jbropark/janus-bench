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

func saveToDisk(i media.Writer, track *webrtc.TrackRemote) {
	defer func() {
		if err := i.Close(); err != nil {
			panic(err)
		}
	}()

	for {
		packet, _, err := track.ReadRTP()
		if err != nil {
			panic(err)
		}

		if err := i.WriteRTP(packet); err != nil {
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
			fmt.Print("SlowLinkMsg type ", handle.ID)
		case *janus.MediaMsg:
			fmt.Print("MediaEvent type", msg.Type, " receiving ", msg.Receiving)
		case *janus.WebRTCUpMsg:
			fmt.Print("WebRTCUp type ", handle.ID)
		case *janus.HangupMsg:
			fmt.Print("HangupEvent type ", handle.ID)
		case *janus.EventMsg:
			fmt.Printf("EventMsg %+v", msg.Plugindata.Data)
		}
	}
}

func main() {
	// Everything below is the Pion WebRTC API! Thanks for using it ❤️.
	m := &webrtc.MediaEngine{}

	if err := m.RegisterDefaultCodecs(); err != nil {
		panic(err)
	}

	registry := &interceptor.Registry{}

	statsInterceptorFactory, err := stats.NewInterceptor()
	if err != nil {
		panic(err)
	}

	var statsGetter stats.Getter
	statsInterceptorFactory.OnNewPeerConnection(func(_ string, g stats.Getter) {
		statsGetter = g
	})
	registry.Add(statsInterceptorFactory)

	if err = webrtc.RegisterDefaultInterceptors(m, registry); err != nil {
		panic(err)
	}

	api := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(registry))

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
			fmt.Printf("Connection State has changed %s \n", connectionState.String())
		})

		peerConnection.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
			go func() {
				for {
					stats := statsGetter.Get(uint32(track.SSRC()))

					fmt.Printf("Stats for: %s\n", track.Codec().MimeType)
					fmt.Println(stats.InboundRTPStreamStats)

					time.Sleep(time.Second * 5)
				}
			}()
			codec := track.Codec()
			if codec.MimeType == "audio/opus" {
				fmt.Println("Got Opus track, saving to disk as output.ogg")
				i, oggNewErr := oggwriter.New("output.ogg", codec.ClockRate, codec.Channels)
				if oggNewErr != nil {
					panic(oggNewErr)
				}
				saveToDisk(i, track)
			} else if codec.MimeType == "video/VP8" {
				fmt.Println("Got VP8 track, saving to disk as output.ivf")
				i, ivfNewErr := ivfwriter.New("output.ivf")
				if ivfNewErr != nil {
					panic(ivfNewErr)
				}
				saveToDisk(i, track)
			}
			fmt.Printf("RTX: %t \n", track.HasRTX())
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
	for {
		if _, err = session.KeepAlive(); err != nil {
			panic(err)
		}

		time.Sleep(5 * time.Second)
	}
}