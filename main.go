// SPDX-FileCopyrightText: 2023 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

//go:build !js
// +build !js

// sfu-ws is a many-to-many websocket based SFU
package main

import (
	"encoding/json"
	"flag"
	"github.com/pion/webrtc/v4"
	"log"
	"net/http"
	"os"
	"sync"
	"text/template"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
)

// nolint
var (
	addr     = flag.String("addr", ":8080", "http service address")
	upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	indexTemplate = &template.Template{}

	// lock for peerConnections and trackLocals
	listLock        sync.RWMutex
	peerConnections []peerConnectionState
	trackLocals     map[string]*webrtc.TrackLocalStaticRTP
)

type websocketMessage struct {
	Event string `json:"event"`
	Data  string `json:"data"`
}

type peerConnectionState struct {
	peerConnection *webrtc.PeerConnection
	websocket      *threadSafeWriter
}

func main() {
	// Parse the flags passed to program
	flag.Parse()

	// Init other state
	log.SetFlags(0)
	trackLocals = map[string]*webrtc.TrackLocalStaticRTP{}

	// Read index.html from disk into memory, serve whenever anyone requests /
	indexHTML, err := os.ReadFile("index.html")
	if err != nil {
		panic(err)
	}
	indexTemplate = template.Must(template.New("").Parse(string(indexHTML)))

	// websocket handler
	http.HandleFunc("/websocket", websocketHandler)

	// index.html handler
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if err := indexTemplate.Execute(w, "ws://"+r.Host+"/websocket"); err != nil {
			log.Fatal(err)
		}
	})

	// request a keyframe every 3 seconds
	go func() {
		for range time.NewTicker(time.Second * 3).C {
			dispatchKeyFrame()
		}
	}()

	// start HTTP server
	log.Fatal(http.ListenAndServe(*addr, nil)) // nolint:gosec
}

// Add to list of tracks and fire renegotation for all PeerConnections
func addTrack(t *webrtc.TrackRemote) *webrtc.TrackLocalStaticRTP {
	listLock.Lock()
	defer func() {
		listLock.Unlock()
		signalPeerConnections()
	}()

	// Создает новый локальный трек, используя кодек и идентификатор входящего трека. Если возникла ошибка, программа завершает выполнение с помощью panic
	trackLocal, err := webrtc.NewTrackLocalStaticRTP(t.Codec().RTPCodecCapability, t.ID(), t.StreamID())
	if err != nil {
		panic(err)
	}

	// Добавляет локальный трек в карту trackLocals и возвращает его.
	trackLocals[t.ID()] = trackLocal
	return trackLocal
}

// блокирует доступ к trackLocals, удаляет трек из карты и вызывает signalPeerConnections().
func removeTrack(t *webrtc.TrackLocalStaticRTP) {
	listLock.Lock()
	defer func() {
		listLock.Unlock()
		signalPeerConnections()
	}()

	delete(trackLocals, t.ID())
}

// signalPeerConnections updates each PeerConnection so that it is getting all the expected media tracks
func signalPeerConnections() {

	//Блокирует доступ к списку peerConnections.
	//После завершения signalPeerConnections() функции, разблокирует общий ресурс и  отправляет запрос на ключевой кадр.
	listLock.Lock()
	defer func() {
		listLock.Unlock()
		dispatchKeyFrame()
	}()

	// Определяет вложенную функцию attemptSync, для синхронизации всех активных PeerConnections.
	attemptSync := func() (tryAgain bool) {
		for i := range peerConnections {

			//Если состояние соединения закрыто, удаляет его из списка.
			if peerConnections[i].peerConnection.ConnectionState() == webrtc.PeerConnectionStateClosed {
				peerConnections = append(peerConnections[:i], peerConnections[i+1:]...)
				return true // We modified the slice, start from the beginning
			}

			// Создает карту existingSenders для отслеживания отправителей и их треков.
			existingSenders := map[string]bool{}
			for _, sender := range peerConnections[i].peerConnection.GetSenders() {
				if sender.Track() == nil {
					continue
				}

				existingSenders[sender.Track().ID()] = true

				// Если для отправителя не существует соответствующего трека в trackLocals, он удаляет этот трек из PeerConnection.
				if _, ok := trackLocals[sender.Track().ID()]; !ok {
					if err := peerConnections[i].peerConnection.RemoveTrack(sender); err != nil {
						return true
					}
				}
			}

			// Проверяет получателей и добавляет их в existingSenders.
			for _, receiver := range peerConnections[i].peerConnection.GetReceivers() {
				if receiver.Track() == nil {
					continue
				}

				existingSenders[receiver.Track().ID()] = true
			}

			// Добавляет все треки, которые еще не отправляются PeerConnection.
			for trackID := range trackLocals {
				if _, ok := existingSenders[trackID]; !ok {
					if _, err := peerConnections[i].peerConnection.AddTrack(trackLocals[trackID]); err != nil {
						return true
					}
				}
			}

			// Создает SDP предложение (offer) для установления соединения и обрабатывает ошибку закрытием функции.
			offer, err := peerConnections[i].peerConnection.CreateOffer(nil)
			if err != nil {
				return true
			}

			// Устанавливает предложение как локальное описание и обрабатывает ошибку.
			if err = peerConnections[i].peerConnection.SetLocalDescription(offer); err != nil {
				return true
			}

			// Сериализует предложение в JSON.
			offerString, err := json.Marshal(offer)
			if err != nil {
				return true
			}

			// Отправляет предложение новому клиенту через WebSocket. Если происходит ошибка, возвращает true для повторной обработки.
			if err = peerConnections[i].websocket.WriteJSON(&websocketMessage{
				Event: "offer",
				Data:  string(offerString),
			}); err != nil {
				return true
			}
		}

		return
	}

	// Если не удалось синхронизировать после 25 попыток, функция запускает новую горутину, ждет 3 секунды и пытается снова. Это позволяет избежать блокировок.
	for syncAttempt := 0; ; syncAttempt++ {
		if syncAttempt == 25 {
			// Release the lock and attempt a sync in 3 seconds. We might be blocking a RemoveTrack or AddTrack
			go func() {
				time.Sleep(time.Second * 3)
				signalPeerConnections()
			}()
			return
		}

		if !attemptSync() {
			break
		}
	}
}

// Блокирует доступ к peerConnections, затем для каждого получателя в каждом соединении отправляет RTCP пакет с указанием потерянного ключевого кадра.
// Это позволяет сигнализировать о том, что клиентам требуется ключевой кадр (например, при присоединении нового клиента).
func dispatchKeyFrame() {
	listLock.Lock()
	defer listLock.Unlock()

	for i := range peerConnections {
		for _, receiver := range peerConnections[i].peerConnection.GetReceivers() {
			if receiver.Track() == nil {
				continue
			}

			_ = peerConnections[i].peerConnection.WriteRTCP([]rtcp.Packet{
				&rtcp.PictureLossIndication{
					MediaSSRC: uint32(receiver.Track().SSRC()),
				},
			})
		}
	}
}

// Обработчик для WebSocket соединений. Он обновляет HTTP-запрос до WebSocket. Если произойдет ошибка, она будет зафиксирована через log.Print.
// Создает threadSafeWriter, чтобы обеспечить потокобезопасный доступ к WebSocket.
func websocketHandler(w http.ResponseWriter, r *http.Request) {
	// Upgrade HTTP request to Websocket
	unsafeConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Print("upgrade:", err)
		return
	}

	c := &threadSafeWriter{unsafeConn, sync.Mutex{}}

	// defer закрывает WebSocket соединение, когда функция завершится.
	defer c.Close() //nolint

	// Создание нового PeerConnection
	peerConnection, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		log.Print(err)
		return
	}

	// defer закрывает PeerConnection, когда функция завершится.
	defer peerConnection.Close() //nolint

	// Добавляет один аудиотрек и один видеотрек для получения, создавая трансиверы с направлением "recvonly".
	for _, typ := range []webrtc.RTPCodecType{webrtc.RTPCodecTypeVideo, webrtc.RTPCodecTypeAudio} {
		if _, err := peerConnection.AddTransceiverFromKind(typ, webrtc.RTPTransceiverInit{
			Direction: webrtc.RTPTransceiverDirectionRecvonly,
		}); err != nil {
			log.Print(err)
			return
		}
	}

	// Добавление PeerConnection в глобальный список
	// Блокирует доступ к списку peerConnections, добавляет новое соединение в глобальный список и освобождает блокировку.
	listLock.Lock()
	peerConnections = append(peerConnections, peerConnectionState{peerConnection, c})
	listLock.Unlock()

	// Обработка ICE кандидатов
	// Trickle ICE. Emit server candidate to client
	peerConnection.OnICECandidate(func(i *webrtc.ICECandidate) {
		// Присваивает функцию обратного вызова для обработки ICE кандидатур. Если кандидат равен nil, выполнение прерывается.
		if i == nil {
			return
		}
		// If you are serializing a candidate make sure to use ToJSON
		// Using Marshal will result in errors around `sdpMid`
		candidateString, err := json.Marshal(i.ToJSON())
		if err != nil {
			log.Println(err)
			return
		}

		// Отправляет ICE кандидата клиенту через WebSocket. Если ошибка, она будет записана в лог.
		if writeErr := c.WriteJSON(&websocketMessage{
			Event: "candidate",
			Data:  string(candidateString),
		}); writeErr != nil {
			log.Println(writeErr)
		}
	})

	// Обработка изменений состояния соединения
	// Устанавливает обработчик для отслеживания изменений состояния соединения.
	// Если соединение закрылось или потерпело неудачу, оно будет закрыто и удалено из списка.
	peerConnection.OnConnectionStateChange(func(p webrtc.PeerConnectionState) {
		switch p {
		case webrtc.PeerConnectionStateFailed:
			if err := peerConnection.Close(); err != nil {
				log.Print(err)
			}
		case webrtc.PeerConnectionStateClosed:
			signalPeerConnections()
		default:
		}
	})

	// Устанавливает обработчик на входящие треки. При получении трека вызывается функция addTrack для добавления его в глобальный список.
	// Создается буфер для чтения данных RTP и объект RTP-пакета.
	peerConnection.OnTrack(func(t *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		// Create a track to fan out our incoming video to all peers
		trackLocal := addTrack(t)
		defer removeTrack(trackLocal)

		buf := make([]byte, 1500)
		rtpPkt := &rtp.Packet{}

		// Запускается бесконечный цикл для чтения RTP-пакетов из трека. Если чтение завершится ошибкой, выполнение прекратится.
		// После чтения пакет десериализуется и отправляется всем локальным трекам.
		for {
			i, _, err := t.Read(buf)
			if err != nil {
				return
			}

			if err = rtpPkt.Unmarshal(buf[:i]); err != nil {
				log.Println(err)
				return
			}

			rtpPkt.Extension = false
			rtpPkt.Extensions = nil

			if err = trackLocal.WriteRTP(rtpPkt); err != nil {
				return
			}
		}
	})

	// Вызывает функцию signalPeerConnections, чтобы уведомить всех участников о новом подключении.
	signalPeerConnections()

	// апускает бесконечный цикл для чтения сообщений от клиента. Если возникает ошибка, она выводится в лог.
	// Также происходит десериализация входящих сообщений в структуру websocketMessage.
	message := &websocketMessage{}
	for {
		_, raw, err := c.ReadMessage()
		if err != nil {
			log.Println(err)
			return
		} else if err := json.Unmarshal(raw, &message); err != nil {
			log.Println(err)
			return
		}

		// В зависимости от типа события (например, candidate или answer) обрабатываются соответствующие данные.
		// Если есть ошибки, они записываются в лог.
		switch message.Event {
		case "candidate":
			candidate := webrtc.ICECandidateInit{}
			if err := json.Unmarshal([]byte(message.Data), &candidate); err != nil {
				log.Println(err)
				return
			}

			if err := peerConnection.AddICECandidate(candidate); err != nil {
				log.Println(err)
				return
			}
		case "answer":
			answer := webrtc.SessionDescription{}
			if err := json.Unmarshal([]byte(message.Data), &answer); err != nil {
				log.Println(err)
				return
			}

			if err := peerConnection.SetRemoteDescription(answer); err != nil {
				log.Println(err)
				return
			}
		}
	}
}

// Определяет структуру, которая включает WebSocket соединение и мьютекс для обеспечения потокобезопасности.
type threadSafeWriter struct {
	*websocket.Conn
	sync.Mutex
}

// WriteJSON Метод блокирует доступ при записи JSON-данных, что позволяет избежать конфликтов при использовании из нескольких горутин.
func (t *threadSafeWriter) WriteJSON(v interface{}) error {
	t.Lock()
	defer t.Unlock()

	return t.Conn.WriteJSON(v)
}
