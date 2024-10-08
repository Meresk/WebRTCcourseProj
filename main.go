package main

import (
	"fmt"
	"log"
	"net/http"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v3"
)

var upgrader = websocket.Upgrader{} // use default options for upgrading

func main() {
	http.HandleFunc("/", serveHome)
	http.HandleFunc("/ws", handleWebSocket)

	fmt.Println("Starting server on :8080")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		log.Fatal(err)
	}
}

func serveHome(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "index.html")
}

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("Error upgrading:", err)
		return
	}
	defer conn.Close()

	peerConnection, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		log.Println("Error creating PeerConnection:", err)
		return
	}
	defer peerConnection.Close() // Ensures the connection is closed when done

	// Set up ICE candidate handling
	peerConnection.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate != nil {
			if err := conn.WriteJSON(candidate.ToJSON()); err != nil {
				log.Println("Error sending candidate:", err)
			}
		}
	})

	// Handle incoming track
	peerConnection.OnTrack(func(track *webrtc.TrackRemote, rtpReceiver *webrtc.RTPReceiver) {
		//// For demonstration purposes, just create a new RTP sender
		//_, err := peerConnection.AddTrack(track)

		log.Printf("Received track: ID=%s, Kind=%s", track.ID(), track.Kind())
		if err != nil {
			log.Println("Error adding track:", err)
		}
	})

	// Read messages from the websocket
	for {
		var msg struct {
			Sdp  string `json:"sdp"`
			Type string `json:"type"`
		}
		err := conn.ReadJSON(&msg)
		if err != nil {
			log.Println("Error reading json:", err)
			break
		}

		switch msg.Type {
		case "offer":
			offer := webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: msg.Sdp}
			err = peerConnection.SetRemoteDescription(offer)
			if err != nil {
				log.Println("Error setting remote description:", err)
				return
			}

			// Create an answer
			answer, err := peerConnection.CreateAnswer(nil)
			if err != nil {
				log.Println("Error creating answer:", err)
				return
			}

			err = peerConnection.SetLocalDescription(answer)
			if err != nil {
				log.Println("Error setting local description:", err)
				return
			}

			if err := conn.WriteJSON(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer.SDP}); err != nil {
				log.Println("Error sending answer:", err)
			}
		}
	}
}
